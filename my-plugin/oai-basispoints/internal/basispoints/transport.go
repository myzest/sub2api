package basispoints

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	pluginv1 "local.sub2api/oai-basispoints/internal/pluginapi"
)

type forwardWriter struct {
	stream     pluginv1.TransportPlugin_ForwardServer
	started    time.Time
	bytes      int64
	diagnostic *requestDiagnostic
}

func (w *forwardWriter) fail(code, message string, sent bool) error {
	if w.diagnostic != nil {
		w.diagnostic.Error = limitCharacters(code+": "+message, 1600)
		message += "；诊断 ID：" + w.diagnostic.ID
	}
	return w.stream.Send(&pluginv1.ForwardResponse{Frame: &pluginv1.ForwardResponse_Error{Error: &pluginv1.ForwardResponseError{Code: code, Message: message, RequestSent: sent}}})
}
func (w *forwardWriter) start(resp *http.Response) error {
	return w.stream.Send(&pluginv1.ForwardResponse{Frame: &pluginv1.ForwardResponse_Start{Start: &pluginv1.ForwardResponseStart{StatusCode: int32(resp.StatusCode), Status: resp.Status, Protocol: resp.Proto, ProtocolMajor: int32(resp.ProtoMajor), ProtocolMinor: int32(resp.ProtoMinor), Headers: headersToProto(resp.Header), ContentLength: resp.ContentLength}}})
}
func (w *forwardWriter) body(raw []byte) error {
	for len(raw) > 0 {
		n := min(len(raw), 32<<10)
		if err := w.stream.Send(&pluginv1.ForwardResponse{Frame: &pluginv1.ForwardResponse_BodyChunk{BodyChunk: raw[:n]}}); err != nil {
			return err
		}
		w.bytes += int64(n)
		raw = raw[n:]
	}
	return nil
}
func (w *forwardWriter) end() error {
	return w.stream.Send(&pluginv1.ForwardResponse{Frame: &pluginv1.ForwardResponse_End{End: &pluginv1.ForwardResponseEnd{BytesReceived: w.bytes, DurationMs: time.Since(w.started).Milliseconds()}}})
}
func (w *forwardWriter) json(status int, value any, headers http.Header) error {
	if status >= 400 && w.diagnostic != nil {
		if body, ok := value.(object); ok {
			if failure, ok := body["error"].(object); ok {
				w.diagnostic.Error = limitCharacters(str(failure, "code")+": "+str(failure, "message"), 1600)
				failure["diagnostic_id"] = w.diagnostic.ID
				failure["message"] = str(failure, "message") + "；诊断 ID：" + w.diagnostic.ID
			}
		}
	}
	data := encoded(value)
	if headers == nil {
		headers = http.Header{}
	}
	headers.Set("Content-Type", "application/json")
	headers.Set("Cache-Control", "no-store")
	resp := &http.Response{StatusCode: status, Status: fmt.Sprintf("%d %s", status, http.StatusText(status)), Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1, Header: headers, ContentLength: int64(len(data))}
	if err := w.start(resp); err != nil {
		return err
	}
	if err := w.body(data); err != nil {
		return err
	}
	return w.end()
}
func (w *forwardWriter) reject(status int, code, message string) error {
	return w.json(status, object{"error": object{"type": "invalid_request_error", "code": code, "message": message}}, nil)
}

