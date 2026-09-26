package detect

import (
	"context"
	"crypto/rand"
	"errors"
	"math/big"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"local.sub2api/codex-inspector/internal/diag"
	"local.sub2api/codex-inspector/internal/kv"
	"local.sub2api/codex-inspector/internal/modeltrace"
	pluginv1 "local.sub2api/codex-inspector/internal/pluginapi/v1"
	"local.sub2api/codex-inspector/internal/transport"
)

const storageTimeout = 5 * time.Second

type Runner struct {
	Store                    *Store
	Lease                    kv.Lease
	Pool                     *transport.Pool
	Bank                     *modeltrace.Bank
	Host                     HostAPI
	Diag                     *diag.Recorder
	Holder                   string
	URL                      string
	ProbeTimeout, RenewEvery time.Duration
	Now                      func() time.Time
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}
func (r *Runner) record(action, outcome string, account int64, model string) {
	if r.Diag != nil {
		r.Diag.Record(diag.Event{Action: action, Outcome: outcome, AccountID: account, Model: model})
	}
}

func (r *Runner) renew(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, storageTimeout)
	defer cancel()
	// Do not reacquire an expired lease: a gap in ownership interrupts this task.
	ok, err := r.Lease.Holds(ctx, r.Holder)
	if err != nil || !ok {
		return false
	}
	ok, err = r.Lease.Acquire(ctx, r.Holder, leaseTTL)
	return err == nil && ok
}

func (r *Runner) holds(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, storageTimeout)
	defer cancel()
	ok, err := r.Lease.Holds(ctx, r.Holder)
	return err == nil && ok && ctx.Err() == nil
}

// Run executes only explicitly armed tasks. The server is responsible for its
// process-bound trigger and freshness checks. KV has no CAS, so this lease is
// best-effort fencing, not an exactly-once guarantee across multiple replicas.
func (r *Runner) Run(ctx context.Context, task Task) {
	if r.Store == nil || r.Store.KV == nil || r.Lease.KV == nil || r.Pool == nil || r.Host == nil || r.Bank == nil || r.Holder == "" || !normalizeTask(&task, r.Bank) {
		r.record("detect_probe", "invalid_task", 0, "")
		return
	}
	leaseCtx, leaseCancel := context.WithTimeout(ctx, storageTimeout)
	acquired, err := r.Lease.Acquire(leaseCtx, r.Holder, leaseTTL)
	leaseCancel()
	if err != nil || !acquired {
		r.record("detect_probe", "not_leader", 0, "")
		return
	}
	checkCtx, checkCancel := context.WithTimeout(ctx, storageTimeout)
	executed, err := r.Store.AlreadyExecuted(checkCtx, task.ID)
	checkCancel()
	if err != nil {
		r.record("detect_probe", "storage_failed", 0, "")
		return
	}
	if executed {
		r.record("detect_probe", "already_executed", 0, "")
		return
	}
	if ctx.Err() != nil || !r.holds(ctx) {
		r.record("detect_probe", "interrupted", 0, "")
		return
	}

	task.StartedAt = r.now()
	task.HeartbeatAt = task.StartedAt
	task.Results = []PairResult{}
	task.Done = false
	// Persist the initial record first, so a failed save cannot consume the
	// idempotency marker without leaving a visible task. No request is sent yet.
	saveCtx, saveCancel := context.WithTimeout(ctx, storageTimeout)
	err = r.Store.SaveTask(saveCtx, task)
	if err == nil {
		err = r.Store.MarkExecuted(saveCtx, task.ID)
	}
	saveCancel()
	if err != nil {
		r.record("detect_probe", "storage_failed", 0, "")
		return
	}

	runCtx, cancel := context.WithCancel(ctx)
	var mu sync.Mutex
	var renewWG sync.WaitGroup
	renewDone := make(chan struct{})
	defer func() { cancel(); close(renewDone); renewWG.Wait() }()
	interval := r.RenewEvery
	if interval <= 0 || interval >= leaseTTL/2 {
		interval = 30 * time.Second
	}
	saveLocked := func() bool {
		if runCtx.Err() != nil {
			return false
		}
		sc, done := context.WithTimeout(runCtx, storageTimeout)
		defer done()
		if err := r.Store.SaveTask(sc, task); err != nil {
			r.record("detect_probe", "storage_failed", 0, "")
			cancel()
			return false
		}
		return true
	}
	appendResult := func(result PairResult) bool {
		mu.Lock()
		defer mu.Unlock()
		if runCtx.Err() != nil {
			return false
		}
		if !r.holds(runCtx) {
			r.record("detect_probe", "interrupted", 0, "")
			cancel()
			return false
		}
		task.Results = append(task.Results, result)
		return saveLocked()
	}
	renewWG.Add(1)
	go func() {
		defer renewWG.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-renewDone:
				return
			case <-ticker.C:
				mu.Lock()
				if runCtx.Err() != nil {
					mu.Unlock()
					return
				}
				if !r.renew(runCtx) {
					r.record("detect_probe", "interrupted", 0, "")
					cancel()
					mu.Unlock()
					return
				}
				task.HeartbeatAt = r.now()
				ok := saveLocked()
				mu.Unlock()
				if !ok {
					return
				}
			}
		}
	}()

	listCtx, listCancel := context.WithTimeout(runCtx, 10*time.Second)
	accounts, err := r.Host.ListAccounts(listCtx, &pluginv1.ListAccountsRequest{Platform: "openai", AccountType: "oauth"})
	listCancel()
	if err != nil || accounts == nil {
		r.record("detect_probe", "account_list_failed", 0, "")
		return
	}
	available := map[int64]*pluginv1.AccountInfo{}
	for _, account := range accounts.Accounts {
		if account != nil && account.Platform == "openai" && account.AccountType == "oauth" {
			available[account.Id] = account
		}
	}

	jobs := make(chan int64)
	var workers sync.WaitGroup
	for range min(task.Concurrency, len(task.AccountIDs)) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for {
				select {
				case <-runCtx.Done():
					return
				case accountID, ok := <-jobs:
					if !ok {
						return
					}
					if runCtx.Err() != nil {
						return
					}
					stopReason := ""
					account := available[accountID]
					if account == nil {
						stopReason = "account_not_found"
					} else if !account.Schedulable {
						stopReason = "account_unschedulable"
					}
					for _, model := range task.Models {
						if runCtx.Err() != nil {
							return
						}
						var result PairResult
						if stopReason != "" {
							result = skippedPair(accountID, model, task.Repeats, stopReason, r.now())
						} else {
							if !r.holds(runCtx) {
								cancel()
								return
							}
							result, stopReason = r.probePair(runCtx, accountID, model, task.Repeats)
							if stopReason == "lease_lost" {
								r.record("detect_probe", "interrupted", 0, "")
								cancel()
								return
							}
						}
						if runCtx.Err() != nil || !appendResult(result) {
							return
						}
					}
				}
			}
		}()
	}
