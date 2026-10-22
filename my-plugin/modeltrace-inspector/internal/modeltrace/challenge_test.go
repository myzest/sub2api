package modeltrace

import (
	"fmt"
	"strings"
	"testing"
)

func TestGenerateChallengesUniqueLengths(t *testing.T) {
	for _, n := range []int{0, 1, 3, 41} {
		challenges := GenerateChallenges(n, func(k int) int { return k - 1 })
		if len(challenges) != n {
			t.Fatal("wrong count")
		}
		seen := map[int]bool{}
		for _, c := range challenges {
			if c.ExpectedCount < 292 || c.ExpectedCount > 332 || seen[c.ExpectedCount] {
				t.Fatalf("bad length %d", c.ExpectedCount)
			}
			seen[c.ExpectedCount] = true
			if !strings.Contains(c.Prompt, fmt.Sprintf("%d 个 1 到 355（含端点）的整数", c.ExpectedCount)) {
				t.Fatal("missing expected count")
			}
		}
	}
}

func TestGenerateChallengesBounds(t *testing.T) {
	for _, n := range []int{-1, 42} {
		if got := GenerateChallenges(n, func(int) int { return 0 }); got != nil {
			t.Fatal("invalid n accepted")
		}
	}
}
