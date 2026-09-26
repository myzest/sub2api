package diag

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"local.sub2api/codex-inspector/internal/kv"
)

const NS = "log"
const TTL = 15 * time.Minute
const RingSize = 500
const MergedLimit = 300
const PublishInterval = 10 * time.Second
const TallyWindow = 60 * time.Second
const maxPayloadBytes = 250 * 1024

type Event struct {
	At        time.Time `json:"at"`
	Host      string    `json:"host"`
	Action    string    `json:"action"`
	AccountID int64     `json:"account_id,omitempty"`
	Model     string    `json:"model,omitempty"`
	Outcome   string    `json:"outcome"`
	Count     int       `json:"count"`
	Error     string    `json:"error,omitempty"`
}
type Recorder struct {
	mu                    sync.Mutex
	host                  string
	now                   func() time.Time
	events                []*Event
	tally                 map[string]*Event
	generation, published uint64
	lastPublished         time.Time
}

func New(host string, now func() time.Time) *Recorder {
	if now == nil {
		now = time.Now
	}
	return &Recorder{host: host, now: now, tally: map[string]*Event{}}
}
func (r *Recorder) push(e *Event) {
	r.events = append(r.events, e)
	if len(r.events) > RingSize {
		old := r.events[0]
		r.events = r.events[1:]
		for k, v := range r.tally {
			if v == old {
				delete(r.tally, k)
			}
		}
	}
	r.generation++
}
func (r *Recorder) Record(e Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e.At.IsZero() {
		e.At = r.now()
	}
	e.Host = r.host
	e.Error = SanitizeError(e.Error)
	if e.Count == 0 {
		e.Count = 1
	}
	r.push(&e)
}
func (r *Recorder) Tally(action, outcome string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	key := action + "\x00" + outcome
	e := r.tally[key]
	if e != nil && now.Sub(e.At) >= 0 && now.Sub(e.At) < TallyWindow {
		e.Count++
		r.generation++
		return
	}
	e = &Event{At: now, Host: r.host, Action: action, Outcome: outcome, Count: 1}
	r.push(e)
	r.tally[key] = e
}
func (r *Recorder) eventsLocked() []Event {
	events := make([]Event, 0, len(r.events))
	for i := len(r.events) - 1; i >= 0; i-- {
		events = append(events, *r.events[i])
	}
	return events
}
func (r *Recorder) Events() []Event { r.mu.Lock(); defer r.mu.Unlock(); return r.eventsLocked() }
func (r *Recorder) Publish(ctx context.Context, k kv.KV) (bool, error) {
	r.mu.Lock()
	if r.published == r.generation && (r.lastPublished.IsZero() || r.now().Sub(r.lastPublished) < TTL/2) {
		r.mu.Unlock()
		return false, nil
	}
	events := r.eventsLocked()
	generation := r.generation
	r.mu.Unlock()
	var raw []byte
	var err error
	for {
		raw, err = json.Marshal(events)
		if err != nil {
			return false, err
		}
		if len(raw) <= maxPayloadBytes {
			break
		}
		events = events[:len(events)-1]
	}
	hash := sha256.Sum256([]byte(r.host))
	if err = k.Set(ctx, NS, hex.EncodeToString(hash[:]), raw, TTL); err != nil {
		return false, err
	}
	r.mu.Lock()
	if generation > r.published {
		r.published = generation
	}
	r.lastPublished = r.now()
	r.mu.Unlock()
	return true, nil
}
func Merge(ctx context.Context, k kv.KV) ([]Event, int, error) {
	keys, err := k.List(ctx, NS, "", 1000)
	if err != nil {
		return []Event{}, 0, err
	}
	events := []Event{}
	count := 0
	for _, key := range keys {
		raw, ok, e := k.Get(ctx, NS, key)
		if e != nil {
			return events, count, e
		}
		if !ok {
			continue
		}
		var block []Event
		if json.Unmarshal(raw, &block) != nil {
			continue
		}
		count++
		for _, event := range block {
			event.Error = SanitizeError(event.Error)
			events = append(events, event)
		}
	}
	sort.SliceStable(events, func(i, j int) bool { return events[i].At.After(events[j].At) })
	if len(events) > MergedLimit {
		events = events[:MergedLimit]
	}
	return events, count, nil
}

var urlPattern = regexp.MustCompile(`(?i)[a-z][a-z0-9+.-]*://[^\s<>"']+`)
var headerPattern = regexp.MustCompile(`(?im)\b(?:set-cookie|cookie|authorization)\s*:\s*[^\r\n]*`)
var bearerPattern = regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/-]+=*`)
var secretPattern = regexp.MustCompile(`(?i)(access_token|refresh_token|token|password|secret|cookie|authorization)\s*[:=]\s*("[^"]*"|'[^']*'|[^\s,;]+)`)

func SanitizeError(s string) string {
	s = headerPattern.ReplaceAllString(s, "sensitive-header: [redacted]")
	s = urlPattern.ReplaceAllString(s, "[url]")
	s = bearerPattern.ReplaceAllString(s, "Bearer [redacted]")
	s = secretPattern.ReplaceAllString(s, "$1=[redacted]")
	s = strings.Map(func(r rune) rune {
		if r < ' ' && r != '\t' {
			return ' '
		}
		return r
	}, s)
	runes := []rune(s)
	if len(runes) > 200 {
		s = string(runes[:200])
	}
	return s
}
