package basispoints

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestOnlyNonStreamingProtocolInterruptionRetriesOnce(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		var requests atomic.Int32
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if requests.Add(1) == 1 {
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				// A known framing failure, without any complete output item.
				io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nContent-Length: 100\r\n\r\n")
				conn.Close()
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write(encoded(completed(terminalText("done"))))
		}))
		s := fixtureServer()
		routeFixture(s, up.URL+"/responses")
		source := fixtureRequest()
		source["stream"] = streaming
		raw := encoded(source)
		frames := forward(t, transportClient(t, s), bpsStart(raw), raw)
		up.Close()
		if streaming {
			if requests.Load() != 1 || s.lastRequest.ProtocolRetries != 0 || frames[len(frames)-1].GetError() == nil {
				t.Fatal("stream was retried", s.lastRequest)
			}
		} else if requests.Load() != 2 || s.lastRequest.ProtocolRetries != 1 || frames[len(frames)-1].GetEnd() == nil {
			t.Fatal("non-stream retry failed", s.lastRequest)
		}
	}
}

func TestImagesHTTP200ErrorOrURLOnlyIsNotDiagnosedAsSuccess(t *testing.T) {
	for _, payload := range []object{{"error": object{"message": "no image available"}}, {"data": []any{object{"url": "https://example.invalid/image.png"}}}, {"data": []any{}}} {
		var requests atomic.Int32
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.Write(encoded(payload))
		}))
		s := fixtureServer()
		routeFixture(s, up.URL+"/responses")
		raw := []byte(`{"prompt":"example"}`)
		start := bpsStart(raw)
		start.Url = "https://chatgpt.com/backend-api/codex/images/generations"
		frames := forward(t, transportClient(t, s), start, raw)
		up.Close()
		if requests.Load() != 1 || s.lastRequest.Terminal == "images.completed" || s.lastRequest.Error == "" || s.lastRequest.GeneratedImages != 0 {
			t.Fatal("Images 200 was treated as successful or retried", s.lastRequest)
		}
		got, err := decodeObject(bodyOf(frames))
		if err != nil || digest(got) != digest(payload) {
			t.Fatal("Images body must remain available for host validation")
		}
	}
}
