package modeltrace

import (
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

var digitRE = regexp.MustCompile("[0-9]+")

// ParseNumbers preserves upstream parsing: take the first longest digit run,
// split runs at letters, and ignore values outside 1..355. This deliberately
// does not rewrite, reorder, or repair the original model output.
func ParseNumbers(text string) []int {
	best, current := []int{}, []int{}
	previousEnd := 0
	for _, loc := range digitRE.FindAllStringIndex(text, -1) {
		if len(current) > 0 && strings.ContainsFunc(text[previousEnd:loc[0]], unicode.IsLetter) {
			if len(current) > len(best) {
				best = current
			}
			current = nil
		}
		// Strip leading zeros before conversion, matching JavaScript Number
		// even when a small number has a prefix too long for a machine integer.
		digits := strings.TrimLeft(text[loc[0]:loc[1]], "0")
		if len(digits) <= 3 {
			if n, err := strconv.Atoi(digits); err == nil && n >= 1 && n <= 355 {
				current = append(current, n)
			}
		}
		previousEnd = loc[1]
	}
	if len(current) > len(best) {
		best = current
	}
	return best
}

// Inspect reports sample quality without changing ModelTrace's acceptance or
// scoring rules. A warning is evidence to inspect, not a model identity verdict.
func Inspect(answer Answer) Diagnostic {
	return inspectNumbers(ParseNumbers(answer.Text), answer.ExpectedCount)
}

func inspectNumbers(numbers []int, expected int) Diagnostic {
	minimum := max(80, int(math.Ceil(float64(expected)*0.55)))
	d := Diagnostic{ParsedNumbers: len(numbers), MinimumNumbers: minimum, Accepted: len(numbers) >= minimum}
	if !d.Accepted {
		d.Warnings = append(d.Warnings, "insufficient_numbers")
	}
	if len(numbers) < 2 {
		return d
	}
	constant, ascending, descending := true, true, true
	for i := 1; i < len(numbers); i++ {
		delta := numbers[i] - numbers[i-1]
		constant = constant && delta == 0
		ascending = ascending && delta == 1
		descending = descending && delta == -1
	}
	switch {
	case constant:
		d.Warnings = append(d.Warnings, "constant_sequence")
	case ascending:
		d.Warnings = append(d.Warnings, "ascending_sequence")
	case descending:
		d.Warnings = append(d.Warnings, "descending_sequence")
	}
	return d
}
