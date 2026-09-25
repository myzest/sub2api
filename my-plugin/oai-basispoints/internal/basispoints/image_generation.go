package basispoints

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"slices"
	"strings"

	pluginv1 "local.sub2api/oai-basispoints/internal/pluginapi"
)

// Wire contract: excel-codex-bridge 66c41df, image_generation.py. Codex's
// client-side image_gen.imagegen executor calls the independent Images API;
// this does not enable hosted image_generation tools on /responses.
const imageGenerationModel = "gpt-image-2"

func imageOperation(path string) string {
	if strings.Contains(path, "//") {
		return ""
	}
	for _, operation := range []string{"generations", "edits"} {
		if strings.HasSuffix(path, "/images/"+operation) {
			return operation
		}
	}
	return ""
}

func imageGenerationFields(source object) (object, error) {
	prompt, ok := source["prompt"].(string)
	if !ok || strings.TrimSpace(prompt) == "" {
		return nil, errors.New("生图需要非空字符串 prompt")
	}
	if model := source["model"]; model != nil && model != imageGenerationModel {
		return nil, errors.New("BPS 生图仅适配 gpt-image-2；不会自动替换其他模型")
	}
	if format := source["output_format"]; format != nil && format != "png" {
		return nil, errors.New("BPS 生图仅适配 PNG 输出")
	}
	if source["background"] == "transparent" {
		return nil, errors.New("BPS 生图不支持透明背景；请改用 auto 或 opaque")
	}
	// The reference uses non-streaming JSON and does not implement masks.
	// Reject these explicit requests instead of silently losing their meaning.
	if stream := source["stream"]; stream != nil && stream != false {
		return nil, errors.New("BPS 生图仅适配非流式 JSON；不支持 stream:true")
	}
	if source["mask"] != nil {
		return nil, errors.New("BPS 改图尚未适配 mask；请发送无需蒙版的改图请求")
	}
	fields := object{"model": imageGenerationModel, "prompt": prompt, "output_format": "png"}
	for _, choice := range []struct {
		key     string
		allowed []string
	}{
		{"background", []string{"auto", "opaque"}},
		{"quality", []string{"auto", "low", "medium", "high"}},
		{"size", []string{"auto", "1024x1024", "1536x1024", "1024x1536", "1280x720"}},
	} {
		value := "auto"
		if raw := source[choice.key]; raw != nil {
			var ok bool
			value, ok = raw.(string)
			if !ok || !slices.Contains(choice.allowed, value) {
				return nil, fmt.Errorf("BPS 生图 %s 仅支持 %s", choice.key, strings.Join(choice.allowed, "/"))
			}
		}
		fields[choice.key] = value
	}
	if raw := source["n"]; raw != nil {
		n, ok := raw.(json.Number)
		count, err := n.Int64()
		if !ok || err != nil || count < 1 || count > 3 {
			return nil, errors.New("BPS 生图 n 必须为 1–3 的整数")
		}
		fields["n"] = n
	}
	return fields, nil
}

func imageGenerationBody(source object, operation string, d *requestDiagnostic) ([]byte, string, error) {
	fields, err := imageGenerationFields(source)
	if err != nil {
		return nil, "", err
	}
	if operation == "generations" {
		return encoded(fields), "application/json", nil
	}
	pictures, ok := source["images"].([]any)
	if !ok || len(pictures) == 0 {
		return nil, "", errors.New("BPS 改图需要 images 数组，图片必须为内嵌 data URL")
	}
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	for _, key := range []string{"background", "model", "output_format", "prompt", "quality", "size", "n"} {
		if value, exists := fields[key]; exists {
			if err := w.WriteField(key, fmt.Sprint(value)); err != nil {
				return nil, "", err
			}
		}
	}
	name := "image"
	if len(pictures) > 1 {
		name = "image[]"
	}
	for index, raw := range pictures {
		picture, _ := raw.(object)
		value := str(picture, "image_url")
		header, _, _ := strings.Cut(value, ",")
		if !strings.HasPrefix(value, "data:") || !strings.HasSuffix(strings.ToLower(header), ";base64") {
			return nil, "", fmt.Errorf("images[%d].image_url 必须为 base64 data URL；不抓取远程图片或 file_id", index)
		}
		mediaType, pixels, err := inlineImageBytes(value)
		if err != nil {
			return nil, "", fmt.Errorf("images[%d].image_url 无法解码", index)
		}
		// Reuse the explicit MIME mapping used by the proven input-image path.
		mediaType, extension, supported := attachmentImageSpec(mediaType)
		if !supported {
			return nil, "", unsupportedImageError()
		}
		filename := fmt.Sprintf("picture-%d%s", index+1, extension)
		partHeaders := textproto.MIMEHeader{}
		partHeaders.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{"name": name, "filename": filename}))
		partHeaders.Set("Content-Type", mediaType)
		part, err := w.CreatePart(partHeaders)
		if err != nil {
			return nil, "", err
		}
		if _, err := part.Write(pixels); err != nil {
			return nil, "", err
		}
		if len(d.UpstreamImages) < 16 {
			d.UpstreamImages = append(d.UpstreamImages, imageDiagnostic{Path: fmt.Sprintf("%s[%d]", name, index), Type: "image", Source: "multipart", Detail: "absent", MIME: mediaType, EncodedBytes: len(pixels)})
		}
	}
	if err := w.Close(); err != nil {
		return nil, "", err
	}
	if body.Len() > maxBody {
		return nil, "", errors.New("转换后的 BPS 改图请求超过 8 MiB")
	}
	return body.Bytes(), w.FormDataContentType(), nil
}

