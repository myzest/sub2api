package inspector

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	pluginv1 "local.sub2api/gpt-inspector/internal/pluginapi"
)

func TestRequestTimeoutDefaultsAndUnlimitedCancellation(t *testing.T) {
	s, _ := testServer(t, nil)
	for _, minutes := range []int{-2, -1, 0, 1, 30, 120, 121} {
		c := testCommand(s, "start")
		if minutes != -2 { // An omitted value also covers saved 0.1.0 commands.
			c.TimeoutMinutes = &minutes
		}
		raw, _ := json.Marshal(Config{Command: &c})
		result, err := s.ValidateConfig(context.Background(), &pluginv1.ValidateConfigRequest{ConfigJson: raw})
		valid := minutes == -2 || (minutes >= 0 && minutes <= 120)
		if err != nil || result.Valid != valid {
			t.Fatalf("timeout %d: %v / %v", minutes, result, err)
		}
		if !valid {
			continue
		}
		var normalized Config
		if err := json.Unmarshal(result.NormalizedConfigJson, &normalized); err != nil {
			t.Fatal(err)
		}
		expected := minutes
		if minutes == -2 {
			expected = 30
		}
		if normalized.Command.TimeoutMinutes == nil || *normalized.Command.TimeoutMinutes != expected {
			t.Fatal("timeout default or explicit unlimited setting lost")
		}
		parent, stop := context.WithCancel(context.Background())
		ctx, cancel := testRequestContext(parent, *normalized.Command.TimeoutMinutes)
		deadline, hasDeadline := ctx.Deadline()
		if hasDeadline != (expected > 0) {
			t.Fatalf("timeout %d: unexpected request deadline", expected)
		}
		if hasDeadline {
			remaining, want := time.Until(deadline), time.Duration(expected)*time.Minute
			if remaining > want || remaining < want-time.Second {
				t.Fatalf("timeout %d: actual deadline is %v away", expected, remaining)
			}
		}
		stop()
		if ctx.Err() != context.Canceled {
			t.Fatal("stopping did not cancel the request context")
		}
		cancel()
	}
}
