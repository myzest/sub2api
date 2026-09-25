package basispoints

import (
	"bytes"
	"container/list"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
	"sync"
)

// Wire behavior follows cpa-plugin-oai-basispoints f4a2563 attachments.go:
// POST a multipart file, then replace the data URL with openai_file_id.
type attachmentReport struct {
	Uploaded        int                         `json:"uploaded"`
	Reused          int                         `json:"reused"`
	Attempts        int                         `json:"attempts"`
	Images          []attachmentImageDiagnostic `json:"images,omitempty"`
	HTTPStatus      int                         `json:"http_status,omitempty"`
	ContentType     string                      `json:"content_type,omitempty"`
	RequestID       string                      `json:"request_id,omitempty"`
	Diagnostic      string                      `json:"diagnostic,omitempty"`
	DiagnosticState string                      `json:"diagnostic_state,omitempty"`
}

// Only generated filenames and protocol metadata, never original filenames,
// file IDs, credentials or image content. Bounded to 16 entries per request.
type attachmentImageDiagnostic struct {
	Path       string `json:"path"`
	Filename   string `json:"filename"`
	MIME       string `json:"mime"`
	Bytes      int    `json:"bytes"`
	State      string `json:"state"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

// The observed BPS validation lists .jpeg/.jpg/.png/.gif/.webp. Go's MIME
// database can prefer .jfif or .jpe for JPEG, so do not use its ordering.
const attachmentWireVersion = "image-extensions-v2"

func attachmentImageSpec(mediaType string) (canonical, extension string, ok bool) {
	switch strings.ToLower(strings.TrimSpace(mediaType)) {
	case "image/jpeg", "image/jpg":
		return "image/jpeg", ".jpg", true
	case "image/png":
		return "image/png", ".png", true
	case "image/gif":
		return "image/gif", ".gif", true
	case "image/webp":
		return "image/webp", ".webp", true
	default:
		return "", "", false
	}
}

func unsupportedImageError() error {
	return &attachmentError{status: 400, code: "bps_unsupported_image", text: "BPS 图片附件仅支持 JPEG、PNG、GIF 或 WebP；请转换图片格式后再发送"}
}

type attachmentError struct {
	status  int
	code    string
	text    string
	sent    bool
	headers http.Header
}

func (e *attachmentError) Error() string { return e.text }

type attachmentEntry struct{ key, id string }
type attachmentCache struct {
	mu      sync.Mutex
	entries map[string]*list.Element
	order   list.List
}

func (c *attachmentCache) obtain(key string, upload func() (string, error)) (string, bool, error) {
	c.mu.Lock()
	if entry := c.entries[key]; entry != nil {
		c.order.MoveToFront(entry)
		id := entry.Value.(attachmentEntry).id
		c.mu.Unlock()
		return id, true, nil
	}
	c.mu.Unlock()
	// Only completed uploads are shared. Each in-flight request owns its
	// upload context, so one client's cancellation cannot fail another client.
	id, err := upload()
	if err != nil {
		return "", false, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if entry := c.entries[key]; entry != nil {
		entry.Value = attachmentEntry{key, id}
		c.order.MoveToFront(entry)
	} else {
		if c.entries == nil {
			c.entries = map[string]*list.Element{}
		}
		c.entries[key] = c.order.PushFront(attachmentEntry{key, id})
		if c.order.Len() > 512 {
			old := c.order.Back()
			delete(c.entries, old.Value.(attachmentEntry).key)
			c.order.Remove(old)
		}
	}
	return id, false, nil
}

func inlineImageBytes(value string) (string, []byte, error) {
	invalid := func() (string, []byte, error) {
		return "", nil, &attachmentError{status: 400, code: "bps_invalid_image", text: "内嵌图片必须是非空、有效的 image/* data URL"}
	}
	if len(value) < 5 || !strings.EqualFold(value[:5], "data:") {
		return invalid()
	}
	header, payload, ok := strings.Cut(value[5:], ",")
	if !ok {
		return invalid()
	}
	b64 := strings.HasSuffix(strings.ToLower(header), ";base64")
	if b64 {
		header = header[:len(header)-7]
	}
	mediaType, _, err := mime.ParseMediaType(header)
	if err != nil || !strings.HasPrefix(mediaType, "image/") {
		return invalid()
	}
	decoded, err := url.PathUnescape(payload)
	if err != nil {
		return invalid()
	}
	data := []byte(decoded)
	if b64 {
		data, err = base64.StdEncoding.DecodeString(decoded)
	}
	if err != nil || len(data) == 0 {
		return invalid()
	}
	return mediaType, data, nil
}

func (s *Server) uploadInputImages(ctx context.Context, plan *requestPlan, headers http.Header, proxy string) (*attachmentReport, error) {
	return s.uploadInputImagesAvailable(ctx, plan, headers, proxy, false)
}

func (s *Server) uploadInputImagesAvailable(ctx context.Context, plan *requestPlan, headers http.Header, proxy string, unavailable bool) (*attachmentReport, error) {
	report := &attachmentReport{}
	base, err := url.Parse(s.responsesURL)
	if err != nil || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" || (base.Scheme != "https" && base.Scheme != "http") {
		return report, errors.New("无法确定同源 BPS 附件地址")
	}
	endpoint := base.ResolveReference(&url.URL{Path: "attachments"}).String()
	items, _ := plan.body["input"].([]any)
	for index, raw := range items {
		item, ok := raw.(object)
		if !ok || str(item, "role") != "user" || (str(item, "type") != "" && str(item, "type") != "message") {
			continue
		}
		parts, _ := item["content"].([]any)
		var rewritten []any
		for j, raw := range parts {
			part, ok := raw.(object)
			dataURL := str(part, "image_url")
			if !ok || str(part, "type") != "input_image" || len(dataURL) < 5 || !strings.EqualFold(dataURL[:5], "data:") {
				continue
			}
			if str(part, "file_id") != "" {
				return report, &attachmentError{status: 400, code: "bps_invalid_image", text: "图片不能同时包含 image_url 与 file_id"}
			}
			mediaType, data, err := inlineImageBytes(dataURL)
			if err != nil {
				return report, err
			}
			canonical, extension, supported := attachmentImageSpec(mediaType)
			if !supported {
				return report, unsupportedImageError()
			}
			mediaType = canonical
			name := "image" + extension
			info := attachmentImageDiagnostic{Path: fmt.Sprintf("input[%d].content[%d]", index, j), Filename: name, MIME: mediaType, Bytes: len(data), State: "failed"}
			// The cache retains only a hash and file ID, never pixels or token.
			// Key the actual upload format. The cache is process-local and is
			// also cleared when the new plugin process starts on upgrade.
			key := digest([]any{attachmentWireVersion, endpoint, plan.store.accountID, headers.Get("Chatgpt-Account-Id"), headers.Get("Authorization"), mediaType, extension, digest(data)})
			id, reused, err := s.attachments.obtain(key, func() (string, error) {
				if unavailable {
					return "", &attachmentError{code: "bps_attachment_unavailable", text: "当前请求的附件连接不可用"}
				}
				return s.uploadImage(ctx, endpoint, headers, proxy, mediaType, name, data, dataURL, report)
			})
			if !reused {
				info.HTTPStatus = report.HTTPStatus
			}
			if err == nil {
				info.State = "uploaded"
				if reused {
					info.State = "reused"
				}
			}
			if len(report.Images) < 16 {
				report.Images = append(report.Images, info)
			}
			if err != nil {
				return report, err
			}
			if reused {
				report.Reused++
			} else {
				report.Uploaded++
			}
			if rewritten == nil {
				rewritten = append([]any(nil), parts...)
			}
			copy := clone(part)
			delete(copy, "image_url")
			copy["file_id"] = id
			if _, exists := copy["detail"]; !exists {
				copy["detail"] = "auto"
			}
			rewritten[j] = copy
		}
		if rewritten != nil {
			copy := clone(item)
			copy["content"] = rewritten
			items[index] = copy
		}
	}
	return report, nil
}

func (s *Server) uploadImage(ctx context.Context, endpoint string, headers http.Header, proxy, mediaType, name string, data []byte, dataURL string, report *attachmentReport) (string, error) {
	// These fields describe the latest upload attempt, never an earlier
	// successful image when a subsequent request fails before receiving HTTP.
	report.Attempts++
	report.HTTPStatus = 0
	report.ContentType, report.RequestID, report.Diagnostic, report.DiagnosticState = "", "", "", ""
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	ph := make(textproto.MIMEHeader)
	ph.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{"name": "file", "filename": name}))
	ph.Set("Content-Type", mediaType)
	part, err := w.CreatePart(ph)
	if err != nil {
		return "", errors.New("无法创建图片附件")
	}
	if _, err := part.Write(data); err != nil {
		return "", errors.New("无法编码图片附件")
	}
	if err := w.Close(); err != nil {
		return "", errors.New("无法完成图片附件编码")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, &body)
	if err != nil {
		return "", errors.New("BPS 附件地址无效")
	}
	req.Header = headers.Clone()
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", w.FormDataContentType())
	t, err := s.pool.get(proxy)
	if err != nil {
		return "", err
	}
	resp, err := t.RoundTrip(req)
	if err != nil {
		return "", &attachmentError{code: "bps_attachment_transport", text: "图片上传失败：" + networkError(ctx, err), sent: true}
	}
	defer resp.Body.Close()
	report.HTTPStatus = resp.StatusCode
	report.ContentType, _, _ = mime.ParseMediaType(resp.Header.Get("Content-Type"))
	report.RequestID = redactProbeText(resp.Header.Get("X-Request-Id"), headers, dataURL)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// A missing/oversized error body must not turn an observed HTTP
		// rejection into an uncertain transport error in the host scheduler.
		// Use the production error projection, including URL, quoted-value
		// and opaque-value redaction. Cover both original and decoded encodings.
		request := object{"input": []any{dataURL, base64.StdEncoding.EncodeToString(data)}}
		report.Diagnostic, report.DiagnosticState = readUpstreamDiagnostic(resp.Body, headers, nil, request)
		return "", &attachmentError{status: resp.StatusCode, code: "bps_attachment_http", text: fmt.Sprintf("BPS 图片上传返回 HTTP %d；尚未发送识图请求", resp.StatusCode), sent: true, headers: responseHeaders(resp.Header)}
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (64<<10)+1))
	if err != nil {
		report.DiagnosticState = "read_error"
		return "", &attachmentError{code: "bps_attachment_transport", text: "图片上传响应传输中断", sent: true}
	}
	if len(raw) > 64<<10 {
		report.DiagnosticState = "too_large"
		return "", &attachmentError{code: "bps_attachment_response", text: "图片上传响应超过 64 KiB", sent: true}
	}
	value, parseErr := decodeObject(raw)
	id := strings.TrimSpace(str(value, "openai_file_id"))
	if parseErr != nil || id == "" || len(id) > 512 || strings.ContainsAny(id, "\r\n") {
		report.DiagnosticState = "missing_file_id"
		if parseErr != nil {
			report.DiagnosticState = "invalid_json"
		}
		return "", &attachmentError{code: "bps_attachment_response", text: "图片上传响应没有有效的 openai_file_id", sent: true}
	}
	return id, nil
}