func (s *Server) Forward(stream pluginv1.TransportPlugin_ForwardServer) (result error) {
	w := &forwardWriter{stream: stream, started: time.Now()}
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	start := first.GetStart()
	if start == nil || start.Platform != "openai" || start.AccountType != "oauth" {
		return w.fail("invalid_start", "无效的 OpenAI OAuth 转发请求", false)
	}
	cfg := s.config()
	probeObserver, isRouteProbe := stream.Context().Value(imageRouteProbeContextKey{}).(func(*requestDiagnostic))
	if !cfg.routes(start.AccountId) {
		if isRouteProbe {
			return w.reject(400, "bps_route_disabled", "图片路由探测需要开启 BPS 路由并将所选账号加入白名单；不会改走普通通道")
		}
		return s.passthrough(w, start)
	}
	d := &requestDiagnostic{ID: newID(), Version: Version, Instance: s.instance, Origin: "route", ImageTransport: cfg.ImageTransport, AccountID: start.AccountId, StartedAt: time.Now().Unix(), Stage: "request", ToolTypes: map[string]int{}, ToolNames: []string{}}
	if isRouteProbe {
		d.Origin = "image_route_probe"
	}
	w.diagnostic = d
	defer func() {
		if result != nil && d.Error == "" {
			d.Error = "客户端传输中断，响应未完整交付"
		}
		s.finishDiagnostic(d)
		if isRouteProbe {
			probeObserver(d)
		}
	}()
	u, err := url.Parse(start.Url)
	if err != nil || start.Method != http.MethodPost || u == nil || !strings.HasSuffix(u.Path, "/responses") || strings.HasSuffix(u.Path, "//responses") {
		return w.reject(400, "bps_unsupported_endpoint", "所选 BPS 账号仅支持 POST /responses；不支持 /responses/compact 或 /input_tokens")
	}
	var buffer bytes.Buffer
	if err := receiveBody(stream, &buffer, start.HasBody, maxBody); err != nil {
		return w.reject(400, "bps_request_body", err.Error())
	}
	if start.ContentLength >= 0 && start.ContentLength != int64(buffer.Len()) {
		return w.reject(400, "bps_request_body", "请求体长度不一致")
	}
	ctx, cancel := context.WithTimeout(stream.Context(), time.Duration(cfg.TimeoutSeconds)*time.Second)
	defer cancel()
	d.input(buffer.Bytes())
	d.Stage = "prepare"
	plan, err := s.prepare(ctx, buffer.Bytes(), start.AccountId, headersFromProto(start.Headers), cfg)
	if err != nil {
		return w.reject(400, "bps_invalid_request", err.Error())
	}
	d.catalog(plan.tools)
	d.ReasoningEffort = str(plan.body, "reasoning_effort")
	d.Stage = "identity"
	headers, err := bpsHeaders(headersFromProto(start.Headers))
	if err != nil {
		return w.reject(400, "bps_identity", err.Error())
	}
	transport, err := s.pool.get(start.ProxyUrl)
	if err != nil {
		return w.fail("invalid_proxy", err.Error(), false)
	}
	if cfg.ImageTransport == "attachment" {
		d.Stage = "upload"
		report, err := s.uploadInputImages(ctx, plan, headers, start.ProxyUrl)
		d.AttachmentHTTPStatus = report.HTTPStatus
		d.Attachment = report
		d.UpstreamImages, _ = summarizeImages(plan.body)
		if err != nil {
			var failure *attachmentError
			if errors.As(err, &failure) {
				if failure.status != 0 {
					return w.json(failure.status, object{"error": object{"type": "bps_attachment_error", "code": failure.code, "message": failure.text}}, failure.headers)
				}
				return w.fail(failure.code, failure.text, failure.sent)
			}
			return w.fail("bps_attachment", err.Error(), true)
		}
	}
	d.UpstreamImages, _ = summarizeImages(plan.body)
	body := encoded(plan.body)
	d.UpstreamRequestBytes = len(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.responsesURL, bytes.NewReader(body))
	if err != nil {
		return w.fail("invalid_url", "BPS 地址无效", false)
	}
	req.Header = headers
	d.Stage = "connect"
	// Once RoundTrip is entered, conservatively prohibit host account replay.
	resp, err := transport.RoundTrip(req)
	if err != nil {
		return w.fail("bps_transport", networkError(ctx, err), true)
	}
	defer resp.Body.Close()
	d.Stage, d.HTTPStatus = "http", resp.StatusCode
	d.ContentType, _, _ = mime.ParseMediaType(resp.Header.Get("Content-Type"))
	d.RequestID = redactProbeText(resp.Header.Get("X-Request-Id"), headers, "")
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		d.UpstreamError, d.UpstreamErrorState = readUpstreamDiagnostic(resp.Body, headers, buffer.Bytes(), plan.body)
		return w.json(resp.StatusCode, object{"error": object{"type": "bps_upstream_error", "code": fmt.Sprintf("bps_http_%d", resp.StatusCode), "message": upstreamMessage(resp.StatusCode)}}, responseHeaders(resp.Header))
	}
	var emit eventWriter
	if plan.stream {
		header := responseHeaders(resp.Header)
		header.Set("Content-Type", "text/event-stream")
		header.Set("Cache-Control", "no-cache")
		header.Set("X-Accel-Buffering", "no")
		copy := *resp
		copy.Header, copy.ContentLength = header, -1
		if err := w.start(&copy); err != nil {
			return err
		}
		emit = w.body
	}
	d.Stage = "response"
	relay := newRelay(ctx, plan, emit)
	relay.observe = d.observe
	if err := relay.consume(resp); err != nil {
		return w.fail("bps_response", err.Error(), true)
	}
	d.Terminal = relay.terminal
	d.OutputToolCalls = len(responseToolCalls(relay.response))
	if relay.terminal == "response.completed" {
		d.Stage = "completed"
	} else {
		d.Error = "BPS 返回 " + relay.terminal
	}
	if !plan.stream {
		return w.json(200, relay.response, responseHeaders(resp.Header))
	}
	if err := w.body([]byte("data: [DONE]\n\n")); err != nil {
		return err
	}
	return w.end()
}

