package inspector

import (
	"crypto/rand"
	"math/big"

	"local.sub2api/modeltrace-inspector/internal/modeltrace"
)

type Prompt struct {
	Text          string `json:"text"`
	ExpectedCount int    `json:"expected_count"`
}

func roundItems(round int) []Item {
	challenges := modeltrace.GenerateChallenges(probesPerRound, func(n int) int {
		value, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
		if err != nil {
			panic("cryptographic random source unavailable")
		}
		return int(value.Int64())
	})
	items := make([]Item, len(challenges))
	for i, challenge := range challenges {
		items[i] = Item{
			Probe: i + 1, Round: round, State: "queued", SessionID: newID(),
			Prompt: Prompt{Text: challenge.Prompt, ExpectedCount: challenge.ExpectedCount},
		}
	}
	return items
}
