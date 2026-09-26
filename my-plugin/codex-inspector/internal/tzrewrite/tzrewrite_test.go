package tzrewrite

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

var fixedNow = time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)

func request(t *testing.T, texts ...string) []byte {
	t.Helper()
	contents := make([]any, 0, len(texts))
	for _, text := range texts {
		contents = append(contents, map[string]any{"type": "input_text", "text": text})
	}
	body, err := json.Marshal(map[string]any{"input": []any{map[string]any{"role": "user", "content": contents}}})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func firstText(t *testing.T, body []byte) string {
	t.Helper()
	var v struct {
		Input []struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"input"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatal(err)
	}
	return v.Input[0].Content[0].Text
}

func TestRewriteReplacesTimezoneAndDate(t *testing.T) {
	body := request(t, "<environment_context>\n  <timezone>Asia/Shanghai</timezone>\n  <current_date>2026-09-25</current_date>\n</environment_context>")
	out, blocks, changed, err := Rewrite(body, "America/Los_Angeles", fixedNow)
	if err != nil || !changed || blocks != 1 {
		t.Fatalf("got %d %v %v", blocks, changed, err)
	}
	for _, want := range []string{"<timezone>America/Los_Angeles</timezone>", "<current_date>2026-09-24</current_date>"} {
		if !bytes.Contains(out, []byte(want)) {
			t.Fatalf("missing %s: %s", want, out)
		}
	}
}

func TestRewriteHandlesHostEscapedBody(t *testing.T) {
	body := request(t, "<environment_context><timezone>Asia/Tokyo</timezone><current_date>2026-09-24</current_date></environment_context>")
	if !bytes.Contains(body, []byte(`\u003c`)) {
		t.Fatal("test must reproduce WS json.Marshal escaping")
	}
	out, blocks, changed, err := Rewrite(body, "America/Los_Angeles", fixedNow)
	if err != nil || !changed || blocks != 1 || bytes.Contains(out, []byte(`\u003c`)) || !bytes.Contains(out, []byte("<timezone>America/Los_Angeles</timezone>")) {
		t.Fatalf("escaped body rewrite: %s %v", out, err)
	}
}

func TestRewriteSelfClosingDateUntouchedButTZReplaced(t *testing.T) {
	block := `<environment_context><timezone>Asia/Tokyo</timezone><current_date status="unavailable" /></environment_context>`
	out, _, changed, err := Rewrite(request(t, block), "Europe/Paris", fixedNow)
	want := strings.Replace(block, "Asia/Tokyo", "Europe/Paris", 1)
	if err != nil || !changed || firstText(t, out) != want {
		t.Fatalf("self-closing date changed: %s %v", out, err)
	}
}

func TestRewriteNoMatchReturnsOriginalBytes(t *testing.T) {
	for _, body := range [][]byte{
		[]byte(` { "input" : [ {"role":"assistant","content":[]} ], "unused" : 1e+003 } `),
		[]byte(`{"input":[null,{"role":"user","content":[null]}]}`),
		[]byte(`null`), []byte(`{}`), []byte(`{"input":null}`), request(t, "just text"),
		request(t, `<environment_context><current_date status="unavailable" /></environment_context>`),
		request(t, "<environment_context><timezone>Asia/Tokyo</timezone><current_date>2026-09-24</current_date></environment_context>"),
	} {
		out, _, changed, err := Rewrite(body, "Asia/Tokyo", fixedNow)
		if err != nil || changed || !bytes.Equal(out, body) {
			t.Fatalf("not byte-preserving: %s %v", out, err)
		}
	}
}

func TestRewriteOnlyGenuineUserEnvironmentTexts(t *testing.T) {
	block := "<environment_context><timezone>Asia/Tokyo</timezone></environment_context>"
	for _, tc := range []struct{ role, kind, text string }{
		{"assistant", "input_text", block}, {"system", "input_text", block}, {"developer", "input_text", block}, {"tool", "input_text", block},
		{"User", "input_text", block}, {"user", "output_text", block}, {"user", "input_image", block},
		{"user", "input_text", "quoted " + block}, {"user", "input_text", "```\n" + block}, {"user", "input_text", `<environment_context extra="yes">` + block},
	} {
		body, _ := json.Marshal(map[string]any{"input": []any{map[string]any{"role": tc.role, "content": []any{map[string]any{"type": tc.kind, "text": tc.text}}}}})
		out, blocks, changed, err := Rewrite(body, "Europe/Paris", fixedNow)
		if err != nil || changed || blocks != 0 || !bytes.Equal(body, out) {
			t.Fatalf("rewrote nonmatching content %+v: %s %v", tc, out, err)
		}
	}
	out, blocks, changed, err := Rewrite(request(t, " \n\t"+block), "Europe/Paris", fixedNow)
	if err != nil || !changed || blocks != 1 || !strings.HasPrefix(firstText(t, out), " \n\t") {
		t.Fatalf("leading whitespace: %s %v", out, err)
	}
}

func TestRewriteFirstTagsOnlyAndStayInsideBlock(t *testing.T) {
	block := "<environment_context><timezone>Asia/Tokyo</timezone><timezone>Asia/Seoul</timezone><current_date>2000-01-01</current_date><current_date>2001-01-01</current_date></environment_context><timezone>outside</timezone>"
	out, _, _, err := Rewrite(request(t, block), "Europe/Paris", fixedNow)
	want := strings.Replace(strings.Replace(block, "Asia/Tokyo", "Europe/Paris", 1), "2000-01-01", "2026-09-24", 1)
	if err != nil || firstText(t, out) != want {
		t.Fatalf("first-tag replacement: %s %v", out, err)
	}
	outside := "<environment_context></environment_context><timezone>Asia/Tokyo</timezone><current_date>2000-01-01</current_date>"
	body := request(t, outside)
	out, blocks, changed, err := Rewrite(body, "Europe/Paris", fixedNow)
	if err != nil || blocks != 1 || changed || !bytes.Equal(out, body) {
		t.Fatalf("outside block changed: %s %v", out, err)
	}
}

func TestRewritePreservesUndeclaredKeysAndNumberLexemes(t *testing.T) {
	body := []byte(`{"model":"gpt-6-astra","huge":1e4000,"format":{"value":9007199254740993},"input":[{"type":"message","id":"m1","state":{"x":true},"role":"user","content":[{"type":"input_text","extra":{"n":1.00},"text":"<environment_context><timezone>Asia/Tokyo</timezone></environment_context>"}]}]}`)
	out, blocks, changed, err := Rewrite(body, "Europe/Paris", fixedNow)
	if err != nil || !changed || blocks != 1 {
		t.Fatalf("rewrite failed: %v", err)
	}
	var before, after map[string]json.RawMessage
	_ = json.Unmarshal(body, &before)
	_ = json.Unmarshal(out, &after)
	for _, key := range []string{"model", "huge", "format"} {
		if !bytes.Equal(before[key], after[key]) {
			t.Errorf("lost top key %s", key)
		}
	}
	for _, s := range []string{`"type":"message"`, `"id":"m1"`, `"state":{"x":true}`, `"extra":{"n":1.00}`} {
		if !bytes.Contains(out, []byte(s)) {
			t.Errorf("lost %s: %s", s, out)
		}
	}
}

func TestRewriteMultipleBlocksAndNoopCount(t *testing.T) {
	b1 := "<environment_context><timezone>Asia/Tokyo</timezone></environment_context>"
	b2 := "<environment_context><timezone>Europe/Paris</timezone></environment_context>"
	out, blocks, changed, err := Rewrite(request(t, b1, b2), "Europe/Paris", fixedNow)
	if err != nil || blocks != 2 || !changed || bytes.Count(out, []byte("Europe/Paris")) != 2 {
		t.Fatalf("multiple blocks: %s %d %v %v", out, blocks, changed, err)
	}
}

func TestRewriteDateDSTAndCrossDay(t *testing.T) {
	for _, tc := range []struct{ instant, tz, date string }{
		{"2026-09-24T01:00:00Z", "America/Los_Angeles", "2026-09-23"},
		{"2026-09-24T23:00:00Z", "Asia/Tokyo", "2026-09-25"},
		{"2026-03-08T07:30:00Z", "America/Los_Angeles", "2026-03-07"},
		{"2026-03-09T07:30:00Z", "America/Los_Angeles", "2026-03-09"},
		{"2026-11-01T07:30:00Z", "America/Los_Angeles", "2026-11-01"},
		{"2026-11-02T07:30:00Z", "America/Los_Angeles", "2026-11-01"},
	} {
		now, _ := time.Parse(time.RFC3339, tc.instant)
		out, _, changed, err := Rewrite(request(t, "<environment_context><current_date>2000-01-01</current_date></environment_context>"), tc.tz, now)
		if err != nil || !changed || !bytes.Contains(out, []byte(tc.date)) {
			t.Errorf("%+v: %s %v", tc, out, err)
		}
	}
}

func TestRewriteInvalidJSONReturnsOriginalAndError(t *testing.T) {
	for _, s := range []string{"not json", "{} {}", "null {}", "[]", `{"input":{}}`, `{"input":[1]}`, `{"input":[{"role":"user","content":{}}]}`, `{"input":[{"role":"user","content":[1]}]}`} {
		body := []byte(s)
		out, _, changed, err := Rewrite(body, "Asia/Tokyo", fixedNow)
		if err == nil || changed || !bytes.Equal(body, out) {
			t.Fatalf("must fail open for %s: %s %v", body, out, err)
		}
	}
	body := request(t, "<environment_context></environment_context>")
	out, _, changed, err := Rewrite(body, "No/Such_Zone", fixedNow)
	if err == nil || changed || !bytes.Equal(body, out) {
		t.Fatal("invalid timezone must preserve body and error")
	}
}

func TestRewriteWrongRoleTypeCannotMatch(t *testing.T) {
	for _, role := range []any{nil, 42, true, []string{"user"}} {
		body, _ := json.Marshal(map[string]any{"input": []any{map[string]any{"role": role, "content": []any{map[string]any{"type": "input_text", "text": "<environment_context><timezone>Asia/Tokyo</timezone></environment_context>"}}}}})
		out, blocks, changed, err := Rewrite(body, "Europe/Paris", fixedNow)
		if err != nil || blocks != 0 || changed || !bytes.Equal(body, out) {
			t.Fatalf("role type matched: %s %v", out, err)
		}
	}
}

func FuzzRewrite(f *testing.F) {
	for _, s := range []string{`{}`, `null`, `{"input":[]}`, `{"input":[{"role":"user","content":[{"type":"input_text","text":"<environment_context><timezone>UTC</timezone></environment_context>"}]}]}`} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		body := []byte(s)
		out, _, changed, err := Rewrite(body, "Asia/Tokyo", fixedNow)
		if (err != nil || !changed) && !bytes.Equal(out, body) {
			t.Fatal("non-changing path altered bytes")
		}
		if changed && (err != nil || !json.Valid(out)) {
			t.Fatal("changed body is invalid")
		}
	})
}