func (s *Server) forwardImages(ctx context.Context, w *forwardWriter, start *pluginv1.ForwardRequestStart, raw []byte) error {
	d := w.diagnostic
	d.RequestBytes, d.Stage = len(raw), "prepare"
	source, err := decodeObject(raw)
	if err != nil {
		return w.reject(400, "bps_image_request", err.Error())
	}
	if model := str(source, "model"); modelPattern.MatchString(model) {
		d.Model = model
	}
	d.Images, d.InputImages = summarizeImages(source)
	body, contentType, err := imageGenerationBody(source, d.ImageOperation, d)
	if err != nil {
		return w.reject(400, "bps_image_request", err.Error())
	}
	d.Model, d.UpstreamRequestBytes, d.Stage = imageGenerationModel, len(body), "identity"
	headers, err := bpsHeaders(headersFromProto(start.Headers))
	if err != nil {
		return w.reject(400, "bps_identity", err.Error())
	}
	headers.Set("Accept", "application/json")
	headers.Set("Content-Type", contentType)
	// The actor marker only enables Codex's local tool; bpsHeaders builds a
	// fresh header set and never forwards that marker as authorization.
	endpoint, err := url.Parse(s.responsesURL)
	if err != nil {
		return w.fail("invalid_url", "BPS 地址无效", false)
	}
	endpoint.Path = strings.TrimSuffix(endpoint.Path, "/responses") + "/images/" + d.ImageOperation
	endpoint.RawPath, endpoint.RawQuery, endpoint.Fragment = "", "", ""
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return w.fail("invalid_url", "BPS 生图地址无效", false)
	}
	request.Header = headers
	transport, err := s.pool.get(start.ProxyUrl)
	if err != nil {
		return w.fail("invalid_proxy", err.Error(), false)
	}
	d.Stage, d.UpstreamStarted = "connect", true
	response, err := transport.RoundTrip(request)
	if err != nil {
		d.ErrorSource = "upstream_transport"
		if ctx.Err() != nil {
			d.ErrorSource = "request_timeout_or_canceled"
		}
		return w.fail("bps_image_transport", networkError(ctx, err), true)
	}
	defer response.Body.Close()
	d.Stage, d.HTTPStatus = "http", response.StatusCode
	d.ContentType, _, _ = mime.ParseMediaType(response.Header.Get("Content-Type"))
	d.RequestID = redactProbeText(response.Header.Get("X-Request-Id"), headers, "")
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		d.ErrorSource = "upstream_http"
		d.UpstreamError, d.UpstreamErrorState = readUpstreamDiagnostic(response.Body, headers, raw, source)
		message := fmt.Sprintf("BPS 图片接口返回 HTTP %d；请查看诊断中的脱敏上游原因", response.StatusCode)
		return w.json(response.StatusCode, object{"error": object{"type": "bps_upstream_error", "code": fmt.Sprintf("bps_image_http_%d", response.StatusCode), "message": message}}, responseHeaders(response.Header))
	}
	d.Stage = "response"
	data, err := io.ReadAll(io.LimitReader(response.Body, maxResponse+1))
	if err != nil || len(data) > maxResponse {
		d.ErrorSource = "response_conversion"
		return w.fail("bps_image_response", "BPS 图片响应读取失败或超过 32 MiB", true)
	}
	payload, err := decodeObject(data)
	if err != nil {
		d.ErrorSource = "response_conversion"
		return w.fail("bps_image_response", "BPS 图片接口未返回有效 JSON 对象", true)
	}
	pictures, _ := payload["data"].([]any)
	for _, raw := range pictures {
		picture, _ := raw.(object)
		if strings.TrimSpace(str(picture, "b64_json")) != "" {
			d.GeneratedImages++
		}
	}
	d.Stage, d.Terminal = "completed", "images.completed"
	if payload["error"] != nil {
		d.Stage, d.Terminal, d.ErrorSource = "response", "images.failed", "upstream_response"
		d.Error = "bps_image_response: HTTP 200 的 Images JSON 包含 error；宿主将按错误处理"
		d.UpstreamError, d.UpstreamErrorState = readUpstreamDiagnostic(bytes.NewReader(data), headers, raw, source)
	} else if d.GeneratedImages == 0 {
		// The host's parseCodexDirectImagesResponse only accepts b64_json.
		// A URL-only/empty JSON cannot be marked completed in diagnostics.
		d.Stage, d.Terminal, d.ErrorSource = "response", "images.incomplete", "response_conversion"
		d.Error = "bps_image_response: Images JSON 没有宿主可接收的 b64_json 图片条目"
	}
	// Preserve the Images API payload, including usage and b64_json, for the
	// host's existing image accounting and the client's image file handling.
	return w.json(response.StatusCode, payload, responseHeaders(response.Header))
}
