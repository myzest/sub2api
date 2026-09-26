package detect

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"time"

	"local.sub2api/codex-inspector/internal/kv"
)

const Namespace = "detect"
const taskTTL = 7 * 24 * time.Hour
const executedTTL = 24 * time.Hour
const leaseTTL = 90 * time.Second
const maxTaskBytes = 250 * 1024

var taskIDPattern = regexp.MustCompile("^[A-Za-z0-9-]{1,64}$")

type Task struct {
	ID          string       `json:"id"`
	CreatedAt   string       `json:"created_at"`
	AccountIDs  []int64      `json:"account_ids"`
	Models      []string     `json:"models"`
	Repeats     int          `json:"repeats"`
	Concurrency int          `json:"concurrency"`
	StartedAt   time.Time    `json:"started_at"`
	HeartbeatAt time.Time    `json:"heartbeat_at"`
	Done        bool         `json:"done"`
	Results     []PairResult `json:"results"`
}

type PairResult struct {
	AccountID    int64     `json:"account_id"`
	Model        string    `json:"model"`
	Verdict      string    `json:"verdict"`
	Probability  float64   `json:"probability"`
	Match        bool      `json:"match"`
	TopHits      int       `json:"top_hits"`
	ValidRuns    int       `json:"valid_runs"`
	AvgLatencyMs int64     `json:"avg_latency_ms"`
	Failures     int       `json:"failures"`
	Reasons      []string  `json:"reasons"`
	At           time.Time `json:"at"`
}

type Store struct{ KV kv.KV }

func (s *Store) MarkExecuted(ctx context.Context, id string) error {
	if !taskIDPattern.MatchString(id) {
		return errors.New("invalid task ID")
	}
	return s.KV.Set(ctx, Namespace, "executed."+id, []byte("1"), executedTTL)
}

func (s *Store) AlreadyExecuted(ctx context.Context, id string) (bool, error) {
	if !taskIDPattern.MatchString(id) {
		return false, errors.New("invalid task ID")
	}
	_, found, err := s.KV.Get(ctx, Namespace, "executed."+id)
	return found, err
}

func (s *Store) SaveTask(ctx context.Context, task Task) error {
	if !taskIDPattern.MatchString(task.ID) {
		return errors.New("invalid task ID")
	}
	data, err := json.Marshal(task)
	if err != nil {
		return errors.New("invalid task result")
	}
	if len(data) > maxTaskBytes {
		return errors.New("task result exceeds storage limit")
	}
	if err := s.KV.Set(ctx, Namespace, "task."+task.ID, data, taskTTL); err != nil {
		return err
	}
	return s.KV.Set(ctx, Namespace, "latest", []byte(task.ID), taskTTL)
}

func (s *Store) LoadTask(ctx context.Context, id string) (Task, bool, error) {
	if !taskIDPattern.MatchString(id) {
		return Task{}, false, errors.New("invalid task ID")
	}
	data, found, err := s.KV.Get(ctx, Namespace, "task."+id)
	if err != nil || !found {
		return Task{}, found, err
	}
	if len(data) > maxTaskBytes {
		return Task{}, false, errors.New("task result exceeds storage limit")
	}
	var task Task
	if err := json.Unmarshal(data, &task); err != nil || task.ID != id {
		return Task{}, false, errors.New("invalid stored task")
	}
	return task, true, nil
}

func (s *Store) LoadLatest(ctx context.Context) (Task, bool, error) {
	data, found, err := s.KV.Get(ctx, Namespace, "latest")
	if err != nil || !found {
		return Task{}, found, err
	}
	return s.LoadTask(ctx, string(data))
}
