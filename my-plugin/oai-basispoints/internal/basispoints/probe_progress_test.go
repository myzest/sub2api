package basispoints

import (
	"strings"
	"testing"
	"time"
)

func TestProbeContextHonorsConfiguredBudgetWithoutTwoMinuteCap(t *testing.T) {
	for _, seconds := range []int{10, 120, 600, 3600} {
		cfg := defaultConfig()
		cfg.TimeoutSeconds = seconds
		before := time.Now()
		ctx, cancel := probeContext(cfg)
		after := time.Now()
		deadline, ok := ctx.Deadline()
		cancel()
		budget := time.Duration(seconds) * time.Second
		if !ok || deadline.Before(before.Add(budget)) || deadline.After(after.Add(budget)) {
			t.Fatalf("configured %d seconds, unexpected deadline %v", seconds, deadline)
		}
	}
}

func TestProbeProgressTracksMetadataWithoutClaimingCompletion(t *testing.T) {
	progress := &probeStreamProgress{}
	progress.observe("response.created", time.Unix(100, 0))
	progress.observe("response.function_call_arguments.delta", time.Unix(104, 0))
	if progress.Events != 2 || progress.FirstAt != 100 || progress.LastAt != 104 || progress.Terminal != "" || progress.Recovered {
		t.Fatal(progress)
	}
	snapshot := progress.snapshot()
	progress.observe("response.completed", time.Unix(105, 0))
	if snapshot.Events != 2 || snapshot.LastAt != 104 || progress.Terminal != "" {
		t.Fatal("snapshot mutated or observed label falsely confirmed terminal")
	}
	for _, invalid := range []string{"", "arbitrary body content", strings.Repeat("a", 129)} {
		progress.observe(invalid, time.Unix(106, 0))
		if progress.LastType != "unknown" {
			t.Fatal("unbounded/non-protocol event label retained")
		}
	}
	var absent *probeStreamProgress
	if absent.snapshot() != nil {
		t.Fatal("absent progress invented")
	}
}

func TestPublishProbeCopiesProgressAndKeepsFinishedRound(t *testing.T) {
	s := New()
	progress := &probeStreamProgress{}
	progress.observe("response.created", time.Unix(100, 0))
	p := &Probe{TimeoutSeconds: 600, Stream: progress, Rounds: []probeRoundResult{{Round: 1, Stream: progress.snapshot()}}}
	s.publishProbe(p)
	progress.observe("response.in_progress", time.Unix(101, 0))
	if s.probe.Stream.Events != 1 || s.probe.Rounds[0].Stream.Events != 1 || s.probe.TimeoutSeconds != 600 {
		t.Fatal("worker mutated published progress")
	}
	p.Stream = nil // Reset before preparing a new round, including failures.
	s.publishProbe(p)
	if s.probe.Stream != nil || s.probe.Rounds[0].Stream.Events != 1 {
		t.Fatal("new round inherited old progress or lost finished round")
	}
}
