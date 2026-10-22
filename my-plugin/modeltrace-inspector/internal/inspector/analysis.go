package inspector

import "local.sub2api/modeltrace-inspector/internal/modeltrace"

// finishRound runs after all three workers have saved their individual results.
// Failed response text stays available to the user but is excluded from scoring.
func (s *Server) finishRound(job *runningBatch, round int, answers []Answer) {
	bank := modeltrace.LoadBank()
	analysis := RoundAnalysis{Round: round, State: "unavailable", Covered: bank.Covers(job.batch.Model)}
	if !analysis.Covered {
		analysis.Warnings = append(analysis.Warnings, "model_not_covered")
	}
	queries := make([]modeltrace.Answer, probesPerRound)
	seen := map[string]bool{}
	warnings := map[string]bool{}
	for i, answer := range answers {
		prompt := job.batch.Items[(round-1)*probesPerRound+i].Prompt
		queries[i].ExpectedCount = prompt.ExpectedCount
		if answer.Error == "" {
			queries[i].Text = answer.Text
			if answer.Text != "" {
				if seen[answer.Text] {
					warnings["duplicate_outputs"] = true
				}
				seen[answer.Text] = true
			}
		}
		if answer.Quality != nil {
			for _, warning := range answer.Quality.Warnings {
				warnings[warning] = true
			}
		}
	}
	// Keep warning ordering deterministic across concurrent responses.
	for _, code := range []string{"response_incomplete", "insufficient_numbers", "constant_sequence", "ascending_sequence", "descending_sequence", "duplicate_outputs"} {
		if warnings[code] {
			analysis.Warnings = append(analysis.Warnings, code)
		}
	}
	result, err := bank.Analyze(queries)
	for i, answer := range answers {
		if answer.Quality != nil && i < len(result.Diagnostics) {
			result.Diagnostics[i] = *answer.Quality
			result.Diagnostics[i].Index = i
			result.Diagnostics[i].Accepted = answer.Error == "" && answer.Quality.Accepted
		}
	}
	analysis.Result = &result
	if err != nil {
		analysis.Error = "本轮没有可用于归因的完整数字回答，请查看各条请求的原文与错误。"
	} else if result.UsedOutputs == probesPerRound {
		analysis.State = "complete"
	} else {
		analysis.State = "partial"
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	b := job.batch
	if s.jobs[b.AccountID] != job {
		return // A clear operation must never be undone by a late result.
	}
	b.Analyses = append(b.Analyses, analysis)
	if err := s.persistLocked(b.AccountID); err != nil {
		b.Error = "保存本轮归因失败：" + err.Error()
		job.cancel()
	}
	s.publishLocked()
}