func responseHeaders(in http.Header) http.Header {
	out := http.Header{}
	for key, values := range in {
		lower := strings.ToLower(key)
		if lower == "retry-after" || lower == "x-request-id" || strings.HasPrefix(lower, "x-ratelimit-") || strings.HasPrefix(lower, "x-codex-") {
			out[key] = append([]string(nil), values...)
		}
	}
	return out
}
func upstreamMessage(status int) string {
	switch status {
	case 401:
		return "BPS 拒绝当前 OAuth 凭据（HTTP 401）；格式适配不能生成 Excel 会话或更改 token audience"
	case 403:
		return "BPS 拒绝此账号访问（HTTP 403）；需检查账号授权或网络策略"
	case 429:
		return "BPS 限流或额度不足（HTTP 429），请稍后重试"
	case 400, 422:
		return fmt.Sprintf("BPS 不接受请求参数（HTTP %d）；请检查模型、effort 和输入兼容性", status)
	default:
		return fmt.Sprintf("BPS 返回 HTTP %d；原始错误体未透传以避免泄露凭据或输入", status)
	}
}

func receiveBody(stream pluginv1.TransportPlugin_ForwardServer, writer io.Writer, hasBody bool, limit int64) error {
	var size int64
	for {
		frame, err := stream.Recv()
		if err != nil {
			return errors.New("请求体在 body_end 前中断")
		}
		switch v := frame.Frame.(type) {
		case *pluginv1.ForwardRequest_BodyChunk:
			size += int64(len(v.BodyChunk))
			if (!hasBody && len(v.BodyChunk) > 0) || (limit > 0 && size > limit) {
				return errors.New("请求体不存在或超过 8 MiB 限制")
			}
			if writer != nil {
				if _, err := writer.Write(v.BodyChunk); err != nil {
					return errors.New("请求体写入中断")
				}
			}
		case *pluginv1.ForwardRequest_BodyEnd:
			if !v.BodyEnd {
				return errors.New("无效的 body_end")
			}
			return nil
		default:
			return errors.New("非预期的请求帧")
		}
	}
}

