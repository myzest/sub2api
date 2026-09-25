package basispoints

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAttachmentFailureDoesNotReusePreviousHTTP(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-Id", "first-upload")
		_, _ = w.Write([]byte(`{"openai_file_id":"file-fixture"}`))
	}))
	defer up.Close()
	s := New()
	report := &attachmentReport{Reused: 1}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := s.uploadImage(ctx, up.URL, http.Header{}, "", "image/png", "image.png", []byte("fixture"), "", report); err != nil || report.HTTPStatus != 200 {
		t.Fatalf("first upload: %+v, %v", report, err)
	}
	cancel()
	if _, err := s.uploadImage(ctx, up.URL, http.Header{}, "", "image/png", "image.png", []byte("fixture-two"), "", report); err == nil {
		t.Fatal("cancelled upload accepted")
	}
	if report.Attempts != 2 || report.HTTPStatus != 0 || report.RequestID != "" || report.ContentType != "" || report.Diagnostic != "" || report.DiagnosticState != "" {
		t.Fatalf("earlier HTTP response survived a new failed attempt: %+v", report)
	}
}

func TestAttachmentHTTPErrorUsesProductionRedaction(t *testing.T) {
	data := []byte("fixture pixels")
	payload := base64.StdEncoding.EncodeToString(data)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write(encoded(object{"error": object{"message": "Invalid input: .jpeg .jpg .png .gif .webp but got none. " + payload + " https://upload.invalid/private?signature=fixture 'private fragment'", "param": "input", "type": "invalid_request_error"}}))
	}))
	defer up.Close()
	report := &attachmentReport{}
	_, err := New().uploadImage(context.Background(), up.URL, http.Header{}, "", "image/png", "image.png", data, "data:image/png;base64,"+payload, report)
	if err == nil || report.HTTPStatus != 400 || report.DiagnosticState != "captured" {
		t.Fatalf("missing HTTP rejection: %+v, %v", report, err)
	}
	for _, secret := range []string{payload, "upload.invalid", "signature=fixture", "private fragment"} {
		if strings.Contains(report.Diagnostic, secret) {
			t.Fatalf("attachment error leaked %q", secret)
		}
	}
	if !strings.Contains(report.Diagnostic, "got none") || !strings.Contains(report.Diagnostic, `"param":"input"`) {
		t.Fatal("redaction removed useful validation evidence", report.Diagnostic)
	}
}
