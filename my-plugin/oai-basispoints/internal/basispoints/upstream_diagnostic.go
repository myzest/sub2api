package basispoints

import (
	"bytes"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"
)

var diagnosticURL = regexp.MustCompile(`(?i)https?://[^\s"'<>]+`)
var diagnosticOpaque = regexp.MustCompile(`[A-Za-z0-9+/_=-]{80,}`)
var diagnosticQuoted = regexp.MustCompile(`"(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*'`)
var diagnosticProtocolValue = regexp.MustCompile(`^(?:auto|low|high|original|null|input_image|input_text|image_url|file_id|detail|model|reasoning_effort|image/(?:png|jpeg|webp|gif)|(?:input|body)(?:\[[0-9]+\]|\.[A-Za-z_][A-Za-z0-9_]*)*)$`)

// Only called for HTTP errors. Project the bounded JSON error onto validation
// fields; never retain an HTML page, rejected input, ctx or the raw error body.
func readUpstreamDiagnostic(body io.Reader, headers http.Header, rawRequest []byte, request object) (string, string) {
	raw, err := io.ReadAll(io.LimitReader(body, (64<<10)+1))
	if err != nil {
		return "", "read_error"
	}
	if len(raw) > 64<<10 {
		return "", "too_large"
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return "", "empty"
	}
	if bytes.TrimSpace(raw)[0] != '{' {
		return "", "non_json"
	}
	value, err := decodeObject(raw)
	if err != nil {
		return "", "invalid_json"
	}
	// Strip known request values before truncating error strings. Include the
	// transformed request to cover attachment file IDs and relay catalogs.
	secrets := map[string]bool{}
	var collect func(any, string)
	collect = func(value any, key string) {
		switch v := value.(type) {
		case string:
			switch key {
			case "type", "role", "detail", "model", "reasoning_effort", "effort", "status":
				return
			}
			if v != "" {
				secrets[v] = true
			}
		case object:
			if additionalTools(v) {
				return // Tool definitions are protocol, not conversation payload.
			}
			for key, child := range v {
				collect(child, key)
			}
		case []any:
			for _, child := range v {
				collect(child, key)
			}
		}
	}
	collectRequest := func(source object) {
		for _, key := range []string{"input", "instructions", "metadata", "client_metadata", "prompt_cache_key", "promptCacheKey", "session_id", "sessionId"} {
			collect(source[key], key)
		}
	}
	if source, err := decodeObject(rawRequest); err == nil {
		collectRequest(source)
	}
	collectRequest(request)
	values := make([]string, 0, len(secrets))
	for value := range secrets {
		values = append(values, value)
	}
	slices.SortFunc(values, func(a, b string) int { return len(b) - len(a) })
	detail := diagnosticFields(value, func(field, text string) string {
		if field == "message" || field == "detail" || field == "msg" {
			// Short prompts can also be echoed. Only apply content replacement
			// to prose; replacing schema strings in param/loc would hide paths.
			for _, value := range values {
				text = strings.ReplaceAll(text, value, "[REQUEST_VALUE]")
			}
			text = diagnosticQuoted.ReplaceAllStringFunc(text, func(quoted string) string {
				if diagnosticProtocolValue.MatchString(quoted[1 : len(quoted)-1]) {
					return quoted
				}
				return "[QUOTED_VALUE]"
			})
		}
		text = diagnosticURL.ReplaceAllString(text, "[URL]")
		text = diagnosticOpaque.ReplaceAllString(text, "[OPAQUE]")
		return redactProbeText(text, headers, "")
	})
	if detail == "" {
		return "", "no_fields"
	}
	return detail, "captured"
}
