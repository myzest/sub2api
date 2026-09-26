package detect

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"google.golang.org/grpc"
	pluginv1 "local.sub2api/codex-inspector/internal/pluginapi/v1"
)

const CodexResponsesURL = "https://chatgpt.com/backend-api/codex/responses"
const AstraMinVersion = "0.153.4"
const codexOriginator = "codex-tui"
const maxSSEBytes = 4 * 1024 * 1024

var codexUASeg = regexp.MustCompile("(?i)(codex-tui|codex_cli_rs|codex-desktop)/([0-9]+(?:[.][0-9]+){1,3})")

type HostAPI interface {
	ListAccounts(context.Context, *pluginv1.ListAccountsRequest, ...grpc.CallOption) (*pluginv1.ListAccountsResponse, error)
	ResolveOutboundIdentity(context.Context, *pluginv1.ResolveOutboundIdentityRequest, ...grpc.CallOption) (*pluginv1.ResolveOutboundIdentityResponse, error)
}

// Identity is ephemeral. Never persist or log it, its token, or proxy URL.
type Identity struct {
	Token, ProxyURL string
	Headers         http.Header
}

func identityFromResponse(response *pluginv1.ResolveOutboundIdentityResponse) Identity {
	id := Identity{Headers: make(http.Header)}
	if response == nil {
		return id
	}
	id.Token, id.ProxyURL = response.Token, response.ProxyUrl
	for key, values := range response.Headers {
		if values != nil {
			id.Headers[http.CanonicalHeaderKey(key)] = append([]string(nil), values.Values...)
		}
	}
	return id
}

func compareVersions(left, right string) int {
	a, b := strings.Split(strings.TrimPrefix(left, "v"), "."), strings.Split(strings.TrimPrefix(right, "v"), ".")
	for i := range max(len(a), len(b)) {
		x, y := 0, 0
		if i < len(a) {
			x, _ = strconv.Atoi(a[i])
		}
		if i < len(b) {
			y, _ = strconv.Atoi(b[i])
		}
		if x < y {
			return -1
		}
		if x > y {
			return 1
		}
	}
	return 0
}

func needsAstraVersion(model string) bool {
	return model == "gpt-6-astra" || strings.HasPrefix(model, "gpt-6-astra-")
}

func buildProbeRequest(ctx context.Context, url string, id Identity, model, prompt string) (*http.Request, error) {
	if id.Token == "" {
		return nil, errors.New("identity token unavailable")
	}
	body := struct {
		Model  string           `json:"model"`
		Store  bool             `json:"store"`
		Stream bool             `json:"stream"`
		Input  []map[string]any `json:"input"`
	}{Model: model, Stream: true, Input: []map[string]any{{"role": "user", "content": []map[string]string{{"type": "input_text", "text": prompt}}}}}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, errors.New("probe encoding failed")
	}
	if url == "" {
		url = CodexResponsesURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return nil, errors.New("invalid probe URL")
	}
	// Copy only outbound identity headers, excluding connection state, cookies,
	// compression, or headers belonging to an earlier user request.
	for _, key := range []string{"User-Agent", "Version", "Originator", "Chatgpt-Account-Id", "Openai-Beta", "Accept-Language"} {
		if values := id.Headers.Values(key); len(values) > 0 {
			req.Header[key] = append([]string(nil), values...)
		}
	}
	req.Header.Set("Authorization", "Bearer "+id.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Accept-Encoding", "identity")
	if req.Header.Get("Originator") == "" {
		req.Header.Set("Originator", codexOriginator)
	}
	if needsAstraVersion(model) {
		version := req.Header.Get("Version")
		if compareVersions(version, AstraMinVersion) < 0 {
			req.Header.Set("Version", AstraMinVersion)
		}
		ua := req.Header.Get("User-Agent")
		if !codexUASeg.MatchString(ua) {
			ua = codexOriginator + "/" + req.Header.Get("Version")
		} else {
			ua = codexUASeg.ReplaceAllStringFunc(ua, func(segment string) string {
				parts := strings.SplitN(segment, "/", 2)
				if compareVersions(parts[1], AstraMinVersion) < 0 {
					return parts[0] + "/" + AstraMinVersion
				}
				return segment
			})
		}
		req.Header.Set("User-Agent", ua)
	}
	return req, nil
}

type countingReader struct {
	io.Reader
	n int
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.n += n
	return n, err
}

// parseSSE accepts only a completed Responses stream and never reads beyond 4MiB+1.
// Multiple data lines, CRLF, comments, fallback output, malformed JSON and
// truncated events are handled without retaining the upstream body anywhere.
func parseSSE(reader io.Reader) (string, bool) {
	r := &countingReader{Reader: io.LimitReader(reader, maxSSEBytes+1)}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 4096), maxSSEBytes+1)
	var text strings.Builder
	var data []string
	process := func() (bool, bool) {
		if len(data) == 0 {
			return false, true
		}
		payload := strings.Join(data, "\n")
		data = nil
		if payload == "[DONE]" {
			return false, true
		}
		var event struct {
			Type     string `json:"type"`
			Delta    string `json:"delta"`
			Response struct {
				Status string `json:"status"`
				Output []struct {
					Content []struct{ Type, Text string } `json:"content"`
				} `json:"output"`
			} `json:"response"`
		}
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			return false, false
		}
		switch event.Type {
		case "response.output_text.delta":
			text.WriteString(event.Delta)
		case "response.failed", "response.incomplete", "error":
			return false, false
		case "response.completed":
			if event.Response.Status != "" && event.Response.Status != "completed" {
				return false, false
			}
			if text.Len() == 0 {
				for _, output := range event.Response.Output {
					for _, content := range output.Content {
						if content.Type == "output_text" {
							text.WriteString(content.Text)
						}
					}
				}
			}
			return true, true
		}
		return false, true
	}
	for scanner.Scan() {
		if r.n > maxSSEBytes {
			return "", false
		}
		line := scanner.Text()
		if line == "" {
			completed, ok := process()
			if !ok {
				return "", false
			}
			if completed {
				return text.String(), true
			}
		} else if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimPrefix(line[5:], " "))
		}
	}
	if scanner.Err() != nil || r.n > maxSSEBytes {
		return "", false
	}
	completed, ok := process()
	if completed && ok {
		return text.String(), true
	}
	return "", false
}
