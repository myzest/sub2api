package modeltrace

import (
	"errors"
	"math"
	"sort"
	"strconv"
)

// Analyze calculates one round of one to three independent answers. More
// rounds must be analyzed separately: upstream only calibrates 1/2/3 queries.
// On no usable answers the returned result still contains sample diagnostics.
func (b *Bank) Analyze(answers []Answer) (Result, error) {
	result := Result{Method: "统一全局稳健数字指纹", Diagnostics: []Diagnostic{}, Results: []Candidate{}, FamilyProbabilities: []FamilyProbability{}}
	if b == nil || len(b.Models) == 0 {
		return result, errors.New("modeltrace: 指纹库不可用")
	}
	if len(answers) < 1 || len(answers) > 3 {
		return result, errors.New("modeltrace: 每轮只能计算 1–3 份回答，多轮必须分别归因")
	}
	combined := make([]float64, len(b.Models))
	pooled := make([]float64, 355)
	for index, answer := range answers {
		numbers := ParseNumbers(answer.Text)
		diagnostic := inspectNumbers(numbers, answer.ExpectedCount)
		diagnostic.Index = index
		result.Diagnostics = append(result.Diagnostics, diagnostic)
		if !diagnostic.Accepted {
			continue
		}
		for i, value := range b.score(numbers) {
			combined[i] += value
		}
		for _, value := range numbers {
			pooled[value-1]++
		}
		result.UsedOutputs++
	}
	if result.UsedOutputs == 0 {
		return result, errors.New("没有可用回答：完整数字不足，拒答或严重截断的回答不会计入")
	}
	calibrationKey := strconv.Itoa(result.UsedOutputs)
	result.Calibration = b.Calibration[calibrationKey]
	result.Calibration.Queries = calibrationKey
	beta := result.Calibration.Beta
	maximum := math.Inf(-1)
	for i := range combined {
		combined[i] /= float64(result.UsedOutputs)
		maximum = math.Max(maximum, beta*combined[i])
	}
	weights := make([]float64, len(combined))
	total := 0.0
	for i, value := range combined {
		weights[i] = math.Exp(beta*value - maximum)
		total += weights[i]
	}
	familyIndices := map[string]int{}
	for i, model := range b.Models {
		family := model.Family
		if family == "" {
			family = "models"
		}
		if _, exists := familyIndices[family]; !exists {
			name := model.FamilyName
			if name == "" {
				name = family
			}
			familyIndices[family] = len(result.FamilyProbabilities)
			result.FamilyProbabilities = append(result.FamilyProbabilities, FamilyProbability{Family: family, DisplayName: name})
		}
		result.Results = append(result.Results, Candidate{Model: model.ID, DisplayName: model.DisplayName, Probability: weights[i] / total,
			ProfileSimilarity: jsSimilarity(pooled, model.Counts), Score: combined[i], Family: family, FamilyName: result.FamilyProbabilities[familyIndices[family]].DisplayName})
	}
	sort.SliceStable(result.Results, func(i, j int) bool { return result.Results[i].Probability > result.Results[j].Probability })
	// Upstream sums family probabilities in ranked order; preserve that order
	// to keep round-off differences small for the parity fixtures.
	for _, candidate := range result.Results {
		result.FamilyProbabilities[familyIndices[candidate.Family]].Probability += candidate.Probability
	}
	for i := range result.Results {
		candidate := &result.Results[i]
		familyProbability := result.FamilyProbabilities[familyIndices[candidate.Family]].Probability
		if familyProbability > 0 {
			candidate.ConditionalProbability = candidate.Probability / familyProbability
		}
	}
	winner := result.Results[0]
	result.Prediction, result.PredictionName, result.Probability = winner.Model, winner.DisplayName, winner.Probability
	winningFamily := result.FamilyProbabilities[0]
	for _, family := range result.FamilyProbabilities[1:] {
		if family.Probability > winningFamily.Probability {
			winningFamily = family
		}
	}
	result.FamilyPrediction, result.FamilyPredictionName, result.FamilyProbability = winningFamily.Family, winningFamily.DisplayName, winningFamily.Probability
	return result, nil
}

func jsSimilarity(left, right []float64) float64 {
	leftTotal, rightTotal := 0.0, 0.5*355
	for _, value := range left {
		leftTotal += value
	}
	for _, value := range right {
		rightTotal += value
	}
	leftDivergence, rightDivergence := 0.0, 0.0
	for i := range left {
		p, q := left[i]/leftTotal, (right[i]+0.5)/rightTotal
		midpoint := (p + q) / 2
		if p != 0 {
			leftDivergence += p * math.Log(p/midpoint)
		}
		rightDivergence += q * math.Log(q/midpoint)
	}
	return 1 - math.Sqrt((leftDivergence+rightDivergence)/2/math.Log(2))
}
