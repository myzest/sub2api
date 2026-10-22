package inspector

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"local.sub2api/modeltrace-inspector/internal/modeltrace"
)

func TestRoundsUseOnlyTheirOwnSamplesAndCalibration(t *testing.T) {
	var calls atomic.Int64
	s, _ := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		completed(w, fixtureNumbers(int(calls.Add(1))))
	})
	s.catalogs[7] = []Model{{ID: "gpt-6-sol", Efforts: []string{"high"}}}
	c := testCommand(s, "start")
	c.Model, c.Rounds = "gpt-6-sol", 2
	invoke(t, s, c)
	waitFor(t, func() bool { return len(getSnapshot(s).Active) == 0 })
	record := readRecord(t, s, 7)
	if record.Latest == nil || len(record.Latest.Analyses) != 2 {
		t.Fatal("missing per-round analyses", record)
	}
	if record.Latest.Bank.SHA256 != modeltrace.BankSHA256 || record.Latest.Bank.ModelCount != 18 {
		t.Fatal("saved result lost its bank provenance")
	}
	for round, analysis := range record.Latest.Analyses {
		answers := make([]modeltrace.Answer, 3)
		for probe := range 3 {
			answer := savedAnswer(t, s, 7, c.ID, round*3+probe)
			answers[probe] = modeltrace.Answer{ExpectedCount: answer.Prompt.ExpectedCount, Text: answer.Text}
		}
		expected, err := modeltrace.LoadBank().Analyze(answers)
		if err != nil {
			t.Fatal(err)
		}
		if analysis.Result == nil || analysis.Round != round+1 || analysis.State != "complete" || !analysis.Covered {
			t.Fatal("round metadata incorrect", analysis)
		}
		actual := analysis.Result
		if actual.UsedOutputs != 3 || actual.Calibration.Queries != "3" || actual.Prediction != expected.Prediction || len(actual.Results) != 18 {
			t.Fatal("round combined other rounds or lost candidates", actual)
		}
		for i, candidate := range actual.Results {
			want := expected.Results[i]
			if candidate.Model != want.Model || math.Abs(candidate.Probability-want.Probability) > 1e-12 || math.Abs(candidate.ProfileSimilarity-want.ProfileSimilarity) > 1e-12 {
				t.Fatal("saved analysis does not match its three original answers", candidate, want)
			}
		}
	}
}

func TestIncompleteNumericOutputNeverCountsAsValidEvidence(t *testing.T) {
	for _, valid := range []bool{false, true} {
		t.Run(fmt.Sprintf("one_complete_sample=%t", valid), func(t *testing.T) {
			var calls atomic.Int64
			s, _ := testServer(t, func(w http.ResponseWriter, r *http.Request) {
				n := calls.Add(1)
				switch {
				case n == 1:
					raw, _ := json.Marshal(map[string]string{"type": "response.output_text.delta", "delta": fixtureNumbers(1)})
					brokenStream(w, "data: "+string(raw)+"\n\n")
				case n == 3 && valid:
					completed(w, fixtureNumbers(3))
				default:
					completed(w, "无法完成本次请求")
				}
			})
			c := testCommand(s, "start")
			invoke(t, s, c)
			waitFor(t, func() bool { return len(getSnapshot(s).Active) == 0 })
			if calls.Load() != 3 {
				t.Fatal("invalid or partial samples were silently retried", calls.Load())
			}
			record := readRecord(t, s, 7)
			if record.Latest == nil || len(record.Latest.Analyses) != 1 {
				t.Fatal("missing round analysis")
			}
			analysis := record.Latest.Analyses[0]
			if valid {
				if analysis.State != "partial" || analysis.Result == nil || analysis.Result.UsedOutputs != 1 || analysis.Result.Calibration.Queries != "1" {
					t.Fatal("partial stream counted or wrong calibration selected", analysis)
				}
			} else if analysis.State != "unavailable" || analysis.Result == nil || analysis.Result.UsedOutputs != 0 || analysis.Result.Prediction != "" || len(analysis.Result.Results) != 0 || analysis.Error == "" {
				t.Fatal("failed samples were reported as a model prediction", analysis)
			}
			partialFound := false
			for i := range 3 {
				answer := savedAnswer(t, s, 7, c.ID, i)
				if answer.ErrorCode == "stream_interrupted" {
					partialFound = true
					if answer.Text != fixtureNumbers(1) || answer.Quality == nil || answer.Quality.ParsedNumbers != 310 || answer.Quality.Accepted {
						t.Fatal("partial numerical answer lost or accepted", answer)
					}
				}
			}
			if !partialFound {
				t.Fatal("interrupted response was not retained")
			}
		})
	}
}

