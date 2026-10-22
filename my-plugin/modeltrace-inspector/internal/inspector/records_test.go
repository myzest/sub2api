package inspector

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func encodeTestRecord(t *testing.T, raw []byte) []byte {
	t.Helper()
	var encoded bytes.Buffer
	encoded.WriteString(recordGzipPrefix)
	writer := gzip.NewWriter(&encoded)
	if _, err := writer.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

func TestRecordCompressionRoundTripAndCorruptionDetection(t *testing.T) {
	s, h := testServer(t, nil)
	for _, length := range []int{10, recordCompressionThreshold + 100} {
		record := &Record{AccountID: 7, Name: strings.Repeat("x", length), Latest: &Batch{ID: newID(), State: "completed"}}
		if err := s.store.setRecord(context.Background(), record); err != nil {
			t.Fatal(err)
		}
		raw := h.values[recordKey(7)]
		compressed := bytes.HasPrefix(raw, []byte(recordGzipPrefix))
		if compressed != (length > recordCompressionThreshold) {
			t.Fatal("unexpected storage encoding")
		}
		var decoded Record
		if err := decodeRecord(raw, &decoded); err != nil || !reflect.DeepEqual(record, &decoded) {
			t.Fatal("record changed during compression", err)
		}
		if compressed {
			truncated := raw[:len(raw)-5]
			if err := decodeRecord(truncated, &decoded); err == nil {
				t.Fatal("truncated compressed index accepted")
			}
			corrupted := append([]byte(nil), raw...)
			corrupted[len(corrupted)-8] ^= 0xff
			if err := decodeRecord(corrupted, &decoded); err == nil {
				t.Fatal("invalid gzip checksum accepted")
			}
		}
	}
	var record Record
	for _, raw := range [][]byte{[]byte("invalid JSON"), []byte(recordGzipPrefix + "invalid gzip"), encodeTestRecord(t, []byte("invalid JSON"))} {
		if err := decodeRecord(raw, &record); err == nil {
			t.Fatal("corrupt index accepted")
		}
	}
}

func TestRecordExpansionAndWriteLimit(t *testing.T) {
	// Small compressed input can expand beyond the allowed metadata size.
	raw, err := json.Marshal(Record{AccountID: 7, Name: strings.Repeat("x", maxRecordSize)})
	if err != nil {
		t.Fatal(err)
	}
	for _, data := range [][]byte{raw, encodeTestRecord(t, raw)} {
		var record Record
		if err := decodeRecord(data, &record); err == nil || !strings.Contains(err.Error(), "大小限制") {
			t.Fatal("expanded index limit was not enforced", err)
		}
	}
	s, h := testServer(t, nil)
	if err := s.store.setRecord(context.Background(), &Record{AccountID: 7, Name: strings.Repeat("x", maxRecordSize)}); err == nil {
		t.Fatal("oversize record was written")
	}
	if len(h.values) != 0 {
		t.Fatal("rejected write left partial state")
	}
}
