package transport

import (
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"
)

type Pool struct {
	mu      sync.Mutex
	clients map[string]*http.Client
}

func NewPool() *Pool { return &Pool{clients: map[string]*http.Client{}} }

// Client caches by static account proxy URL; never pass per-request secrets.
// Empty proxy means direct (HTTP_PROXY/HTTPS_PROXY are deliberately ignored).
func (p *Pool) Client(proxyURL string) (*http.Client, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if c := p.clients[proxyURL]; c != nil {
		return c, nil
	}
	tr := &http.Transport{DialContext: (&net.Dialer{Timeout: 20 * time.Second, KeepAlive: 30 * time.Second}).DialContext, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}, TLSHandshakeTimeout: 20 * time.Second, IdleConnTimeout: 90 * time.Second, MaxIdleConns: 128, MaxIdleConnsPerHost: 32, ForceAttemptHTTP2: true, DisableCompression: true, ExpectContinueTimeout: time.Second}
	if proxyURL != "" {
		u, e := url.Parse(proxyURL)
		if e != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks5" && u.Scheme != "socks5h") {
			return nil, errors.New("invalid account proxy URL")
		}
		tr.Proxy = http.ProxyURL(u)
	}
	// Redirects must reach the host unchanged and must not replay OAuth tokens.
	c := &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if p.clients == nil {
		p.clients = map[string]*http.Client{}
	}
	p.clients[proxyURL] = c
	return c, nil
}
func (p *Pool) CloseIdle() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.clients {
		c.CloseIdleConnections()
	}
}

// RequestSent is conservative except for positively identified pre-send failures.
func RequestSent(err error) bool {
	if err == nil {
		return true
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return false
	}
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "dial" {
		return false
	}
	return true
}
