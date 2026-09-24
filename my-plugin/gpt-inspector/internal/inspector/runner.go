package inspector

import (
	"context"
	"errors"
	"fmt"
	"time"
)

func (s *Server) start(ctx context.Context, c Command) (any, error) {
	accounts, err := s.listAccounts(ctx)
	if err != nil {
		return nil, err
	}
	var account *Account
	for i := range accounts {
		if accounts[i].ID == c.AccountID && accounts[i].Schedulable {
			account = &accounts[i]
			break
		}
	}
	if account == nil {
		return nil, errors.New("所选账号当前不可调度，请刷新账号列表")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active != nil {
		return nil, errors.New("已有测试批次正在运行，请等待结束或停止当前测试")
	}
	if previous := s.records[c.AccountID]; previous != nil && previous.Pending != nil {
		return nil, errors.New("此账号上个批次尚未成功提交。可先查看已保存回答，恢复存储后重新启用插件以完成恢复；或清理此账号结果后重试")
	}
	valid := false
	for _, m := range s.catalogs[c.AccountID] {
		if m.ID == c.Model {
			for _, effort := range m.Efforts {
				if effort == c.Effort {
					valid = true
					break
				}
			}
		}
	}
	if !valid {
		return nil, errors.New("模型或推理强度不在该账号的最新目录中，请重新加载并选择")
	}
	b := &Batch{ID: c.ID, Selection: c.Selection, AccountName: account.Name, State: "running", CreatedAt: timestamp(), Items: []Item{}}
	b.Prompts = append([]string(nil), c.Prompts...)
	for round := 1; round <= c.Rounds; round++ {
		for _, prompt := range c.Prompts {
			b.Items = append(b.Items, Item{PromptID: prompt, Round: round, State: "queued", SessionID: newID()})
		}
	}
	r := &Record{AccountID: c.AccountID, Name: account.Name, Pending: b}
	if previous := s.records[c.AccountID]; previous != nil {
		r.Latest = previous.Latest
	}
	if err := s.store.setJSON(ctx, recordKey(c.AccountID), r); err != nil {
		return nil, fmt.Errorf("保存测试批次失败：%w", err)
	}
	s.records[c.AccountID] = r
	s.accounts = accounts
	// A task belongs to the plugin process, never the config.test HTTP request.
	jobCtx, cancel := context.WithCancel(context.Background())
	s.active, s.cancel = b, cancel
	s.lastError = ""
	s.publishLocked()
	go s.run(jobCtx, b)
	return map[string]string{"batch_id": b.ID}, nil
}
func (s *Server) persistLocked(id int64) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return s.store.setJSON(ctx, recordKey(id), s.records[id])
}
func (s *Server) run(ctx context.Context, b *Batch) {
	for i := range b.Items {
		s.mu.Lock()
		if s.active != b {
			s.mu.Unlock()
			return
		}
		if ctx.Err() != nil {
			s.mu.Unlock()
			break
		}
		item := &b.Items[i]
		item.State = "running"
		item.StartedAt = timestamp()
		if err := s.persistLocked(b.AccountID); err != nil {
			b.Error = "保存任务进度失败：" + err.Error()
			s.cancel()
			s.mu.Unlock()
			break
		}
		prompt, _ := findPrompt(item.PromptID)
		sessionID := item.SessionID
		s.publishLocked()
		s.mu.Unlock()
		started := time.Now()
		answer := s.ask(ctx, b.Selection, prompt, sessionID)
		s.mu.Lock()
		// Clearing first detaches the batch, then deletes its keys. Late upstream
		// completion can never recreate results after a clear has been accepted.
		if s.active != b {
			s.mu.Unlock()
			return
		}
		item.DurationMS = time.Since(started).Milliseconds()
		item.FinishedAt = timestamp()
		item.State = "completed"
		item.Error = answer.Error
		if answer.Error != "" {
			item.State = "failed"
		}
		if ctx.Err() != nil {
			item.State = "interrupted"
		}
		parts, hash, err := s.saveAnswerLocked(b, i, answer)
		if err != nil {
			item.State = "failed"
			item.Error = "保存回答失败：" + err.Error()
			b.Error = item.Error
			s.cancel()
		} else {
			item.Parts, item.SHA256 = parts, hash
		}
		if err := s.persistLocked(b.AccountID); err != nil {
			b.Error = "保存结果索引失败：" + err.Error()
			s.cancel()
		}
		s.publishLocked()
		s.mu.Unlock()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active != b {
		return
	}
	b.State = "completed"
	if ctx.Err() != nil {
		b.State = "interrupted"
	}
	if b.Error != "" {
		b.State = "failed"
	}
	b.FinishedAt = timestamp()
	for i := range b.Items {
		if b.Items[i].State == "queued" || b.Items[i].State == "running" {
			b.Items[i].State = "interrupted"
			b.Items[i].Error = "测试已停止"
			b.Items[i].FinishedAt = b.FinishedAt
		}
	}
	r := s.records[b.AccountID]
	old := r.Latest
	r.Latest, r.Pending = b, nil
	if err := s.persistLocked(b.AccountID); err != nil {
		s.lastError = "提交最新批次失败，此账号暂停新测试。恢复存储后请重新启用插件，或清理此账号结果：" + err.Error()
		// Retain the previous batch until the new pointer is durably committed.
		r.Latest, r.Pending = old, b
	} else if old != nil {
		cleanCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
		if err := s.store.deletePrefix(cleanCtx, batchPrefix(b.AccountID, old.ID)); err != nil {
			s.lastError = "最新结果已保存，但清理旧批次失败：" + err.Error()
		}
		cancel()
	}
	s.cancel()
	s.active, s.cancel = nil, nil
	s.publishLocked()
}
