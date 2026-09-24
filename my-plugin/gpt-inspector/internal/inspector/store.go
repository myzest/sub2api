package inspector

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	pluginv1 "local.sub2api/gpt-inspector/internal/pluginapi"
)

const storageNamespace = "inspector-tests-v1"
const partSize = 192 * 1024

// Engine serializes writes; Health only reads an already-published snapshot.
type store struct {
	host  pluginv1.HostServiceClient
	sizes map[string]int64
}

func (s *store) get(ctx context.Context, key string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	r, err := s.host.KVGet(ctx, &pluginv1.KVGetRequest{Namespace: storageNamespace, Key: key})
	if err != nil {
		return nil, err
	}
	if !r.Found {
		return nil, nil
	}
	return r.Value, nil
}
func (s *store) set(ctx context.Context, key string, value []byte) error {
	if len(value) > 256*1024 {
		return fmt.Errorf("KV value exceeds host limit")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err := s.host.KVSet(ctx, &pluginv1.KVSetRequest{Namespace: storageNamespace, Key: key, Value: value})
	if err == nil {
		s.sizes[key] = int64(len(value))
	}
	return err
}
func (s *store) setJSON(ctx context.Context, key string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return s.set(ctx, key, raw)
}
func (s *store) del(ctx context.Context, key string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err := s.host.KVDelete(ctx, &pluginv1.KVDeleteRequest{Namespace: storageNamespace, Key: key})
	if err == nil {
		delete(s.sizes, key)
	}
	return err
}
func (s *store) list(ctx context.Context, prefix string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	r, err := s.host.KVList(ctx, &pluginv1.KVListRequest{Namespace: storageNamespace, KeyPrefix: prefix, Limit: 1000})
	if err != nil {
		return nil, err
	}
	return r.Keys, nil
}

// KVList has no cursor. Recursively partition our restricted key alphabet when
// a page is full, rather than silently forgetting keys after the first 1000.
func (s *store) allKeys(ctx context.Context, prefix string) ([]string, error) {
	keys, err := s.list(ctx, prefix)
	if err != nil || len(keys) < 1000 {
		return keys, err
	}
	var out []string
	for _, key := range keys {
		if key == prefix {
			out = append(out, key)
		}
	}
	if len(prefix) >= 256 {
		return nil, fmt.Errorf("invalid saturated key prefix")
	}
	for _, c := range ".-0123456789abcdefghijklmnopqrstuvwxyz" {
		part, err := s.allKeys(ctx, prefix+string(c))
		if err != nil {
			return nil, err
		}
		out = append(out, part...)
	}
	return out, nil
}
func (s *store) scan(ctx context.Context) error {
	keys, err := s.allKeys(ctx, "")
	if err != nil {
		return err
	}
	s.sizes = map[string]int64{}
	for _, key := range keys {
		raw, err := s.get(ctx, key)
		if err != nil {
			return err
		}
		if raw != nil {
			s.sizes[key] = int64(len(raw))
		}
	}
	return nil
}
func (s *store) deletePrefix(ctx context.Context, prefix string) error {
	for {
		keys, err := s.list(ctx, prefix)
		if err != nil {
			return err
		}
		if len(keys) == 0 {
			return nil
		}
		for _, key := range keys {
			if err := s.del(ctx, key); err != nil {
				return err
			}
		}
	}
}
func (s *store) bytes(prefix string) int64 {
	var n int64
	for key, size := range s.sizes {
		if strings.HasPrefix(key, prefix) {
			n += size
		}
	}
	return n
}
func (s *store) keys() []string {
	keys := make([]string, 0, len(s.sizes))
	for key := range s.sizes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
func accountPrefix(id int64) string             { return fmt.Sprintf("a.%d.", id) }
func recordKey(id int64) string                 { return accountPrefix(id) + "meta" }
func batchPrefix(id int64, batch string) string { return accountPrefix(id) + "b." + batch + "." }
func resultKey(id int64, batch string, item, part int) string {
	return fmt.Sprintf("%si.%03d.%03d", batchPrefix(id, batch), item, part)
}
