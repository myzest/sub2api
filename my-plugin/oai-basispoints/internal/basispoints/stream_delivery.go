package basispoints

import "time"

// These are plugin-to-host observations, not proof of a client WebSocket
// handshake, host flush, or receipt by Codex. Only counters and times are kept.
type streamDeliveryDiagnostic struct {
	KeepaliveMode       string `json:"keepalive_mode"`
	KeepaliveIntervalMS int64  `json:"keepalive_interval_ms,omitempty"`
	UpstreamEvents      int    `json:"upstream_events"`
	FirstUpstreamAtMS   int64  `json:"first_upstream_at_ms,omitempty"`
	LastUpstreamAtMS    int64  `json:"last_upstream_at_ms,omitempty"`
	BufferedEvents      int    `json:"buffered_events"`
	SuppressedTools     int    `json:"suppressed_tool_events"`
	PendingEvents       int    `json:"pending_events"`
	SentEvents          int    `json:"sent_events"`
	FirstSentAtMS       int64  `json:"first_sent_at_ms,omitempty"`
	LastSentAtMS        int64  `json:"last_sent_at_ms,omitempty"`
	MaxSendGapMS        int64  `json:"max_send_gap_ms"`
}

type streamDeliveryTracker struct {
	streamDeliveryDiagnostic
	started  time.Time
	lastSent time.Time
}

func (r *relay) startDelivery(interval time.Duration) {
	r.delivery = &streamDeliveryTracker{started: time.Now()}
	r.delivery.KeepaliveMode = "none"
	if r.emit != nil {
		r.delivery.KeepaliveMode = "keepalive"
		r.delivery.KeepaliveIntervalMS = interval.Milliseconds()
	}
}

func (r *relay) recordUpstream(at time.Time) {
	if r.delivery == nil {
		return
	}
	r.delivery.UpstreamEvents++
	if r.delivery.FirstUpstreamAtMS == 0 {
		r.delivery.FirstUpstreamAtMS = at.UnixMilli()
	}
	r.delivery.LastUpstreamAtMS = at.UnixMilli()
}

func (r *relay) recordSuppressedTool() {
	if r.delivery != nil {
		r.delivery.SuppressedTools++
	}
}

// writeEvent bypasses tool buffering only for events whose caller has already
// established they may be released. Synthetic keepalive has no output or
// sequence_number: host staging can deliver it before older lifecycle events.
func (r *relay) writeEvent(event object) error {
	if r.emit == nil {
		return nil
	}
	if err := r.emit([]byte("event: " + str(event, "type") + "\ndata: " + string(encoded(event)) + "\n\n")); err != nil {
		return err
	}
	if r.delivery != nil {
		now := time.Now()
		r.delivery.observeGap(now)
		r.delivery.SentEvents++
		if r.delivery.FirstSentAtMS == 0 {
			r.delivery.FirstSentAtMS = now.UnixMilli()
		}
		r.delivery.LastSentAtMS, r.delivery.lastSent = now.UnixMilli(), now
	}
	return nil
}

func (d *streamDeliveryTracker) observeGap(now time.Time) {
	base := d.lastSent
	if base.IsZero() {
		base = d.started
	}
	if gap := now.Sub(base).Milliseconds(); gap > d.MaxSendGapMS {
		d.MaxSendGapMS = gap
	}
}

func (r *relay) deliverySnapshot() *streamDeliveryDiagnostic {
	if r.delivery == nil {
		return nil
	}
	r.delivery.observeGap(time.Now()) // Include initial wait and final silence.
	value := r.delivery.streamDeliveryDiagnostic
	value.PendingEvents = len(r.pending)
	return &value
}
