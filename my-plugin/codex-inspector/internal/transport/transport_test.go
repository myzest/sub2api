package transport

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestClientRoundTripCacheAndNoRedirect(t *testing.T) {
	hits := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
		w.Write([]byte("ok"))
	}))
	defer up.Close()
	p := NewPool()
	defer p.CloseIdle()
	c, e := p.Client("")
	if e != nil {
		t.Fatal(e)
	}
	c2, _ := p.Client("")
	if c != c2 {
		t.Fatal("not cached")
	}
	resp, e := c.Get(up.URL)
	if e != nil {
		t.Fatal(e)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "ok" {
		t.Fatal(string(b))
	}
	resp, e = c.Get(up.URL + "/redirect")
	if e != nil {
		t.Fatal(e)
	}
	resp.Body.Close()
	if resp.StatusCode != 302 || hits != 2 {
		t.Fatal("redirect followed")
	}
}
func TestInvalidProxyNoSecret(t *testing.T) {
	p := NewPool()
	for _, value := range []string{"ftp://user:secret@example.com", "http://user:secret@%invalid", "missing://user:secret@host"} {
		_, e := p.Client(value)
		if e == nil || strings.Contains(e.Error(), "secret") {
			t.Fatal(e)
		}
	}
}
func TestRequestSentClassification(t *testing.T) {
	for _, tc := range []struct {
		e    error
		want bool
	}{{nil, true}, {errors.New("reset after write"), true}, {&url.Error{Op: "Post", URL: "https://example.invalid", Err: &net.OpError{Op: "dial", Err: errors.New("refused")}}, false}, {fmt.Errorf("wrapped: %w", &net.DNSError{Err: "NXDOMAIN"}), false}, {&net.OpError{Op: "read", Err: io.ErrUnexpectedEOF}, true}} {
		if got := RequestSent(tc.e); got != tc.want {
			t.Fatalf("%v: %v", tc.e, got)
		}
	}
}
func TestDirectIgnoresEnvironmentProxy(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://invalid.invalid:1")
	p := NewPool()
	c, _ := p.Client("")
	if c.Transport.(*http.Transport).Proxy != nil {
		t.Fatal("ambient proxy used")
	}
}
