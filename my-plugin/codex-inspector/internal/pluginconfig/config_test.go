package pluginconfig

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestDefaultRoundTrips(t *testing.T) {
	c := Default()
	got, err := Parse(c.Marshal())
	if err != nil || !reflect.DeepEqual(got, c) {
		t.Fatalf("default changed: %+v, %v", got, err)
	}
	want := `{"enabled":true,"timezone":{"mode":"egress_ip","custom_tz":"Asia/Singapore","default_tz":"Asia/Singapore","egress_cache_hours":24,"overrides":{}},"detect":{"task_id":"","created_at":"","account_ids":[],"models":["gpt-6-astra","gpt-5.6-sol"],"repeats":3,"concurrency":4}}`
	if string(c.Marshal()) != want {
		t.Fatalf("defaults differ from contract: %s", c.Marshal())
	}
	for _, raw := range []string{"", "  \n\t", "null", " null ", "{}"} {
		got, err := Parse([]byte(raw))
		if err != nil || !reflect.DeepEqual(got, c) {
			t.Fatalf("empty input %q: %+v, %v", raw, got, err)
		}
	}
}

func TestDefaultCollectionsAreIndependent(t *testing.T) {
	a, b := Default(), Default()
	a.Timezone.Overrides["1"] = "Asia/Tokyo"
	a.Detect.Models[0] = "changed"
	if len(b.Timezone.Overrides) != 0 || b.Detect.Models[0] != "gpt-6-astra" {
		t.Fatal("defaults share mutable collections")
	}
}

func TestParseRejects(t *testing.T) {
	cases := map[string]string{
		"unknown field":           `{"nope":1}`,
		"unknown nested field":    `{"timezone":{"nope":1}}`,
		"unknown detection field": `{"detect":{"extra":true}}`,
		"tz not whitelisted":      `{"timezone":{"custom_tz":"Asia/Shanghai"}}`,
		"default not whitelisted": `{"timezone":{"default_tz":"UTC"}}`,
		"unknown mode":            `{"timezone":{"mode":"random"}}`,
		"unknown model":           `{"detect":{"models":["gpt-4o"]}}`,
		"bad task id":             `{"detect":{"task_id":"a:b","created_at":"2026-09-24T10:00:00Z","account_ids":[1]}}`,
		"task without time":       `{"detect":{"task_id":"d1","account_ids":[1]}}`,
		"task invalid time":       `{"detect":{"task_id":"d1","created_at":"2026-09-31T10:00:00Z","account_ids":[1]}}`,
		"task without accts":      `{"detect":{"task_id":"d1","created_at":"2026-09-24T10:00:00Z","account_ids":[]}}`,
		"task without models":     `{"detect":{"task_id":"d1","created_at":"2026-09-24T10:00:00Z","account_ids":[1],"models":[]}}`,
		"bad override value":      `{"timezone":{"overrides":{"1":"Asia/Shanghai"}}}`,
		"null override value":     `{"timezone":{"overrides":{"1":null}}}`,
		"null boolean":            `{"enabled":null}`,
		"null object":             `{"timezone":null}`,
		"null detection":          `{"detect":null}`,
		"null mode":               `{"timezone":{"mode":null}}`,
		"null number":             `{"detect":{"repeats":null}}`,
		"null model":              `{"detect":{"models":[null]}}`,
		"null account":            `{"detect":{"account_ids":[null]}}`,
		"top array":               `[]`, "top scalar": `true`, "top string": `"config"`,
		"boolean type":     `{"enabled":"true"}`,
		"object type":      `{"timezone":[]}`,
		"models type":      `{"detect":{"models":{}}}`,
		"numeric model":    `{"detect":{"models":[42]}}`,
		"string account":   `{"detect":{"account_ids":["1"]}}`,
		"float account":    `{"detect":{"account_ids":[1.0]}}`,
		"zero account":     `{"detect":{"account_ids":[0]}}`,
		"negative account": `{"detect":{"account_ids":[-1]}}`,
		"overflow account": `{"detect":{"account_ids":[9223372036854775808]}}`,
		"overflow number":  `{"detect":{"repeats":999999999999999999999999}}`,
		"exponent number":  `{"detect":{"repeats":1e0}}`,
		"trailing JSON":    `{} {}`, "trailing after null": `null {}`, "malformed": `{`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(raw)); err == nil {
				t.Fatalf("accepted invalid configuration: %s", raw)
			}
		})
	}
}

func TestOverridesRequireCanonicalPositiveKeys(t *testing.T) {
	for _, key := range []string{"abc", "0", "-1", "+1", "01", "1.0", " 1", "1 ", "1e1", "9223372036854775808", ""} {
		raw, _ := json.Marshal(map[string]any{"timezone": map[string]any{"overrides": map[string]string{key: "Asia/Tokyo"}}})
		if _, err := Parse(raw); err == nil {
			t.Errorf("accepted key %q", key)
		}
	}
	if _, err := Parse([]byte(`{"timezone":{"overrides":{"9223372036854775807":"Asia/Tokyo"}}}`)); err != nil {
		t.Fatal(err)
	}
}

