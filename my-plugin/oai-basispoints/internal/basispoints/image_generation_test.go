package basispoints

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// Fixtures follow excel-codex-bridge 66c41df tests/test_image_generation.py.
// These tests never use a real OAuth identity or image generation service.
func TestImagesGenerationReferenceWireThroughForward(t *testing.T) {
	answer := object{"created": json.Number("1"), "background": "opaque", "data": []any{object{"b64_json": "iVBORw0KGgo="}}, "usage": object{"input_tokens": json.Number("9007199254740993")}}
	var requests atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path != "/basispoints/api/images/generations" || r.Header.Get("Accept") != "application/json" || r.Header.Get("Authorization") != "Bearer fixture-secret-token" || r.Header.Get("Chatgpt-Account-Id") != "fixture-account" || r.Header.Get("X-Openai-Actor-Authorization") != "" {
			t.Error("image endpoint or identity differs from reference")
		}
		raw, _ := io.ReadAll(r.Body)
		body, err := decodeObject(raw)
		expected := object{"prompt": "a blue whale", "background": "opaque", "model": "gpt-image-2", "output_format": "png", "quality": "auto", "size": "auto"}
		if err != nil || digest(body) != digest(expected) {
			t.Errorf("reference generation body mismatch: %s, %v", raw, err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(encoded(answer))
	}))
	defer up.Close()
	s := fixtureServer()
	routeFixture(s, up.URL+"/basispoints/api/responses")
	raw := []byte(`{"prompt":"a blue whale","background":"opaque","model":"gpt-image-2","quality":"auto","size":"auto"}`)
	start := bpsStart(raw)
	start.Url = "https://chatgpt.com/backend-api/codex/images/generations"
	h := headersFromProto(start.Headers)
	h.Set("X-Openai-Actor-Authorization", "excel-codex-bridge")
	start.Headers = headersToProto(h)
	frames := forward(t, transportClient(t, s), start, raw)
	got, err := decodeObject(bodyOf(frames))
	if err != nil || digest(got) != digest(answer) || requests.Load() != 1 || frames[0].GetStart().StatusCode != 200 || frames[len(frames)-1].GetEnd() == nil {
		t.Fatalf("image response not preserved: %s, %v", bodyOf(frames), err)
	}
	if s.lastRequest.ImageOperation != "generations" || s.lastRequest.GeneratedImages != 1 || !s.lastRequest.UpstreamStarted || s.lastRequest.ResponsesStarted || len(s.imageRequests) != 1 {
		t.Fatalf("generation not independently diagnosed: %+v", s.lastRequest)
	}
	if strings.Contains(string(encoded(s.lastRequest)), "a blue whale") || strings.Contains(string(encoded(s.lastRequest)), "iVBORw0KGgo=") {
		t.Fatal("image diagnostic contains prompt or output pixels")
	}
}

func TestImagesEditsReferenceMultipart(t *testing.T) {
	for _, count := range []int{1, 2} {
		pixels := []byte("\x89PNG\r\n\x1a\n fixture pixels")
		pictures := make([]any, count)
		for i := range pictures {
			pictures[i] = object{"image_url": "data:image/png;base64," + base64.StdEncoding.EncodeToString(pixels)}
		}
		d := &requestDiagnostic{}
		body, contentType, err := imageGenerationBody(object{"prompt": "change the picture", "images": pictures}, "edits", d)
		if err != nil {
			t.Fatal(err)
		}
		_, params, _ := mime.ParseMediaType(contentType)
		reader := multipart.NewReader(bytes.NewReader(body), params["boundary"])
		fields := map[string]string{}
		files := 0
		for {
			part, err := reader.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := io.ReadAll(part)
			if part.FileName() == "" {
				fields[part.FormName()] = string(raw)
				continue
			}
			files++
			expected := "image"
			if count > 1 {
				expected = "image[]"
			}
			if part.FormName() != expected || !bytes.Equal(raw, pixels) || part.Header.Get("Content-Type") != "image/png" || !strings.HasSuffix(part.FileName(), ".png") {
				t.Fatal("edit file does not match the reference multipart contract")
			}
		}
		if files != count || fields["model"] != "gpt-image-2" || fields["output_format"] != "png" || fields["size"] != "auto" || fields["prompt"] != "change the picture" || len(d.UpstreamImages) != count {
			t.Fatal("missing edit fields or diagnostics", fields, files)
		}
	}
}

func TestImagesRefuseUnsupportedParametersBeforeSending(t *testing.T) {
	var requests atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1) }))
	defer up.Close()
	s := fixtureServer()
	routeFixture(s, up.URL+"/responses")
	client := transportClient(t, s)
	for _, change := range []object{
		{"background": "transparent"}, {"model": "gpt-image-1"}, {"output_format": "webp"}, {"quality": "ultra"}, {"size": "1792x1024"},
		{"n": true}, {"n": json.Number("1.5")}, {"n": json.Number("4")}, {"prompt": " "}, {"stream": true}, {"mask": object{"image_url": "data:image/png;base64,AAAA"}},
	} {
		source := object{"prompt": "fixture"}
		for key, value := range change {
			source[key] = value
		}
		raw := encoded(source)
		start := bpsStart(raw)
		start.Url = "https://chatgpt.com/backend-api/codex/images/generations"
		frames := forward(t, client, start, raw)
		if frames[0].GetStart().StatusCode != 400 || s.lastRequest.UpstreamStarted {
			t.Fatalf("unsupported request accepted: %v", change)
		}
	}
	if requests.Load() != 0 {
		t.Fatal("invalid images request reached upstream")
	}
	for _, value := range []any{nil, []any{}, []any{object{"image_url": "https://example.invalid/a.png"}}, []any{object{"file_id": "file-fixture"}}} {
		if _, _, err := imageGenerationBody(object{"prompt": "edit", "images": value}, "edits", &requestDiagnostic{}); err == nil {
			t.Fatal("unsupported edit input accepted")
		}
	}
}

func TestImagesErrorsPreserveStatusAndRequestedRawMessage(t *testing.T) {
	var requests atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Retry-After", "60")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(429)
		w.Write([]byte(`{"error":{"message":"image limit reached for private picture prompt","type":"rate_limit_error","param":"prompt"}}`))
	}))
	defer up.Close()
	s := fixtureServer()
	routeFixture(s, up.URL+"/responses")
	raw := []byte(`{"prompt":"private picture prompt"}`)
	start := bpsStart(raw)
	start.Url = "https://chatgpt.com/backend-api/codex/images/generations"
	frames := forward(t, transportClient(t, s), start, raw)
	if requests.Load() != 1 || frames[0].GetStart().StatusCode != 429 || headersFromProto(frames[0].GetStart().Headers).Get("Retry-After") != "60" {
		t.Fatal("error status, retry header or no-retry contract changed")
	}
	if !strings.Contains(s.lastRequest.UpstreamError, "image limit reached for private picture prompt") || s.lastRequest.UpstreamErrorState != "captured_raw" || s.lastRequest.ErrorSource != "upstream_http" {
		t.Fatal("requested raw image error missing", s.lastRequest.UpstreamError)
	}
}