// Unselected accounts retain HTTP streaming, duplicate headers and their
// original target, Host and proxy; no BPS transformations apply here.
func (s *Server) passthrough(w *forwardWriter, start *pluginv1.ForwardRequestStart) error {
	transport, err := s.pool.get(start.ProxyUrl)
	if err != nil {
		return w.fail("invalid_proxy", err.Error(), false)
	}
	ctx, cancel := context.WithCancel(w.stream.Context())
	defer cancel()
	var body io.Reader
	var reader *io.PipeReader
	var writer *io.PipeWriter
	if start.HasBody {
		reader, writer = io.Pipe()
		body = reader
		defer reader.Close()
	}
	req, err := http.NewRequestWithContext(ctx, start.Method, start.Url, body)
	if err != nil || req.URL.Host == "" || req.URL.User != nil || (req.URL.Scheme != "https" && req.URL.Scheme != "http") {
		if writer != nil {
			writer.Close()
		}
		return w.fail("invalid_url", "无效的转发地址", false)
	}
	req.Header, req.Host, req.ContentLength = headersFromProto(start.Headers), start.Host, start.ContentLength
	uploadDone := make(chan error, 1)
	go func() {
		var out io.Writer
		if writer != nil {
			out = writer
		}
		err := receiveBody(w.stream, out, start.HasBody, 0)
		if writer != nil {
			writer.CloseWithError(err)
		}
		if err != nil {
			cancel()
		}
		uploadDone <- err
	}()
	resp, err := transport.RoundTrip(req)
	if err != nil {
		return w.fail("upstream_transport", networkError(ctx, err), true)
	}
	defer resp.Body.Close()
	if err := w.start(resp); err != nil {
		return err
	}
	buffer := make([]byte, 32<<10)
	for {
		n, err := resp.Body.Read(buffer)
		if n > 0 {
			if writeErr := w.body(buffer[:n]); writeErr != nil {
				return writeErr
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return w.fail("response_body", networkError(ctx, err), true)
		}
	}
	select {
	case err := <-uploadDone:
		if err != nil {
			return w.fail("request_body", err.Error(), true)
		}
	default:
	}
	return w.end()
}

func (s *Server) probeRound(ctx context.Context, p *Probe, cfg Config, source object, h, headers http.Header, proxy string) (object, error) {
	started := time.Now()
	p.HTTPStatus, p.RequestBytes = 0, 0
	p.RequestID, p.ContentType, p.UpstreamError = "", "", ""
	p.ResponseID, p.ReturnedModel, p.Usage, p.Attachment = "", "", nil, nil
	p.Stage = "prepare"
	s.publishProbe(p)
	defer func() {
		p.Rounds = append(p.Rounds, probeRoundResult{Round: p.Round, HTTPStatus: p.HTTPStatus, RequestID: p.RequestID, ResponseID: p.ResponseID, ReturnedModel: p.ReturnedModel, LatencyMS: time.Since(started).Milliseconds(), Usage: p.Usage})
		s.publishProbe(p)
	}()
	plan, err := s.prepare(ctx, encoded(source), p.AccountID, h, cfg)
	if err != nil {
		return nil, err
	}
	if cfg.ImageTransport == "attachment" {
		p.Stage = "upload"
		s.publishProbe(p)
		p.Attachment, err = s.uploadInputImages(ctx, plan, headers, proxy)
		if err != nil {
			return nil, err
		}
	}
	body := encoded(plan.body)
	p.RequestBytes = len(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.responsesURL, bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("BPS 地址无效")
	}
	req.Header = headers
	p.Stage = "connect"
	s.publishProbe(p)
	t, err := s.pool.get(proxy)
	if err != nil {
		return nil, err
	}
	resp, err := t.RoundTrip(req)
	if err != nil {
		return nil, errors.New(networkError(ctx, err))
	}
	defer resp.Body.Close()
	p.Stage = "http"
	p.HTTPStatus = resp.StatusCode
	p.ContentType, _, _ = mime.ParseMediaType(resp.Header.Get("Content-Type"))
	p.RequestID = redactProbeText(resp.Header.Get("X-Request-Id"), headers, p.ImagePreview)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10)); err == nil {
			if value, err := decodeObject(raw); err == nil {
				p.UpstreamError = probeDiagnostic(value, headers, p.ImagePreview)
			}
		}
		return nil, errors.New(upstreamMessage(resp.StatusCode))
	}
	p.Stage = "response"
	s.publishProbe(p)
	r := newRelay(ctx, plan, nil)
	r.observe = func(event object) {
		if response, ok := event["response"].(object); ok {
			if model := str(response, "model"); model != "" {
				p.ReturnedModel = redactProbeText(model, headers, p.ImagePreview)
			}
			if id := str(response, "id"); id != "" {
				p.ResponseID = redactProbeText(id, headers, p.ImagePreview)
			}
		}
		if detail := probeDiagnostic(event, headers, p.ImagePreview); detail != "" {
			p.UpstreamError = detail
		}
	}
	if err := r.consume(resp); err != nil {
		return nil, err
	}
	p.ReturnedModel = redactProbeText(str(r.response, "model"), headers, p.ImagePreview)
	p.ResponseID, p.Usage = redactProbeText(str(r.response, "id"), headers, p.ImagePreview), r.response["usage"]
	if r.terminal != "response.completed" {
		return nil, errors.New("BPS 返回 " + r.terminal + "；通道尚未通过探测")
	}
	return r.response, nil
}
