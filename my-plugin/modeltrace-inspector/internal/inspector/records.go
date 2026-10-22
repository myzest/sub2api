package inspector

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
)

const recordCompressionThreshold = 64 * 1024
const maxRecordSize = 4 * 1024 * 1024
const recordGzipPrefix = "modeltrace-record-gzip-v1\n"

// Keep both latest and pending until the replacement commits. Full prompts and
// candidate tables can exceed one host KV value, so larger indices use gzip.
// Sizes continue to count the actual stored bytes, not the expanded JSON.
func (s *store) setRecord(ctx context.Context, record *Record) error {
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if len(raw) > maxRecordSize {
		return errors.New("测试索引超过大小限制")
	}
	if len(raw) > recordCompressionThreshold {
		var encoded bytes.Buffer
		encoded.WriteString(recordGzipPrefix)
		writer := gzip.NewWriter(&encoded)
		if _, err := writer.Write(raw); err != nil {
			return err
		}
		if err := writer.Close(); err != nil {
			return err
		}
		raw = encoded.Bytes()
	}
	return s.set(ctx, recordKey(record.AccountID), raw)
}

func decodeRecord(raw []byte, record *Record) error {
	if bytes.HasPrefix(raw, []byte(recordGzipPrefix)) {
		reader, err := gzip.NewReader(bytes.NewReader(raw[len(recordGzipPrefix):]))
		if err != nil {
			return err
		}
		defer reader.Close()
		raw, err = io.ReadAll(io.LimitReader(reader, maxRecordSize+1))
		if err != nil {
			return err
		}
	}
	if len(raw) > maxRecordSize {
		return errors.New("测试索引解压超过大小限制")
	}
	return json.Unmarshal(raw, record)
}
