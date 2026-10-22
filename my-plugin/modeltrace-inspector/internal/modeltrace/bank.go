// Package modeltrace ports ModelTrace's pinned MIT-licensed fingerprint core.
// See LICENSE.ModelTrace and provenance.json for its source and checksums.
package modeltrace

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"sync"
)

const (
	Revision   = "6cd4a8ff0f3f4e49d6d6969dd2fbadfa1569fde2"
	BankSHA256 = "f267d0513aa06981d024ec5394746fceddf44e4e8a75fa6bf8304255f4b32d6d"
)

//go:embed unified_bank.json
var bankBytes []byte

type Model struct {
	ModelInfo
	Counts []float64 `json:"counts"`
}

type Artifact struct {
	FeatureMean          []float64     `json:"feature_mean"`
	FeatureScale         []float64     `json:"feature_scale"`
	NuisanceBasis        [][]float64   `json:"nuisance_basis"`
	Centroids            [][]float64   `json:"centroids"`
	EnvironmentCentroids [][][]float64 `json:"environment_centroids"`
	Weight               float64       `json:"weight"`
}

type Bank struct {
	BuiltAt string  `json:"built_at"`
	Models  []Model `json:"models"`
	Robust  struct {
		ModelOrder    []string  `json:"model_order"`
		Hellinger     Artifact  `json:"hellinger"`
		OrderedBlocks *Artifact `json:"ordered_blocks"`
	} `json:"robust"`
	Calibration map[string]Calibration `json:"calibration"`
}

var load = sync.OnceValue(func() *Bank {
	sum := sha256.Sum256(bankBytes)
	if hex.EncodeToString(sum[:]) != BankSHA256 {
		panic("modeltrace: embedded bank hash mismatch")
	}
	var b Bank
	if err := json.Unmarshal(bankBytes, &b); err != nil {
		panic("modeltrace: invalid embedded bank")
	}
	if err := b.validate(); err != nil {
		panic(err)
	}
	return &b
})

// LoadBank returns the shared immutable embedded bank. Callers must not mutate it.
func LoadBank() *Bank { return load() }

func (b *Bank) Metadata() Metadata {
	m := Metadata{Revision: Revision, BuiltAt: b.BuiltAt, SHA256: BankSHA256, ModelCount: len(b.Models), Models: make([]ModelInfo, 0, len(b.Models))}
	for _, model := range b.Models {
		m.Models = append(m.Models, model.ModelInfo)
		m.ReferenceCount += model.ResponseCount
	}
	return m
}

// Covers uses exact identifiers; a new model or alias must not silently match
// an older model's fingerprint merely because its name has a common prefix.
func (b *Bank) Covers(modelID string) bool {
	for _, model := range b.Models {
		if model.ID == modelID {
			return true
		}
	}
	return false
}

func finite(x float64) bool { return !math.IsNaN(x) && !math.IsInf(x, 0) }

func (b *Bank) validate() error {
	bad := errors.New("modeltrace: invalid embedded bank dimensions or calibration")
	if b.BuiltAt == "" || len(b.Models) == 0 || len(b.Robust.ModelOrder) != len(b.Models) {
		return bad
	}
	seen := map[string]bool{}
	for i, m := range b.Models {
		if m.ID == "" || seen[m.ID] || b.Robust.ModelOrder[i] != m.ID || len(m.Counts) != 355 {
			return bad
		}
		seen[m.ID] = true
		for _, count := range m.Counts {
			if count < 0 || !finite(count) {
				return bad
			}
		}
	}
	checkRow := func(row []float64, dims int) bool {
		if len(row) != dims {
			return false
		}
		for _, v := range row {
			if !finite(v) {
				return false
			}
		}
		return true
	}
	check := func(a Artifact, dims int) bool {
		if !checkRow(a.FeatureMean, dims) || !checkRow(a.FeatureScale, dims) || len(a.Centroids) != len(b.Models) {
			return false
		}
		for _, scale := range a.FeatureScale {
			if scale <= 0 {
				return false
			}
		}
		for _, rows := range [][][]float64{a.NuisanceBasis, a.Centroids} {
			for _, row := range rows {
				if !checkRow(row, dims) {
					return false
				}
			}
		}
		for _, environment := range a.EnvironmentCentroids {
			if len(environment) != len(b.Models) {
				return false
			}
			for _, row := range environment {
				if !checkRow(row, dims) {
					return false
				}
			}
		}
		return true
	}
	if !check(b.Robust.Hellinger, 355) {
		return bad
	}
	if a := b.Robust.OrderedBlocks; a != nil && a.Weight != 0 {
		if !finite(a.Weight) || a.Weight < 0 || a.Weight > 1 || len(a.EnvironmentCentroids) == 0 || !check(*a, 74) {
			return bad
		}
	}
	for _, key := range []string{"1", "2", "3"} {
		c, ok := b.Calibration[key]
		if !ok || c.Beta <= 0 || !finite(c.Beta) || (c.CVAccuracy != nil && (!finite(*c.CVAccuracy) || *c.CVAccuracy < 0 || *c.CVAccuracy > 1)) {
			return bad
		}
	}
	return nil
}
