package modeltrace

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"os"
	"reflect"
	"strings"
	"testing"

	"local.sub2api/codex-inspector/internal/pluginconfig"
)

func TestBankHashPinned(t *testing.T) {
	sum := sha256.Sum256(bankBytes)
	if got := hex.EncodeToString(sum[:]); got != BankSHA256 {
		t.Fatalf("bank hash drift: %s", got)
	}
	if err := LoadBank().validate(); err != nil {
		t.Fatal(err)
	}
}

func TestSelectableModelsInBank(t *testing.T) {
	ids := map[string]bool{}
	for _, model := range LoadBank().Models {
		ids[model.ID] = true
	}
	for _, model := range pluginconfig.SelectableModels {
		if !ids[model] {
			t.Errorf("%s not in bank", model)
		}
	}
}

func TestParseNumbers(t *testing.T) {
	for _, tc := range []struct {
		text string
		want []int
	}{
		{"1 2 abc 3 4 5", []int{3, 4, 5}},
		{"1 2 中文 3 4", []int{1, 2}},
		{"999999999999999999999999 1 2", []int{1, 2}},
		{"０１２ 0123 0 356 355", []int{123, 355}},
		{"1--2,3.4\n5", []int{1, 2, 3, 4, 5}},
		{"nothing", []int{}},
	} {
		if got := ParseNumbers(tc.text); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%q: %v != %v", tc.text, got, tc.want)
		}
	}
}

func TestAnalyzeMatchesGolden(t *testing.T) {
	raw, err := os.ReadFile("testdata/golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var golden []struct {
		Inputs      []Answer           `json:"inputs"`
		Prediction  string             `json:"prediction"`
		Probability float64            `json:"probability"`
		UsedOutputs int                `json:"used_outputs"`
		Scores      map[string]float64 `json:"scores"`
	}
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	if len(golden) != 16 {
		t.Fatalf("expected 16 cases, got %d", len(golden))
	}
	for i, g := range golden {
		got, err := LoadBank().Analyze(g.Inputs)
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		if got.Prediction != g.Prediction || got.UsedOutputs != g.UsedOutputs {
			t.Fatalf("case %d: prediction/count mismatch: %+v", i, got)
		}
		if math.Abs(got.Probability-g.Probability) > 1e-9 {
			t.Fatalf("case %d: probability drift %.12g", i, got.Probability-g.Probability)
		}
		if len(got.Scores) != len(g.Scores) {
			t.Fatalf("case %d: score count", i)
		}
		for m, score := range g.Scores {
			if math.Abs(got.Scores[m]-score) > 1e-9 {
				t.Fatalf("case %d %s: score drift %.12g", i, m, got.Scores[m]-score)
			}
		}
	}
}

func TestAnalyzeNoValidAnswer(t *testing.T) {
	if _, err := LoadBank().Analyze([]Answer{{Text: strings.Repeat("1 ", 10)}}); err == nil {
		t.Fatal("accepted insufficient numbers")
	}
	if _, err := LoadBank().Analyze([]Answer{{ExpectedCount: 300, Text: strings.Repeat("1 ", 164)}}); err == nil {
		t.Fatal("accepted truncated expected output")
	}
	result, err := LoadBank().Analyze([]Answer{{ExpectedCount: 300, Text: strings.Repeat("1 ", 165)}, {Text: "invalid"}})
	if err != nil || result.UsedOutputs != 1 {
		t.Fatalf("threshold: %+v %v", result, err)
	}
}

func TestMarginalOnlyFallback(t *testing.T) {
	for _, mode := range []string{"nil", "zero"} {
		var b Bank
		if err := json.Unmarshal(bankBytes, &b); err != nil {
			t.Fatal(err)
		}
		if mode == "nil" {
			b.Robust.OrderedBlocks = nil
		} else {
			b.Robust.OrderedBlocks.Weight = 0
		}
		result, err := b.Analyze([]Answer{{Text: strings.Repeat("1 23 355 ", 100)}})
		if err != nil || result.Prediction == "" || math.IsNaN(result.Probability) {
			t.Fatalf("%s: %+v %v", mode, result, err)
		}
	}
}
