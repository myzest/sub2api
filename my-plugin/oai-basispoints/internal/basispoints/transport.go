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
		if w.diagnostic.ErrorSource == "" {
			w.diagnostic.ErrorSource = "plugin_transport"
		}
		message += "；诊断 ID：" + w.diagnostic.ID
	}
	return w.stream.Send(&pluginv1.ForwardResponse{Frame: &pluginv1.ForwardResponse_Error{Error: &pluginv1.ForwardResponseError{Code: code, Message: message, RequestSent: sent}}})
}
func (w *forwardWriter) start(resp *http.Response) error {
	err := w.stream.Send(&pluginv1.ForwardResponse{Frame: &pluginv1.ForwardResponse_Start{Start: &pluginv1.ForwardResponseStart{StatusCode: int32(resp.StatusCode), Status: resp.Status, Protocol: resp.Proto, ProtocolMajor: int32(resp.ProtoMajor), ProtocolMinor: int32(resp.ProtoMinor), Headers: headersToProto(resp.Header), ContentLength: resp.ContentLength}}})
	if err == nil && w.diagnostic != nil {
		w.diagnostic.ClientHTTPStatus = resp.StatusCode
	}
	return err
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
				alreadyIdentified := str(failure, "diagnostic_id") == w.diagnostic.ID
				failure["diagnostic_id"] = w.diagnostic.ID
				failure["source"] = w.diagnostic.ErrorSource
				failure["responses_started"] = w.diagnostic.ResponsesStarted
				failure["upstream_started"] = w.diagnostic.UpstreamStarted
				if !alreadyIdentified {
					failure["message"] = str(failure, "message") + "；诊断 ID：" + w.diagnostic.ID
				}
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
	if w.diagnostic != nil && w.diagnostic.ErrorSource == "" {
		w.diagnostic.ErrorSource = "plugin_local_request"
	}
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
	d := s.startDiagnostic(start.AccountId, cfg.ImageTransport)
	if isRouteProbe {
		d.Origin = "image_route_probe"
	}
	w.diagnostic = d
	defer func() {
		if result != nil && d.Error == "" {
			d.Error = "客户端传输中断，响应未完整交付"
			d.ErrorSource = "client_transport"
			if stream.Context().Err() != nil {
				d.ErrorSource = "request_canceled"
				if errors.Is(stream.Context().Err(), context.DeadlineExceeded) {
					d.ErrorSource = "request_timeout_or_canceled"
				}
			}
		}
		s.finishDiagnostic(d)
		if isRouteProbe {
			probeObserver(d)
		}
	}()
	u, err := url.Parse(start.Url)
	if u != nil {
		d.ImageOperation = imageOperation(u.Path)
		if d.ImageOperation != "" {
			d.ImageTransport = "json"
			if d.ImageOperation == "edits" {
				d.ImageTransport = "multipart"
			}
		}
	}
	if err != nil || start.Method != http.MethodPost || u == nil || (d.ImageOperation == "" && (!strings.HasSuffix(u.Path, "/responses") || strings.Contains(u.Path, "//"))) {
		return w.reject(400, "bps_unsupported_endpoint", "所选 BPS 账号支持 POST /responses、/images/generations、/images/edits；不支持 /responses/compact 或 /input_tokens")
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
	if d.ImageOperation != "" {
		return s.forwardImages(ctx, w, start, buffer.Bytes())
	}
	d.Replay = &replayDiagnostic{}
	ctx = context.WithValue(ctx, replayDiagnosticContextKey{}, d.Replay)
	d.input(buffer.Bytes())
	d.Stage = "prepare"
	plan, err := s.prepare(ctx, buffer.Bytes(), start.AccountId, headersFromProto(start.Headers), cfg)
	if err != nil {
		if errors.Is(err, errModelNotAllowed) {
			d.ErrorSource = "plugin_local_config"
			return w.reject(400, "bps_model_not_allowed", err.Error())
		}
		if d.Replay.Failure != "" {
			d.ErrorSource = "plugin_history"
		}
		return w.reject(400, "bps_invalid_request", err.Error())
	}
	plan.tools.observeRelay = d.observeRelay
	if d.AgentInput != nil {
		d.AgentInput.Handling = "preserved"
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
	var pictures *pictureFallback
	if cfg.ImageTransport == "attachment" {
		pictures = s.newPictureFallback(plan, headers, start.ProxyUrl)
	}
	for {
		// Previous attempts are retained separately. A canceled upload on the
		// next attempt must not inherit the earlier Responses HTTP/Request ID.
		d.HTTPStatus, d.ClientHTTPStatus, d.UpstreamRequestBytes = 0, 0, 0
		d.ErrorSource, d.RequestID, d.ContentType, d.UpstreamError, d.UpstreamErrorState = "", "", "", "", ""
		d.UpstreamErrorEvent = ""
		d.UpstreamImages = nil
		if pictures != nil {
			d.Stage = "upload"
			err := pictures.rewrite(ctx)
			report := pictures.report
			d.AttachmentHTTPStatus = report.HTTPStatus
			d.Attachment = report
			d.UpstreamStarted = d.UpstreamStarted || report.Attempts > 0
			d.OmittedImages = pictures.omitted
			if err != nil {
				d.ErrorSource = "attachment"
				var failure *attachmentError
				if errors.As(err, &failure) {
					if failure.status != 0 {
						return w.json(failure.status, object{"error": object{"type": "bps_attachment_error", "code": failure.code, "message": failure.text}}, failure.headers)
					}
					return w.fail(failure.code, failure.text, failure.sent)
				}
				if ctx.Err() != nil {
					d.ErrorSource = "request_timeout_or_canceled"
				}
				return w.fail("bps_attachment", err.Error(), d.UpstreamStarted)
			}
		}
		d.UpstreamImages, _ = summarizeImages(plan.body)
		body := encoded(plan.body)
		d.UpstreamRequestBytes = len(body)
		if len(body) > maxBody {
			return w.reject(400, "bps_request_body", "转换后的 BPS 请求超过 8 MiB")
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.responsesURL, bytes.NewReader(body))
		if err != nil {
			return w.fail("invalid_url", "BPS 地址无效", false)
		}
		req.Header = headers
		d.Stage = "connect"
		// Once RoundTrip is entered, conservatively prohibit host account replay.
		d.ResponsesStarted = true
		d.UpstreamStarted = true
		d.ResponseAttempts++
		resp, err := transport.RoundTrip(req)
		if err != nil {
			if !plan.stream && d.ProtocolRetries == 0 && ctx.Err() == nil && protocolInterruption(err) {
				d.ProtocolRetries++
				d.retry("protocol_interruption")
				continue
			}
			d.ErrorSource = "upstream_transport"
			if ctx.Err() != nil {
				d.ErrorSource = "request_timeout_or_canceled"
			}
			return w.fail("bps_transport", networkError(ctx, err), true)
		}
		defer resp.Body.Close()
		d.Stage, d.HTTPStatus = "http", resp.StatusCode
		d.ContentType, _, _ = mime.ParseMediaType(resp.Header.Get("Content-Type"))
		d.RequestID = redactProbeText(resp.Header.Get("X-Request-Id"), headers, "")
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			d.ErrorSource = "upstream_http"
			d.UpstreamError, d.UpstreamErrorState = readUpstreamDiagnostic(resp.Body)
			if pictures != nil && ctx.Err() == nil {
				if action := pictures.retry(resp.StatusCode); action != "" {
					d.retry(action)
					if len(d.ImageFallbacks) < 16 {
						d.ImageFallbacks = append(d.ImageFallbacks, action)
					}
					resp.Body.Close()
					continue
				}
			}
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
		relay.projectError = func(event object) object {
			fields, state := upstreamFailureFields(event)
			d.UpstreamErrorEvent, d.UpstreamErrorState = str(event, "type"), state
			if len(fields) > 0 {
				d.UpstreamError = string(encoded(fields))
			}
			d.ErrorSource = "upstream_stream"
			if d.ContentType != "text/event-stream" {
				d.ErrorSource = "upstream_response"
			}
			d.Error = "bps_upstream_event: BPS 返回 " + d.UpstreamErrorEvent + "；未完成响应"
			failure := upstreamClientError(fields)
			failure["diagnostic_id"] = d.ID
			failure["message"] = str(failure, "message") + "；诊断 ID：" + d.ID
			return failure
		}
		consumeErr := relay.consume(resp)
		d.Keepalives += relay.keepalives
		d.CompletionRecovered = relay.recovered
		// A verified response with an invalid relay is a protocol failure, not
		// a broken HTTP connection. Keep the stream valid and retain its ID.
		var envelopeErr *relayDecodeError
		if consumeErr != nil && relay.conversionFailed && ctx.Err() == nil &&
			errors.As(consumeErr, &envelopeErr) {
			d.ErrorSource = "tool_relay"
			d.Error = "bps_tool_envelope_invalid: " + limitCharacters(consumeErr.Error(), 1000)
			consumeErr = relay.failEnvelope(d.ID)
		}
		d.SkippedTools = plan.tools.skippedTools
		if err := consumeErr; err != nil {
			if !plan.stream && d.ProtocolRetries == 0 && ctx.Err() == nil && protocolInterruption(err) {
				d.ProtocolRetries++
				d.retry("protocol_interruption")
				resp.Body.Close()
				continue
			}
			if d.ErrorSource == "" {
				d.ErrorSource = "response_conversion"
			}
			var decodeErr *relayDecodeError
			if errors.As(err, &decodeErr) {
				d.ErrorSource = "tool_relay"
			}
			if ctx.Err() != nil {
				d.ErrorSource = "request_timeout_or_canceled"
			}
			return w.fail("bps_response", err.Error(), true)
		}
		d.Terminal = relay.terminal
		d.output(relay.response)
		if relay.terminal == "response.completed" {
			d.Stage = "completed"
		} else if d.Error == "" {
			d.Error = "BPS 返回 " + relay.terminal
			d.ErrorSource = "upstream_response"
		}
		if !plan.stream {
			if relay.terminal == "error" {
				// The upstream HTTP was 200, but no Responses response exists.
				// 502 is the gateway result, not an invented upstream status.
				return w.json(http.StatusBadGateway, relay.response, responseHeaders(resp.Header))
			}
			return w.json(200, relay.response, responseHeaders(resp.Header))
		}
		if err := w.body([]byte("data: [DONE]\n\n")); err != nil {
			return err
		}
		return w.end()
	}
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
	p.Relay, p.Replay = nil, &replayDiagnostic{}
	ctx = context.WithValue(ctx, replayDiagnosticContextKey{}, p.Replay)
	p.Stage = "prepare"
	s.publishProbe(p)
	defer func() {
		p.Rounds = append(p.Rounds, probeRoundResult{Round: p.Round, HTTPStatus: p.HTTPStatus, RequestID: p.RequestID, ResponseID: p.ResponseID, ReturnedModel: p.ReturnedModel, LatencyMS: time.Since(started).Milliseconds(), Usage: p.Usage, Relay: p.Relay, Replay: p.Replay})
		s.publishProbe(p)
	}()
	plan, err := s.prepare(ctx, encoded(source), p.AccountID, h, cfg)
	if err != nil {
		return nil, err
	}
	plan.tools.observeRelay = func(value relayDiagnostic) {
		if len(p.Relay) < 16 {
			p.Relay = append(p.Relay, value)
		}
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
		p.UpstreamError, _ = readUpstreamDiagnostic(resp.Body)
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
	}
	r.projectError = func(event object) object {
		fields, _ := upstreamFailureFields(event)
		if len(fields) > 0 {
			p.UpstreamError = string(encoded(fields))
		}
		return upstreamClientError(fields)
	}
	if err := r.consume(resp); err != nil {
		return nil, err
	}
	if model := str(r.response, "model"); model != "" {
		p.ReturnedModel = redactProbeText(model, headers, p.ImagePreview)
	}
	if id := str(r.response, "id"); id != "" {
		p.ResponseID = redactProbeText(id, headers, p.ImagePreview)
	}
	p.Usage = r.response["usage"]
	if r.terminal != "response.completed" {
		return nil, errors.New("BPS 返回 " + r.terminal + "；通道尚未通过探测")
	}
	return r.response, nil
}
