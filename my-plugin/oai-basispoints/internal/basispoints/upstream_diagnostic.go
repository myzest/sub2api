package basispoints

import (
	"bytes"
	"io"
)

const maxErrorDiagnosticBytes = 64 << 10

// User-requested raw error diagnostics. Read only bounded upstream error
// fields; do not add request headers, conversation bodies, input or context.
func readUpstreamDiagnostic(body io.Reader) (string, string) {
	raw, err := io.ReadAll(io.LimitReader(body, maxErrorDiagnosticBytes+1))
	if err != nil {
		return "", "read_error"
	}
	if len(raw) > maxErrorDiagnosticBytes {
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
	fields, state := upstreamFailureFields(value)
	if len(fields) == 0 {
		return "", state
	}
	return string(encoded(fields)), state
}
