package modeltrace

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
)

type upstreamFixture struct {
	Revision string `json:"revision"`
	Cases    []struct {
		Name     string          `json:"name"`
		Answers  []Answer        `json:"answers"`
		Expected json.RawMessage `json:"expected"`
	} `json:"cases"`
	Challenges []struct {
		Count    int `json:"count"`
		Expected []struct {
			ExpectedCount int    `json:"expected_count"`
			Prompt        string `json:"prompt"`
		} `json:"expected"`
	} `json:"challenges"`
	ParserCases []struct {
		Text     string `json:"text"`
		Expected []int  `json:"expected"`
	} `json:"parser_cases"`
}

func readFixture(t *testing.T) upstreamFixture {
	t.Helper()
	raw, err := os.ReadFile("testdata/upstream.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture upstreamFixture
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.Revision != Revision {
		t.Fatal("fixture and bank revisions differ")
	}
	return fixture
}

func compareJSON(t *testing.T, location string, got, want any) {
	t.Helper()
	switch expected := want.(type) {
	case float64:
		actual, ok := got.(float64)
		if !ok || math.IsNaN(actual) || math.Abs(actual-expected) > 1e-10 {
			t.Errorf("%s: got %v want %.17g", location, got, expected)
		}
	case map[string]any:
		actual, ok := got.(map[string]any)
		if !ok {
			t.Errorf("%s: got %T want object", location, got)
			return
		}
		if len(actual) != len(expected) {
			t.Errorf("%s: object keys differ: got %d want %d", location, len(actual), len(expected))
		}
		for key, value := range expected {
			compareJSON(t, location+"."+key, actual[key], value)
		}
	case []any:
		actual, ok := got.([]any)
		if !ok || len(actual) != len(expected) {
			t.Errorf("%s: array length/type differ", location)
			return
		}
		for i, value := range expected {
			compareJSON(t, fmt.Sprintf("%s[%d]", location, i), actual[i], value)
		}
	default:
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: got %v want %v", location, got, want)
		}
	}
}

func TestAllCandidatesMatchUpstreamJavaScript(t *testing.T) {
	fixture := readFixture(t)
	if len(fixture.Cases) != 54 {
		t.Fatal("expected 18 models x 1/2/3 answers")
	}
	bank := LoadBank()
	for _, entry := range fixture.Cases {
		t.Run(entry.Name, func(t *testing.T) {
			result, err := bank.Analyze(entry.Answers)
			if err != nil {
				t.Fatal(err)
			}
			// Additional quality warnings are separate from upstream mathematics.
			for i := range result.Diagnostics {
				result.Diagnostics[i].Warnings = nil
			}
			raw, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			var got, want any
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(entry.Expected, &want); err != nil {
				t.Fatal(err)
			}
			compareJSON(t, entry.Name, got, want)
		})
	}
}

func TestPromptTemplatesAndParserMatchUpstream(t *testing.T) {
	fixture := readFixture(t)
	for _, entry := range fixture.Challenges {
		cursor := 0
		got := GenerateChallenges(entry.Count, func(k int) int { value := cursor % k; cursor++; return value })
		for i, want := range entry.Expected {
			if got[i].ExpectedCount != want.ExpectedCount || got[i].Prompt != want.Prompt {
				t.Fatalf("challenge %d/%d template or RNG call order mismatch", entry.Count, i)
			}
		}
	}
	for _, entry := range fixture.ParserCases {
		if got := ParseNumbers(entry.Text); !slices.Equal(got, entry.Expected) {
			t.Errorf("parse %q: got %v want %v", entry.Text, got, entry.Expected)
		}
	}
}

func TestSampleWarningsDoNotChangeAttribution(t *testing.T) {
	cases := []struct {
		name    string
		numbers []string
		warning string
	}{
		{"constant", make([]string, 310), "constant_sequence"},
		{"ascending", make([]string, 310), "ascending_sequence"},
		{"descending", make([]string, 310), "descending_sequence"},
	}
	for _, tc := range cases {
		for i := range tc.numbers {
			value := 42
			if tc.name == "ascending" {
				value = i + 1
			}
			if tc.name == "descending" {
				value = 310 - i
			}
			tc.numbers[i] = strconv.Itoa(value)
		}
		answer := Answer{ExpectedCount: 310, Text: strings.Join(tc.numbers, ",")}
		inspection := Inspect(answer)
		if !inspection.Accepted || !slices.Contains(inspection.Warnings, tc.warning) {
			t.Fatalf("%s: missing separate quality warning: %+v", tc.name, inspection)
		}
		result, err := LoadBank().Analyze([]Answer{answer})
		if err != nil || result.UsedOutputs != 1 || len(result.Results) != 18 {
			t.Fatalf("quality warning unexpectedly changed upstream result: %+v %v", result, err)
		}
	}
}

func TestInsufficientAndMixedAnswers(t *testing.T) {
	valid := readFixture(t).Cases[0].Answers[0]
	invalid := Answer{ExpectedCount: 310, Text: "I cannot provide these numbers."}
	result, err := LoadBank().Analyze([]Answer{invalid})
	if err == nil || result.UsedOutputs != 0 || len(result.Diagnostics) != 1 || result.Diagnostics[0].Accepted {
		t.Fatal("missing unusable-output diagnostic", result, err)
	}
	mixed, err := LoadBank().Analyze([]Answer{invalid, valid, invalid})
	if err != nil || mixed.UsedOutputs != 1 || mixed.Calibration.Queries != "1" || mixed.Diagnostics[1].Index != 1 {
		t.Fatal("mixed answers used wrong calibration", mixed, err)
	}
	baseline, _ := LoadBank().Analyze([]Answer{valid})
	if !reflect.DeepEqual(mixed.Results, baseline.Results) {
		t.Fatal("invalid samples changed attribution")
	}
	for _, count := range []int{0, 4, 9} {
		if _, err := LoadBank().Analyze(make([]Answer, count)); err == nil {
			t.Errorf("accepted %d answers in one round", count)
		}
	}
	if d := Inspect(Answer{ExpectedCount: 310, Text: strings.Repeat("1,", 170)}); d.Accepted || d.MinimumNumbers != 171 {
		t.Fatal("wrong threshold", d)
	}
}

func TestMetadataAndCoverage(t *testing.T) {
	bank := LoadBank()
	metadata := bank.Metadata()
	if metadata.ModelCount != 18 || metadata.ReferenceCount != 648 || metadata.SHA256 != BankSHA256 || metadata.BuiltAt == "" {
		t.Fatal("bad provenance", metadata)
	}
	if !bank.Covers("gpt-6-sol") || bank.Covers("gpt-6.1-sol") || bank.Covers("gpt-6-sol-new-alias") {
		t.Fatal("model coverage must be exact")
	}
	metadata.Models[0].ID = "modified metadata"
	if bank.Models[0].ID == "modified metadata" {
		t.Fatal("metadata exposed mutable model slice")
	}
}