func TestMaximumRoundsAndReplacementFitHostKVLimit(t *testing.T) {
	s, h := testServer(t, func(w http.ResponseWriter, r *http.Request) { completed(w, fixtureNumbers(4)) })
	for range 2 {
		c := testCommand(s, "start")
		c.Rounds = maxRounds
		invoke(t, s, c)
		waitForTimeout(t, 20*time.Second, func() bool { return len(getSnapshot(s).Active) == 0 })
		record := readRecord(t, s, 7)
		if record.Latest == nil {
			t.Fatal("maximum round batch did not commit", getSnapshot(s).Error)
		}
		if record.Pending != nil || record.Latest.ID != c.ID || record.Latest.State != "completed" || len(record.Latest.Analyses) != maxRounds {
			t.Fatalf("maximum round batch or replacement failed: batch_state=%s rounds_saved=%d batch_error=%s snapshot_error=%s", record.Latest.State, len(record.Latest.Analyses), record.Latest.Error, getSnapshot(s).Error)
		}
		for _, analysis := range record.Latest.Analyses {
			if analysis.Result == nil || analysis.Result.UsedOutputs != 3 {
				t.Fatal("maximum batch lost a round")
			}
		}
		h.mu.Lock()
		var storedBytes int64
		for key, value := range h.values {
			storedBytes += int64(len(value))
			if len(value) > 256*1024 {
				t.Errorf("%s exceeded host KV limit: %d", key, len(value))
			}
		}
		compressed := bytes.HasPrefix(h.values[recordKey(7)], []byte(recordGzipPrefix))
		h.mu.Unlock()
		if !compressed {
			t.Fatal("large result index was not compressed")
		}
		if getSnapshot(s).Bytes != storedBytes {
			t.Fatal("compressed storage accounting used expanded size")
		}
		restarted := New()
		restarted.host = h
		restarted.store = &store{host: h, sizes: map[string]int64{}}
		restarted.bootstrap()
		recovered := restarted.records[7]
		if !getSnapshot(restarted).Ready || recovered == nil || recovered.Pending != nil || recovered.Latest == nil || !reflect.DeepEqual(record.Latest, recovered.Latest) {
			t.Fatal("compressed maximum-size batch was not recovered exactly", getSnapshot(restarted).Error)
		}
		if getSnapshot(restarted).Bytes != storedBytes {
			t.Fatal("recovery changed compressed storage accounting")
		}
	}
}

func TestRestartKeepsCompletedRoundAttributionWithoutResending(t *testing.T) {
	var calls atomic.Int64
	release := make(chan struct{})
	var released sync.Once
	s, h := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) > 3 {
			<-release
		}
		completed(w, fixtureNumbers(5))
	})
	defer released.Do(func() { close(release) })
	c := testCommand(s, "start")
	c.Rounds = 2
	invoke(t, s, c)
	waitFor(t, func() bool { return calls.Load() == 6 })
	record := readRecord(t, s, 7)
	if record.Pending == nil || len(record.Pending.Analyses) != 1 {
		t.Fatal("completed first round was not saved before second round")
	}
	// Capture a durable snapshot as if the process disappeared while round 2 ran.
	recoveredHost := &fakeHost{values: map[string][]byte{}}
	h.mu.Lock()
	for key, value := range h.values {
		recoveredHost.values[key] = append([]byte(nil), value...)
	}
	h.mu.Unlock()
	restarted := New()
	restarted.host = recoveredHost
	restarted.store = &store{host: recoveredHost, sizes: map[string]int64{}}
	restarted.bootstrap()
	loaded := restarted.records[7]
	if !getSnapshot(restarted).Ready || loaded == nil || loaded.Pending != nil || loaded.Latest.State != "interrupted" || len(loaded.Latest.Analyses) != 1 {
		t.Fatal("restart failed to preserve completed round", loaded)
	}
	if !reflect.DeepEqual(record.Pending.Analyses[0], loaded.Latest.Analyses[0]) {
		t.Fatal("restart changed the completed attribution")
	}
	if calls.Load() != 6 {
		t.Fatal("recovery resent model requests")
	}
	for _, item := range loaded.Latest.Items[3:] {
		if item.State != "interrupted" || !strings.Contains(item.Error, "未自动续跑") {
			t.Fatal("pending item was not marked interrupted", item)
		}
	}
	released.Do(func() { close(release) })
	waitFor(t, func() bool { return len(getSnapshot(s).Active) == 0 })
}