dispatch:
	for _, accountID := range task.AccountIDs {
		select {
		case <-runCtx.Done():
			break dispatch
		case jobs <- accountID:
		}
	}
	close(jobs)
	workers.Wait()
	mu.Lock()
	defer mu.Unlock()
	if runCtx.Err() != nil || len(task.Results) != len(task.AccountIDs)*len(task.Models) {
		return
	}
	if !r.holds(runCtx) {
		r.record("detect_probe", "interrupted", 0, "")
		cancel()
		return
	}
	// Stable order makes UI and offline reproduction independent of worker timing.
	modelOrder := map[string]int{}
	for i, m := range task.Models {
		modelOrder[m] = i
	}
	sort.Slice(task.Results, func(i, j int) bool {
		if task.Results[i].AccountID != task.Results[j].AccountID {
			return task.Results[i].AccountID < task.Results[j].AccountID
		}
		return modelOrder[task.Results[i].Model] < modelOrder[task.Results[j].Model]
	})
	task.Done = true
	task.HeartbeatAt = r.now()
	if saveLocked() {
		r.record("detect_done", "ok", 0, "")
	}
}

func normalizeTask(task *Task, bank *modeltrace.Bank) bool {
	if !taskIDPattern.MatchString(task.ID) || task.Repeats < 1 || task.Repeats > 10 || task.Concurrency < 1 || task.Concurrency > 16 {
		return false
	}
	ids := []int64{}
	seenIDs := map[int64]bool{}
	for _, id := range task.AccountIDs {
		if id <= 0 {
			return false
		}
		if !seenIDs[id] {
			seenIDs[id] = true
			ids = append(ids, id)
		}
	}
	known := map[string]bool{}
	for _, m := range bank.Models {
		known[m.ID] = true
	}
	models := []string{}
	seenModels := map[string]bool{}
	for _, model := range task.Models {
		if !known[model] {
			return false
		}
		if !seenModels[model] {
			seenModels[model] = true
			models = append(models, model)
		}
	}
	if len(ids) == 0 || len(models) == 0 || len(ids) > 200/len(models) {
		return false
	}
	task.AccountIDs = ids
	task.Models = models
	return true
}

