package modeltrace

import "math"

func standardize(values []float64) []float64 {
	center := 0.0
	for _, value := range values {
		center += value
	}
	center /= float64(len(values))
	variance := 0.0
	for _, value := range values {
		variance += (value - center) * (value - center)
	}
	scale := math.Max(math.Sqrt(variance/float64(len(values))), 1e-12)
	out := make([]float64, len(values))
	for i, value := range values {
		out[i] = (value - center) / scale
	}
	return out
}

func dot(a, b []float64) float64 {
	var out float64
	for i, value := range a {
		out += value * b[i]
	}
	return out
}

func normalized(values []float64) []float64 {
	scale := math.Max(math.Sqrt(dot(values, values)), 1e-12)
	out := make([]float64, len(values))
	for i, value := range values {
		out[i] = value / scale
	}
	return out
}

func subtractBasis(values []float64, basis [][]float64) []float64 {
	out := append([]float64(nil), values...)
	for _, vector := range basis {
		projection := dot(out, vector)
		for i := range out {
			out[i] -= projection * vector[i]
		}
	}
	return out
}

func featureStandardized(feature []float64, artifact *Artifact) []float64 {
	out := make([]float64, len(feature))
	for i, value := range feature {
		out[i] = (value - artifact.FeatureMean[i]) / artifact.FeatureScale[i]
	}
	return out
}

func centroidScores(feature []float64, centroids [][]float64) []float64 {
	out := make([]float64, len(centroids))
	for i, centroid := range centroids {
		out[i] = dot(feature, centroid)
	}
	return out
}

func hellingerFeature(numbers []int) []float64 {
	counts := make([]float64, 355)
	for _, number := range numbers {
		counts[number-1]++
	}
	total := float64(len(numbers)) + 0.5*355
	for i := range counts {
		counts[i] = math.Sqrt((counts[i] + 0.5) / total)
	}
	return counts
}

func orderedBlockFeature(numbers []int) []float64 {
	out := make([]float64, 0, 74)
	start, base, remainder := 0, len(numbers)/4, len(numbers)%4
	for block := range 4 {
		size := base
		if block < remainder {
			size++
		}
		bins := make([]float64, 16)
		for i := range bins {
			bins[i] = 0.5
		}
		for _, value := range numbers[start : start+size] {
			bins[min(15, int(math.Floor(float64(value-1)/355*16)))]++
		}
		for _, value := range bins {
			out = append(out, math.Sqrt(value/(float64(size)+8)))
		}
		start += size
	}
	digits := make([]float64, 10)
	for i := range digits {
		digits[i] = 0.5
	}
	for _, value := range numbers {
		digits[value%10]++
	}
	for _, value := range digits {
		out = append(out, math.Sqrt(value/(float64(len(numbers))+5)))
	}
	return out
}

func (b *Bank) score(numbers []int) []float64 {
	h := &b.Robust.Hellinger
	projected := normalized(subtractBasis(featureStandardized(hellingerFeature(numbers), h), h.NuisanceBasis))
	marginal := standardize(centroidScores(projected, h.Centroids))
	a := b.Robust.OrderedBlocks
	if a == nil || a.Weight == 0 {
		return marginal
	}
	standardized := featureStandardized(orderedBlockFeature(numbers), a)
	unit := normalized(standardized)
	template := make([]float64, len(b.Models))
	for i := range template {
		template[i] = math.Inf(-1)
	}
	for _, environment := range a.EnvironmentCentroids {
		for i, centroid := range environment {
			template[i] = math.Max(template[i], dot(unit, centroid))
		}
	}
	template = standardize(template)
	nuisance := standardize(centroidScores(normalized(subtractBasis(standardized, a.NuisanceBasis)), a.Centroids))
	for i := range template {
		template[i] = 0.5*template[i] + 0.5*nuisance[i]
	}
	ordered := standardize(template)
	for i := range marginal {
		marginal[i] = (1-a.Weight)*marginal[i] + a.Weight*ordered[i]
	}
	return marginal
}
