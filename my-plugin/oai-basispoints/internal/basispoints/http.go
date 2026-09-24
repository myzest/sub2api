package basispoints

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	pluginv1 "local.sub2api/oai-basispoints/internal/pluginapi"
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
			for _, value := range v.Values {
				h.Add(k, value)
			}
		}
	}
	return h
}

func networkError(ctx context.Context, err error) string {
	if ctx.Err() != nil {
		return "请求已取消或超时"
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "上游连接或响应超时"
	}
	return "上游网络传输失败，请检查账号代理和网络连接"
}
func headersToProto(in http.Header) map[string]*pluginv1.HeaderValues {
	h := make(map[string]*pluginv1.HeaderValues, len(in))
	for k, v := range in {
		h[k] = &pluginv1.HeaderValues{Values: append([]string(nil), v...)}
	}
	return h
}