func skippedPair(id int64, model string, repeats int, reason string, at time.Time) PairResult {
	return PairResult{AccountID: id, Model: model, Failures: repeats, Reasons: []string{reason}, At: at}
}

func appendReason(reasons []string, reason string) []string {
	for _, existing := range reasons {
		if existing == reason {
			return reasons
		}
	}
	return append(reasons, reason)
}

func (r *Runner) probePair(ctx context.Context, accountID int64, model string, repeats int) (PairResult, string) {
	result := PairResult{AccountID: accountID, Model: model, Reasons: []string{}}
	answers := []modeltrace.Answer{}
	var randomErr error
	challenges := modeltrace.GenerateChallenges(repeats, func(k int) int {
		if randomErr != nil {
			return 0
		}
		n, err := rand.Int(rand.Reader, big.NewInt(int64(k)))
		if err != nil {
			randomErr = err
			return 0
		}
		return int(n.Int64())
	})
	if randomErr != nil {
		return skippedPair(accountID, model, repeats, "random_failed", r.now()), "random_failed"
	}
	var totalLatency int64
	var attempts int
	stopReason := ""
	for _, challenge := range challenges {
		if ctx.Err() != nil {
			break
		}
		if !r.holds(ctx) {
			return result, "lease_lost"
		}
		timeout := r.ProbeTimeout
		if timeout <= 0 {
			timeout = 180 * time.Second
		}
		probeCtx, cancel := context.WithTimeout(ctx, timeout)
		start := time.Now()
		text, reason, halt := r.probeOne(probeCtx, accountID, model, challenge.Prompt)
		cancel()
		if ctx.Err() != nil {
			break
		}
		attempts++
		totalLatency += time.Since(start).Milliseconds()
		if reason == "" {
			answer := modeltrace.Answer{ExpectedCount: challenge.ExpectedCount, Text: text}
			analysis, err := r.Bank.Analyze([]modeltrace.Answer{answer})
			if err != nil {
				reason = "insufficient_numbers"
			} else {
				answers = append(answers, answer)
				if analysis.Prediction == model {
					result.TopHits++
				}
			}
		}
		if reason != "" {
			result.Reasons = appendReason(result.Reasons, reason)
			r.record("detect_probe", reason, accountID, model)
		} else {
			r.record("detect_probe", "ok", accountID, model)
		}
		if halt {
			stopReason = reason
			break
		}
	}
	if attempts > 0 {
		result.AvgLatencyMs = totalLatency / int64(attempts)
	}
	if len(answers) > 0 {
		analysis, err := r.Bank.Analyze(answers)
		if err == nil {
			result.Verdict = analysis.Prediction
			result.Probability = analysis.Probability
			result.ValidRuns = analysis.UsedOutputs
			result.Match = analysis.Prediction == model
		}
	}
	result.Failures = repeats - result.ValidRuns
	result.At = r.now()
	return result, stopReason
}

func (r *Runner) probeOne(ctx context.Context, accountID int64, model, prompt string) (string, string, bool) {
	identity, err := r.Host.ResolveOutboundIdentity(ctx, &pluginv1.ResolveOutboundIdentityRequest{AccountId: accountID})
	if err != nil {
		return "", "identity_failed", false
	}
	if identity == nil || !identity.Found || identity.Token == "" {
		return "", "identity_not_found", true
	}
	if identity.AccountId != accountID || identity.Platform != "openai" || identity.AccountType != "oauth" {
		return "", "identity_mismatch", true
	}
	if !r.holds(ctx) {
		return "", "lease_lost", true
	}
	id := identityFromResponse(identity)
	req, err := buildProbeRequest(ctx, r.URL, id, model, prompt)
	if err != nil {
		return "", "request_failed", false
	}
	client, err := r.Pool.Client(id.ProxyURL)
	if err != nil {
		return "", "proxy_failed", true
	}
	resp, err := client.Do(req)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", "timeout", false
		}
		if ctx.Err() != nil {
			return "", "cancelled", false
		}
		return "", "network_failed", false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		reason := "http_" + strconv.Itoa(resp.StatusCode)
		return "", reason, resp.StatusCode == 401 || resp.StatusCode == 403 || resp.StatusCode == 429
	}
	text, ok := parseSSE(resp.Body)
	if !ok {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", "timeout", false
		}
		return "", "sse_failed", false
	}
	return text, "", false
}
