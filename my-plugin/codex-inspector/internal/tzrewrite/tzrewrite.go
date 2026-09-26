// Package tzrewrite implements byte-preserving, fail-open request rewriting.
package tzrewrite

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
	_ "time/tzdata"
)

var timezoneTag = regexp.MustCompile(`<timezone>[^<]*</timezone>`)
var dateTag = regexp.MustCompile(`<current_date>\d{4}-\d{2}-\d{2}</current_date>`)

// Rewrite only touches input_text environment blocks belonging to user messages.
// All errors return the original bytes, letting the caller explicitly fail open.
func Rewrite(body []byte, targetTZ string, now time.Time) (out []byte, blocks int, changed bool, err error) {
	loc, err := time.LoadLocation(targetTZ)
	if err != nil {
		return body, 0, false, fmt.Errorf("load target timezone: %w", err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return body, 0, false, fmt.Errorf("decode request: %w", err)
	}
	inputRaw, ok := top["input"]
	if !ok {
		return body, 0, false, nil
	}
	var inputs []json.RawMessage
	if err := json.Unmarshal(inputRaw, &inputs); err != nil {
		return body, 0, false, fmt.Errorf("decode input: %w", err)
	}
	date := now.In(loc).Format("2006-01-02")
	for i, input := range inputs {
		var item map[string]json.RawMessage
		if err := json.Unmarshal(input, &item); err != nil {
			return body, 0, false, fmt.Errorf("decode input[%d]: %w", i, err)
		}
		var role string
		if err := json.Unmarshal(item["role"], &role); err != nil || role != "user" {
			continue
		}
		contentRaw, ok := item["content"]
		if !ok {
			continue
		}
		var contents []json.RawMessage
		if err := json.Unmarshal(contentRaw, &contents); err != nil {
			return body, 0, false, fmt.Errorf("decode input[%d].content: %w", i, err)
		}
		itemChanged := false
		for j, content := range contents {
			var part map[string]json.RawMessage
			if err := json.Unmarshal(content, &part); err != nil {
				return body, 0, false, fmt.Errorf("decode content[%d]: %w", j, err)
			}
			var kind, text string
			if err := json.Unmarshal(part["type"], &kind); err != nil || kind != "input_text" {
				continue
			}
			if err := json.Unmarshal(part["text"], &text); err != nil || !strings.HasPrefix(strings.TrimSpace(text), "<environment_context>") {
				continue
			}
			blocks++
			newText, didChange := rewriteBlock(text, targetTZ, date)
			if !didChange {
				continue
			}
			part["text"], err = encode(newText)
			if err != nil {
				return body, 0, false, err
			}
			contents[j], err = encode(part)
			if err != nil {
				return body, 0, false, err
			}
			itemChanged = true
		}
		if itemChanged {
			item["content"], err = encode(contents)
			if err != nil {
				return body, 0, false, err
			}
			inputs[i], err = encode(item)
			if err != nil {
				return body, 0, false, err
			}
			changed = true
		}
	}
	if !changed {
		return body, blocks, false, nil
	}
	top["input"], err = encode(inputs)
	if err != nil {
		return body, 0, false, err
	}
	out, err = encode(top)
	if err != nil {
		return body, 0, false, err
	}
	return out, blocks, true, nil
}

func rewriteBlock(text, targetTZ, dateStr string) (string, bool) {
	// Ignore any unrelated text or second environment block following the first
	// closing tag. Only the first timezone/date inside the matched block changes.
	end := len(text)
	if i := strings.Index(text, "</environment_context>"); i >= 0 {
		end = i
	}
	block, tail := text[:end], text[end:]
	block = replaceFirst(timezoneTag, block, "<timezone>"+targetTZ+"</timezone>")
	block = replaceFirst(dateTag, block, "<current_date>"+dateStr+"</current_date>")
	result := block + tail
	return result, result != text
}

func replaceFirst(re *regexp.Regexp, text, replacement string) string {
	idx := re.FindStringIndex(text)
	if idx == nil {
		return text
	}
	return text[:idx[0]] + replacement + text[idx[1]:]
}

func encode(value any) ([]byte, error) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte{'\n'}), nil
}
