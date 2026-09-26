package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"time"

	"local.sub2api/codex-inspector/internal/diag"
	pluginv1 "local.sub2api/codex-inspector/internal/pluginapi/v1"
	"local.sub2api/codex-inspector/internal/transport"
	"local.sub2api/codex-inspector/internal/tzrewrite"
)

const maxBodyBytes = 64 * 1024 * 1024
const chunkSize = 32 * 1024

func (s *Server) Forward(stream pluginv1.TransportPlugin_ForwardServer) error {
	started := time.Now()
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	start := first.GetStart()
	if start == nil || start.Platform != "openai" || start.AccountType != "oauth" {
		return sendErr(stream, "invalid_start", "无效的 OpenAI OAuth 转发请求", false)
	}
	var buf bytes.Buffer
	for {
		frame, e := stream.Recv()
		if e != nil {
			return sendErr(stream, "request_body", "请求体在 body_end 前中断", false)
		}
		if frame == nil {
			return sendErr(stream, "request_body", "无效请求帧", false)
		}
		switch f := frame.Frame.(type) {
		case *pluginv1.ForwardRequest_BodyChunk:
			if !start.HasBody && len(f.BodyChunk) > 0 {
				return sendErr(stream, "request_body", "非预期请求体", false)
			}
			if len(f.BodyChunk) > maxBodyBytes-buf.Len() {
				return sendErr(stream, "request_body", "请求体超过 64 MiB", false)
			}
			buf.Write(f.BodyChunk)
		case *pluginv1.ForwardRequest_BodyEnd:
			if !f.BodyEnd {
				return sendErr(stream, "request_body", "无效结束帧", false)
			}
			goto bodyReady
		default:
			return sendErr(stream, "request_body", "非预期请求帧", false)
		}
	}
bodyReady:
	body := buf.Bytes()
	cfg := s.cfg.Load()
	if cfg.Enabled && len(body) > 0 {
		target := s.tz.Target(stream.Context(), cfg.Timezone, start.AccountId, start.ProxyUrl)
		out, blocks, changed, e := tzrewrite.Rewrite(body, target, s.now())
		if e != nil {
			s.diag.Tally("tz_rewrite", "error")
		} else if changed {
			body = out
			s.diag.Tally("tz_rewrite", "ok")
		}
		if blocks >= 2 {
			s.diag.Tally("tz_multi_env", "ok")
		}
	}
	req, e := http.NewRequestWithContext(stream.Context(), start.Method, start.Url, bytes.NewReader(body))
	if e != nil || req.URL.Host == "" || req.URL.User != nil || (req.URL.Scheme != "http" && req.URL.Scheme != "https") {
		return sendErr(stream, "invalid_url", "无效的出站 URL", false)
	}
	req.Header = toHeader(start.Headers)
	req.Header.Del("Content-Length")
	req.Header.Del("Transfer-Encoding")
	req.Host = start.Host
	req.ContentLength = int64(len(body))
	client, e := s.pool.Client(start.ProxyUrl)
	if e != nil {
		return sendErr(stream, "invalid_proxy", e.Error(), false)
	}
	resp, e := client.Do(req)
	if e != nil {
		return sendErr(stream, "upstream_transport", networkMessage(stream.Context(), e), transport.RequestSent(e))
	}
	defer resp.Body.Close()
	if e = stream.Send(&pluginv1.ForwardResponse{Frame: &pluginv1.ForwardResponse_Start{Start: &pluginv1.ForwardResponseStart{StatusCode: int32(resp.StatusCode), Status: resp.Status, Protocol: resp.Proto, ProtocolMajor: int32(resp.ProtoMajor), ProtocolMinor: int32(resp.ProtoMinor), Headers: fromHeader(resp.Header), ContentLength: resp.ContentLength}}}); e != nil {
		return e
	}
	chunk := make([]byte, chunkSize)
	var received int64
	for {
		n, readErr := resp.Body.Read(chunk)
		if n > 0 {
			received += int64(n)
			if e = stream.Send(&pluginv1.ForwardResponse{Frame: &pluginv1.ForwardResponse_BodyChunk{BodyChunk: append([]byte(nil), chunk[:n]...)}}); e != nil {
				return e
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return sendErr(stream, "response_body", networkMessage(stream.Context(), readErr), true)
		}
	}
	return stream.Send(&pluginv1.ForwardResponse{Frame: &pluginv1.ForwardResponse_End{End: &pluginv1.ForwardResponseEnd{BytesReceived: received, DurationMs: time.Since(started).Milliseconds()}}})
}

func networkMessage(ctx context.Context, err error) string {
	if ctx.Err() != nil {
		return "请求已取消或超时"
	}
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		return "上游连接或响应超时"
	}
	// Even sanitized URL errors may repeat a proxy host in a nested dial error.
	return "上游网络传输失败，请检查账号代理和网络连接"
}
func toHeader(in map[string]*pluginv1.HeaderValues) http.Header {
	out := http.Header{}
	for k, v := range in {
		if v != nil {
			for _, s := range v.Values {
				out.Add(k, s)
			}
		}
	}
	return out
}
func fromHeader(in http.Header) map[string]*pluginv1.HeaderValues {
	out := map[string]*pluginv1.HeaderValues{}
	for k, v := range in {
		out[k] = &pluginv1.HeaderValues{Values: append([]string(nil), v...)}
	}
	return out
}
func sendErr(stream pluginv1.TransportPlugin_ForwardServer, code, message string, sent bool) error {
	return stream.Send(&pluginv1.ForwardResponse{Frame: &pluginv1.ForwardResponse_Error{Error: &pluginv1.ForwardResponseError{Code: code, Message: diag.SanitizeError(message), RequestSent: sent}}})
}
