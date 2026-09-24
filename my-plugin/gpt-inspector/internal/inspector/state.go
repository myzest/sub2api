package inspector

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

func (s *Server) publishLocked() {
	s.revision++
	x := Snapshot{Instance: s.instance, Ready: s.ready, Busy: s.busy, Error: s.lastError, Accounts: s.accounts, Prompts: prompts, Stored: []StoredAccount{}, Active: []*Batch{}, Revision: s.revision}
	for _, job := range s.jobs {
		x.Active = append(x.Active, job.batch)
	}
	sort.Slice(x.Active, func(i, j int) bool { return x.Active[i].AccountID < x.Active[j].AccountID })
	if s.store != nil {
		x.Bytes = s.store.bytes("")
	}
	for _, r := range s.records {
		entry := StoredAccount{AccountID: r.AccountID, Name: r.Name, Bytes: s.store.bytes(accountPrefix(r.AccountID))}
		for _, b := range []*Batch{r.Latest, r.Pending} {
			if b == nil {
				continue
			}
			entry.BatchID, entry.State, entry.CreatedAt = b.ID, b.State, b.CreatedAt
			for _, item := range b.Items {
				if item.Parts > 0 {
					entry.Results++
				}
			}
		}
		x.Results += entry.Results
		x.Stored = append(x.Stored, entry)
	}
	sort.Slice(x.Stored, func(i, j int) bool { return x.Stored[i].AccountID < x.Stored[j].AccountID })
	raw, _ := json.Marshal(x)
	s.snapshot.Store(string(raw))
}

func (s *Server) bootstrap() {
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	err := s.recover(ctx)
	if err != nil {
		s.lastError = "恢复测试结果失败：" + err.Error()
		s.ready = false
	} else {
		s.ready = true
		if accounts, e := s.listAccounts(ctx); e != nil {
			s.lastError = e.Error()
		} else {
			s.accounts = accounts
		}
	}
	s.busy = ""
	s.publishLocked()
}
func (s *Server) recover(ctx context.Context) error {
	if err := s.store.scan(ctx); err != nil {
		return err
	}
	if raw, err := s.store.get(ctx, "control.clear"); err != nil {
		return err
	} else if raw != nil {
		var id int64
		if err := json.Unmarshal(raw, &id); err != nil {
			return err
		}
		prefix := "a."
		if id > 0 {
			prefix = accountPrefix(id)
		}
		if err := s.store.deletePrefix(ctx, prefix); err != nil {
			return err
		}
		if err := s.store.del(ctx, "control.clear"); err != nil {
			return err
		}
	}
	s.records = map[int64]*Record{}
	for _, key := range s.store.keys() {
		if !strings.HasSuffix(key, ".meta") {
			continue
		}
		raw, err := s.store.get(ctx, key)
		if err != nil {
			return err
		}
		var r Record
		if err := json.Unmarshal(raw, &r); err != nil {
			return fmt.Errorf("结果索引损坏：%s", key)
		}
		if r.AccountID <= 0 || key != recordKey(r.AccountID) {
			return fmt.Errorf("结果索引位置无效：%s", key)
		}
		if r.Pending != nil {
			b := r.Pending
			b.State = "interrupted"
			b.FinishedAt = timestamp()
			for i := range b.Items {
				if b.Items[i].State == "queued" || b.Items[i].State == "running" {
					b.Items[i].State = "interrupted"
					b.Items[i].Error = "插件进程停止，未自动续跑"
				}
			}
			r.Latest, r.Pending = b, nil
			if err := s.store.setJSON(ctx, key, &r); err != nil {
				return err
			}
		}
		s.records[r.AccountID] = &r
	}
	// Only chunks committed in an account manifest are live. This removes old
	// batches and partial writes left by a crash, including >1000 KV entries.
	live := map[string]bool{}
	for _, r := range s.records {
		live[recordKey(r.AccountID)] = true
		for _, b := range []*Batch{r.Latest, r.Pending} {
			if b == nil {
				continue
			}
			if !uuidPattern.MatchString(b.ID) || len(b.Items) > maxRounds*len(prompts) {
				return fmt.Errorf("测试批次索引无效")
			}
			for i, item := range b.Items {
				if item.Parts < 0 || item.Parts > 16 {
					return fmt.Errorf("结果分片索引无效")
				}
				for part := 0; part < item.Parts; part++ {
					live[resultKey(r.AccountID, b.ID, i, part)] = true
				}
			}
		}
	}
	for _, key := range s.store.keys() {
		if !live[key] {
			if err := s.store.del(ctx, key); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Server) beginClear(ctx context.Context, c Command) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := c.AccountID
	if c.Action == "clear_all" {
		id = 0
	}
	if err := s.store.setJSON(ctx, "control.clear", id); err != nil {
		return nil, fmt.Errorf("记录清理操作失败：%w", err)
	}
	for accountID, job := range s.jobs {
		if id == 0 || id == accountID {
			job.cancel()
			delete(s.jobs, accountID)
		}
	}
	s.busy = "正在清理测试数据"
	s.publishLocked()
	go s.clearData(id)
	return map[string]bool{"clearing": true}, nil
}
func (s *Server) clearData(id int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	prefix := "a."
	if id > 0 {
		prefix = accountPrefix(id)
	}
	err := s.store.deletePrefix(ctx, prefix)
	if err == nil {
		if id == 0 {
			s.records = map[int64]*Record{}
		} else {
			delete(s.records, id)
		}
		err = s.store.del(ctx, "control.clear")
	}
	s.busy = ""
	if err != nil {
		s.lastError = "清理未完成，可重试全局清空：" + err.Error()
		s.ready = false
	} else {
		s.lastError = ""
		s.ready = true
	}
	s.publishLocked()
}
