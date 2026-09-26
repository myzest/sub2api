// Package modeltrace ports ModelTrace's pinned MIT-licensed fingerprint core.
// See LICENSE.ModelTrace and provenance.json for the original source and hashes.
package modeltrace

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sync"
)

const BankSHA256 = "1c2cb74d372f9f0f30d0dabbb7b7a838660d2f769a88d0c8489e4c662e088c21"

//go:embed unified_bank.json
var bankBytes []byte

type Model struct {
	ID string `json:"id"`
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
	Models []Model `json:"models"`
	Robust struct {
		ModelOrder    []string  `json:"model_order"`
		Hellinger     Artifact  `json:"hellinger"`
		OrderedBlocks *Artifact `json:"ordered_blocks"`
	} `json:"robust"`
	Calibration map[string]struct {
		Beta float64 `json:"beta"`
	} `json:"calibration"`
}

var load = sync.OnceValue(func() *Bank {
	sum := sha256.Sum256(bankBytes)
	if hex.EncodeToString(sum[:]) != BankSHA256 {
		panic("modeltrace: bank hash mismatch")
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

func (b *Bank) validate() error {
	bad := func() error { return fmt.Errorf("modeltrace: invalid bank dimensions or calibration") }
	if len(b.Models) == 0 || len(b.Robust.ModelOrder) != len(b.Models) {
		return bad()
	}
	seen := map[string]bool{}
	for i, m := range b.Models {
		if m.ID == "" || seen[m.ID] || b.Robust.ModelOrder[i] != m.ID {
			return bad()
		}
		seen[m.ID] = true
	}
	check := func(a Artifact, dims int) bool {
		if len(a.FeatureMean) != dims || len(a.FeatureScale) != dims || len(a.Centroids) != len(b.Models) {
			return false
		}
		for _, scale := range a.FeatureScale {
			if scale <= 0 || math.IsNaN(scale) || math.IsInf(scale, 0) {
				return false
			}
		}
		for _, row := range append(append([][]float64{}, a.NuisanceBasis...), a.Centroids...) {
			if len(row) != dims {
				return false
			}
		}
		for _, env := range a.EnvironmentCentroids {
			if len(env) != len(b.Models) {
				return false
			}
			for _, row := range env {
				if len(row) != dims {
					return false
				}
			}
		}
		return true
	}
	if !check(b.Robust.Hellinger, 355) {
		return bad()
	}
	if a := b.Robust.OrderedBlocks; a != nil && a.Weight != 0 {
		if a.Weight < 0 || a.Weight > 1 || len(a.EnvironmentCentroids) == 0 || !check(*a, 74) {
			return bad()
		}
	}
	for _, key := range []string{"1", "2", "3"} {
		c, ok := b.Calibration[key]
		if !ok || c.Beta <= 0 || math.IsNaN(c.Beta) || math.IsInf(c.Beta, 0) {
			return bad()
		}
	}
	return nil
}
