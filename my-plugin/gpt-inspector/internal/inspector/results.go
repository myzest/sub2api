package inspector

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

func (s *Server) saveAnswerLocked(b *Batch, item int, answer Answer) (int, string, error) {
	raw, err := json.Marshal(answer)
	if err != nil {
		return 0, "", err
	}
	var out bytes.Buffer
	zw := gzip.NewWriter(&out)
	if _, err := zw.Write(raw); err != nil {
		return 0, "", err
	}
	if err := zw.Close(); err != nil {
		return 0, "", err
	}
	compressed := out.Bytes()
	if len(compressed) > 2*1024*1024 {
		return 0, "", errors.New("压缩后的回答超过 2 MiB 存储上限")
	}
	hash := sha256.Sum256(compressed)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	parts := 0
	for offset := 0; offset < len(compressed); offset += partSize {
		end := min(offset+partSize, len(compressed))
		if err := s.store.set(ctx, resultKey(b.AccountID, b.ID, item, parts), compressed[offset:end]); err != nil {
			return 0, "", err
		}
		parts++
	}
	return parts, hex.EncodeToString(hash[:]), nil
}
func (s *Server) readBatch(id int64) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.records[id]
	if r == nil {
		return Record{AccountID: id}, nil
	}
	raw, err := json.Marshal(r)
	return json.RawMessage(raw), err
}
func (s *Server) readResult(ctx context.Context, c Command) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.records[c.AccountID]
	if r == nil {
		return nil, errors.New("该账号尚无保存结果")
	}
	var b *Batch
	for _, candidate := range []*Batch{r.Latest, r.Pending} {
		if candidate != nil && candidate.ID == c.BatchID {
			b = candidate
			break
		}
	}
	if b == nil || c.Item < 0 || c.Item >= len(b.Items) {
		return nil, errors.New("结果已被新批次替换或清理，请刷新")
	}
	item := b.Items[c.Item]
	if item.Parts < 1 || item.Parts > 16 {
		return nil, errors.New("本条测试尚无可读取的回答")
	}
	var out bytes.Buffer
	for part := 0; part < item.Parts; part++ {
		raw, err := s.store.get(ctx, resultKey(c.AccountID, c.BatchID, c.Item, part))
		if err != nil {
			return nil, err
		}
		if raw == nil {
			return nil, fmt.Errorf("结果分片 %d 缺失，无法展示完整回答", part+1)
		}
		out.Write(raw)
		if out.Len() > 2*1024*1024 {
			return nil, errors.New("结果分片总大小无效")
		}
	}
	hash := sha256.Sum256(out.Bytes())
	if hex.EncodeToString(hash[:]) != item.SHA256 {
		return nil, errors.New("结果完整性校验失败")
	}
	return map[string]any{"encoding": "gzip-base64", "data": base64.StdEncoding.EncodeToString(out.Bytes()), "sha256": item.SHA256}, nil
}
