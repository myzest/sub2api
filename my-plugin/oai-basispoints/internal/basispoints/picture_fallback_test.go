package basispoints

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func fixturePicture(value string) object {
	return object{"type": "input_image", "image_url": "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte(value))}
}

func pictureRequest() object {
	source := fixtureRequest()
	source["tools"], source["stream"] = []any{}, false
	source["input"] = []any{object{"type": "message", "role": "user", "content": []any{fixturePicture("private fixture pixels")}}}
	return source
}

// A synthetic extension with an image-shaped object is deliberately not
// evidence of a supported BPS encrypted payload; the adapter must leave it alone.
func TestEncryptedPartsRemainOpaqueToPictureHandling(t *testing.T) {
	var uploads atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uploads.Add(1)
		if r.URL.Path != "/attachments" {
			t.Error("unexpected request outside attachment fixture")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"openai_file_id":"file_visible_picture"}`))
	}))
	defer up.Close()
	s := fixtureServer()
	routeFixture(s, up.URL+"/responses")
	opaque := object{"type": "encrypted_content", "encrypted_content": "opaque fixture",
		"extension": object{"nested": []any{fixturePicture("not an input picture")}}}
	source := pictureRequest()
	source["input"] = []any{object{"type": "agent_message", "content": []any{
		object{"type": "input_text", "text": "prefix"}, opaque, fixturePicture("visible picture"),
	}}}
	before := digest(source)
	if _, count := summarizeImages(source); count != 1 {
		t.Fatal("opaque extension counted as an input picture", count)
	}
	plan := mustPlan(t, s, source, 7)
	headers, _ := bpsHeaders(headersFromProto(bpsStart(encoded(source)).Headers))
	pictures := s.newPictureFallback(plan, headers, "")
	pictures.refused["agent_message"] = true
	for _, omit := range []bool{false, true} {
		pictures.omit = omit
		if err := pictures.rewrite(context.Background()); err != nil {
			t.Fatal(err)
		}
		items := plan.body["input"].([]any)
		parts := items[len(items)-1].(object)["content"].([]any)
		if digest(parts[1]) != digest(opaque) || digest(source) != before || uploads.Load() != 1 {
			t.Fatal("opaque content changed or triggered attachment handling")
		}
		images, count := summarizeImages(plan.body)
		if omit {
			if pictures.omitted != 1 || count != 0 {
				t.Fatal("image fallback interpreted opaque contents")
			}
		} else if count != 1 || images[0].Source != "file_id" {
			t.Fatal("ordinary sibling image no longer follows attachment handling")
		}
	}
}

func TestPictureRefusalRefreshesOnlyOldCacheThenExplicitlyOmits(t *testing.T) {
	for _, status := range []int{400, 422} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var uploads, requests atomic.Int32
			var identity string
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/attachments" {
					w.Write(encoded(object{"openai_file_id": fmt.Sprintf("file_%d", uploads.Add(1))}))
					return
				}
				attempt := requests.Add(1)
				data, _ := io.ReadAll(r.Body)
				body, err := decodeObject(data)
				if err != nil {
					t.Error(err)
					w.WriteHeader(500)
					return
				}
				if attempt == 1 {
					identity = digest(body["metadata"])
				} else if identity != digest(body["metadata"]) {
					t.Error("retry changed turn metadata")
				}
				if attempt < 3 {
					w.Header().Set("X-Request-Id", fmt.Sprintf("refused-%d", attempt))
					w.WriteHeader(status)
					w.Write([]byte(`{"error":{"message":"Invalid input: unsupported image format","param":"input"}}`))
					return
				}
				if _, count := summarizeImages(body); count != 0 || !strings.Contains(string(data), "[image content omitted:") {
					t.Error("last attempt silently lost its picture")
				}
				w.Write(encoded(completed(terminalText("picture unavailable"))))
			}))
			defer up.Close()
			s := fixtureServer()
			routeFixture(s, up.URL+"/responses")
			source := pictureRequest()
			raw := encoded(source)
			headers, _ := bpsHeaders(headersFromProto(bpsStart(raw).Headers))
			prior := mustPlan(t, s, source, 7)
			if _, err := s.uploadInputImages(context.Background(), prior, headers, ""); err != nil {
				t.Fatal(err)
			}
			frames := forward(t, transportClient(t, s), bpsStart(raw), raw)
			d := s.lastRequest
			if uploads.Load() != 2 || requests.Load() != 3 || d.OmittedImages != 1 || len(d.ResponseRetries) != 2 || d.ResponseRetries[0].HTTPStatus != status || d.ResponseRetries[0].RequestID != "refused-1" || d.Terminal != "response.completed" || frames[0].GetStart().StatusCode != 200 {
				t.Fatalf("invalid retry boundaries: uploads=%d requests=%d diagnostic=%+v", uploads.Load(), requests.Load(), d)
			}
			if strings.Contains(string(encoded(d)), "private fixture pixels") || strings.Contains(string(encoded(d)), "file_1") {
				t.Fatal("diagnostic leaked picture data or id")
			}
		})
	}
}

func TestNestedToolPicturesLearnAttachmentTransport(t *testing.T) {
	var uploads, requests atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/attachments" {
			uploads.Add(1)
			w.Write([]byte(`{"openai_file_id":"file_tool_picture"}`))
			return
		}
		raw, _ := io.ReadAll(r.Body)
		body, _ := decodeObject(raw)
		images, count := summarizeImages(body)
		attempt := requests.Add(1)
		if count != 1 {
			t.Error("nested picture missing from diagnostics", count)
		}
		if attempt == 1 {
			if images[0].Source != "data_url" || uploads.Load() != 0 {
				t.Error("tool picture not initially inline")
			}
			w.WriteHeader(400)
			w.Write([]byte(`{"error":{"message":"Invalid input image","param":"input"}}`))
			return
		}
		if images[0].Source != "file_id" {
			t.Error("refused kind did not use attachments")
		}
		w.Write(encoded(completed(terminalText("seen"))))
	}))
	defer up.Close()
	s := fixtureServer()
	routeFixture(s, up.URL+"/responses")
	source := pictureRequest()
	source["input"] = []any{message("user", "look"), object{"type": "function_call", "call_id": "old_call", "name": "view", "arguments": "{}"}, object{"type": "function_call_output", "call_id": "old_call", "output": object{"private key": []any{fixturePicture("tool image")}}}}
	raw := encoded(source)
	client := transportClient(t, s)
	forward(t, client, bpsStart(raw), raw)
	if uploads.Load() != 1 || requests.Load() != 2 || s.lastRequest.InputImages != 1 || s.lastRequest.OmittedImages != 0 || len(s.imageRequests) != 1 {
		t.Fatal("nested picture fallback failed", s.lastRequest)
	}
	if strings.Contains(string(encoded(s.lastRequest)), "private key") {
		t.Fatal("arbitrary output key leaked into image path")
	}
	forward(t, client, bpsStart(raw), raw)
	if uploads.Load() != 1 || requests.Load() != 3 {
		t.Fatal("learned kind or cache lost")
	}
}

func TestUploadUnavailableStillUsesCachedPictures(t *testing.T) {
	var attempts atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			w.Write([]byte(`{"openai_file_id":"file_cached"}`))
			return
		}
		connection, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		connection.Close()
	}))
	defer up.Close()
	s := fixtureServer()
	routeFixture(s, up.URL+"/responses")
	source := pictureRequest()
	headers, _ := bpsHeaders(headersFromProto(bpsStart(encoded(source)).Headers))
	plan := mustPlan(t, s, source, 7)
	if _, err := s.uploadInputImages(context.Background(), plan, headers, ""); err != nil {
		t.Fatal(err)
	}
	source["input"].([]any)[0].(object)["content"] = []any{fixturePicture("new failing image"), fixturePicture("private fixture pixels"), fixturePicture("never uploaded")}
	plan = mustPlan(t, s, source, 7)
	pictures := s.newPictureFallback(plan, headers, "")
	if err := pictures.rewrite(context.Background()); err != nil {
		t.Fatal(err)
	}
	if attempts.Load() != 2 || pictures.omitted != 2 || pictures.report.Reused != 1 {
		t.Fatal("unavailable uploads prevented reuse or kept uploading", attempts.Load(), pictures.report)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := pictures.rewrite(ctx); err == nil {
		t.Fatal("canceled upload walk continued")
	}
}

func TestPicturesDoNotRetryOtherHTTPStatuses(t *testing.T) {
	for _, status := range []int{401, 403, 429, 500, 503} {
		var responses atomic.Int32
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/attachments" {
				w.Write([]byte(`{"openai_file_id":"file_fresh"}`))
				return
			}
			responses.Add(1)
			w.WriteHeader(status)
			w.Write([]byte(`{"error":{"message":"refused"}}`))
		}))
		s := fixtureServer()
		routeFixture(s, up.URL+"/responses")
		raw := encoded(pictureRequest())
		frames := forward(t, transportClient(t, s), bpsStart(raw), raw)
		up.Close()
		if responses.Load() != 1 || len(s.lastRequest.ResponseRetries) != 0 || int(frames[0].GetStart().StatusCode) != status {
			t.Fatal("unexpected retry", status)
		}
	}
}

func TestInterruptedUploadResponseStopsFurtherNewUploads(t *testing.T) {
	for _, status := range []int{200, 400} {
		var attempts atomic.Int32
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			attempts.Add(1)
			connection, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			fmt.Fprintf(connection, "HTTP/1.1 %d %s\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n{", status, http.StatusText(status))
			connection.Close()
		}))
		s := fixtureServer()
		routeFixture(s, up.URL+"/responses")
		source := pictureRequest()
		source["input"].([]any)[0].(object)["content"] = []any{fixturePicture("first"), fixturePicture("second")}
		pictures := s.newPictureFallback(mustPlan(t, s, source, 7), http.Header{}, "")
		err := pictures.rewrite(context.Background())
		up.Close()
		if err != nil || attempts.Load() != 1 || pictures.omitted != 2 || pictures.report.HTTPStatus != status || pictures.report.DiagnosticState != "read_error" {
			t.Fatal("partial upload body did not stop new uploads", err, pictures.report)
		}
	}
}
