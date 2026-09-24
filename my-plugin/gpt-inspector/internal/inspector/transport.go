package inspector

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	pluginv1 "local.sub2api/gpt-inspector/internal/pluginapi"
)

type transportEntry struct {
	transport *http.Transport
	used      time.Time
}
type transportPool struct {
	mu      sync.Mutex
	entries map[string]*transportEntry
}

func (p *transportPool) get(proxy string) (*http.Transport, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.entries == nil {
		p.entries = map[string]*transportEntry{}
	}
	if e := p.entries[proxy]; e != nil {
		e.used = time.Now()
		return e.transport, nil
	}
	var proxyFunc func(*http.Request) (*url.URL, error)
	if proxy != "" {
		u, err := url.Parse(proxy)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks5" && u.Scheme != "socks5h") {
			return nil, errors.New("账号代理地址无效")
		}
		proxyFunc = http.ProxyURL(u)
	}
	if len(p.entries) >= 32 {
		var oldest string
		var when time.Time
		for k, e := range p.entries {
			if when.IsZero() || e.used.Before(when) {
				oldest, when = k, e.used
			}
		}
		p.entries[oldest].transport.CloseIdleConnections()
		delete(p.entries, oldest)
	}
	t := &http.Transport{Proxy: proxyFunc, DialContext: (&net.Dialer{Timeout: 20 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}, TLSHandshakeTimeout: 20 * time.Second,
		IdleConnTimeout: 90 * time.Second, MaxIdleConns: 64,
		MaxIdleConnsPerHost: 16, ForceAttemptHTTP2: true, DisableCompression: true, ExpectContinueTimeout: time.Second}
	p.entries[proxy] = &transportEntry{t, time.Now()}
	return t, nil
}
func headersFromProto(in map[string]*pluginv1.HeaderValues) http.Header {
	h := make(http.Header)
	for k, v := range in {
		if v != nil {
			h[k] = append([]string(nil), v.Values...)
		}
	}
	return h
}
func headersToProto(in http.Header) map[string]*pluginv1.HeaderValues {
	h := make(map[string]*pluginv1.HeaderValues, len(in))
	for k, v := range in {
		h[k] = &pluginv1.HeaderValues{Values: append([]string(nil), v...)}
	}
	return h
}

func (s *Server) Forward(stream pluginv1.TransportPlugin_ForwardServer) error {
	started := time.Now()
	fail := func(code, message string, sent bool) error {
		return stream.Send(&pluginv1.ForwardResponse{Frame: &pluginv1.ForwardResponse_Error{Error: &pluginv1.ForwardResponseError{Code: code, Message: message, RequestSent: sent}}})
	}
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	start := first.GetStart()
	if start == nil || start.Platform != "openai" || start.AccountType != "oauth" {
		return fail("invalid_start", "无效的 OpenAI OAuth 转发请求", false)
	}
	transport, err := s.pool.get(start.ProxyUrl)
	if err != nil {
		return fail("invalid_proxy", err.Error(), false)
	}
	var body io.Reader
	var reader *io.PipeReader
	var writer *io.PipeWriter
	if start.HasBody {
		reader, writer = io.Pipe()
		body = reader
		defer reader.Close()
	}
	req, err := http.NewRequestWithContext(stream.Context(), start.Method, start.Url, body)
	if err != nil || req.URL.Host == "" || req.URL.User != nil || (req.URL.Scheme != "https" && req.URL.Scheme != "http") {
		if writer != nil {
			writer.Close()
		}
		return fail("invalid_url", "无效的转发地址", false)
	}
	req.Header = headersFromProto(start.Headers)
	req.Host = start.Host
	req.ContentLength = start.ContentLength
	uploadDone := make(chan error, 1)
	go func() {
		err := receiveBody(stream, writer, start.HasBody)
		if writer != nil {
			writer.CloseWithError(err)
		}
		uploadDone <- err
	}()
	// Once RoundTrip is invoked, conservatively prohibit host account replay.
	resp, err := transport.RoundTrip(req)
	if err != nil {
		return fail("upstream_transport", networkError(stream.Context(), err), true)
	}
	defer resp.Body.Close()
	if err := stream.Send(&pluginv1.ForwardResponse{Frame: &pluginv1.ForwardResponse_Start{Start: &pluginv1.ForwardResponseStart{
		StatusCode: int32(resp.StatusCode), Status: resp.Status, Protocol: resp.Proto, ProtocolMajor: int32(resp.ProtoMajor), ProtocolMinor: int32(resp.ProtoMinor), Headers: headersToProto(resp.Header), ContentLength: resp.ContentLength,
	}}}); err != nil {
		return err
	}
	buf := make([]byte, 32*1024)
	var received int64
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			received += int64(n)
			if err := stream.Send(&pluginv1.ForwardResponse{Frame: &pluginv1.ForwardResponse_BodyChunk{BodyChunk: buf[:n]}}); err != nil {
				return err
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return fail("response_body", networkError(stream.Context(), readErr), true)
		}
	}
	select {
	case err := <-uploadDone:
		if err != nil {
			return fail("request_body", "请求体传输中断", true)
		}
	default:
	}
	return stream.Send(&pluginv1.ForwardResponse{Frame: &pluginv1.ForwardResponse_End{End: &pluginv1.ForwardResponseEnd{BytesReceived: received, DurationMs: time.Since(started).Milliseconds()}}})
}
func receiveBody(stream pluginv1.TransportPlugin_ForwardServer, writer *io.PipeWriter, hasBody bool) error {
	for {
		frame, err := stream.Recv()
		if err != nil {
			return fmt.Errorf("request ended before body_end")
		}
		switch x := frame.Frame.(type) {
		case *pluginv1.ForwardRequest_BodyChunk:
			if !hasBody && len(x.BodyChunk) > 0 {
				return errors.New("unexpected request body")
			}
			if writer != nil {
				if _, err := writer.Write(x.BodyChunk); err != nil {
					return err
				}
			}
		case *pluginv1.ForwardRequest_BodyEnd:
			if !x.BodyEnd {
				return errors.New("invalid body_end")
			}
			return nil
		default:
			return errors.New("unexpected request frame")
		}
	}
}
func networkError(ctx context.Context, err error) string {
	if ctx.Err() != nil {
		return "请求已取消或超时"
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "上游连接或响应超时"
	}
	// Avoid serializing URL errors: they can contain proxy credentials.
	return "上游网络传输失败，请检查账号代理和网络连接"
}