func TestParseBounds(t *testing.T) {
	for _, test := range []struct {
		path, key string
		min, max  int
	}{{"timezone", "egress_cache_hours", 1, 720}, {"detect", "repeats", 1, 10}, {"detect", "concurrency", 1, 16}} {
		for _, value := range []int{test.min - 1, test.min, test.max, test.max + 1} {
			raw := []byte(fmt.Sprintf(`{%q:{%q:%d}}`, test.path, test.key, value))
			_, err := Parse(raw)
			if (err != nil) != (value < test.min || value > test.max) {
				t.Errorf("%s=%d, err=%v", test.key, value, err)
			}
		}
	}
}

func TestParseNormalizesCollections(t *testing.T) {
	c, err := Parse([]byte(`{"detect":{"account_ids":[9,2,9,1,2],"models":["","gpt-6-sol","gpt-6-astra","gpt-6-sol"]}}`))
	if err != nil || !reflect.DeepEqual(c.Detect.AccountIDs, []int64{1, 2, 9}) || !reflect.DeepEqual(c.Detect.Models, []string{"gpt-6-sol", "gpt-6-astra"}) {
		t.Fatalf("normalization: %+v %v", c, err)
	}
	c, err = Parse([]byte(`{"timezone":{"overrides":null},"detect":{"account_ids":null,"models":null}}`))
	if err != nil || c.Timezone.Overrides == nil || c.Detect.AccountIDs == nil || c.Detect.Models == nil || bytes.Contains(c.Marshal(), []byte("null")) {
		t.Fatalf("null collections were not normalized: %s, %v", c.Marshal(), err)
	}
}

func TestPairLimitUsesNormalizedCounts(t *testing.T) {
	c := Default()
	for i := int64(1); i <= 100; i++ {
		c.Detect.AccountIDs = append(c.Detect.AccountIDs, i)
	}
	if _, err := Parse(c.Marshal()); err != nil {
		t.Fatal(err)
	}
	c.Detect.AccountIDs = append(c.Detect.AccountIDs, 1)
	if _, err := Parse(c.Marshal()); err != nil {
		t.Fatal("duplicates must normalize before limit", err)
	}
	c.Detect.AccountIDs = append(c.Detect.AccountIDs, 101)
	if _, err := Parse(c.Marshal()); err == nil {
		t.Fatal("accepted 202 pairs")
	}
}

func TestTaskValidation(t *testing.T) {
	for _, id := range []string{"a", strings.Repeat("a", 64), "task-123"} {
		c := Default()
		c.Detect.TaskID = id
		c.Detect.CreatedAt = "2026-09-24T10:00:00+08:00"
		c.Detect.AccountIDs = []int64{1}
		if _, err := Parse(c.Marshal()); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{strings.Repeat("a", 65), "a_b", "任务", "a/b", "a.b"} {
		c := Default()
		c.Detect.TaskID = id
		c.Detect.CreatedAt = "2026-09-24T10:00:00Z"
		c.Detect.AccountIDs = []int64{1}
		if _, err := Parse(c.Marshal()); err == nil {
			t.Errorf("accepted id %q", id)
		}
	}
}

func TestLists(t *testing.T) {
	if len(Whitelist) != 30 || len(SelectableModels) != 8 {
		t.Fatalf("list lengths %d %d", len(Whitelist), len(SelectableModels))
	}
	seen := map[string]bool{}
	for _, tz := range Whitelist {
		if seen[tz] {
			t.Errorf("duplicate timezone %s", tz)
		}
		seen[tz] = true
		if _, err := time.LoadLocation(tz); err != nil {
			t.Error(err)
		}
	}
	for _, tz := range []string{"Asia/Shanghai", "Asia/Hong_Kong", "Asia/Macau", "Asia/Taipei", "UTC"} {
		if InWhitelist(tz) {
			t.Errorf("unexpected timezone %s", tz)
		}
	}
}

func TestLegacyKeysAreRemovedBeforeStrictDecode(t *testing.T) {
	old := legacyKeys
	legacyKeys = []string{"retired"}
	defer func() { legacyKeys = old }()
	if _, err := Parse([]byte(`{"retired":{"old":true}}`)); err != nil {
		t.Fatal(err)
	}
}

func FuzzParse(f *testing.F) {
	for _, v := range []string{"{}", "null", "[]", `{"detect":{"account_ids":[9223372036854775807]}}`} {
		f.Add(v)
	}
	f.Fuzz(func(t *testing.T, s string) {
		c, err := Parse([]byte(s))
		if err != nil {
			return
		}
		got, err := Parse(c.Marshal())
		if err != nil || !reflect.DeepEqual(c, got) {
			t.Fatalf("unstable configuration: %s %v", c.Marshal(), err)
		}
	})
}
