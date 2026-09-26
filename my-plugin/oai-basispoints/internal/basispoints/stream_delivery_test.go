package basispoints

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func TestKeepaliveBeforeResponseIdentityDoesNotInventLifecycle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	reader, writer := io.Pipe()
	defer writer.Close()
	var out bytes.Buffer
	r := newRelay(ctx, mustPlan(t, fixtureServer(), fixtureRequest(), 7), func(raw []byte) error {
		out.Write(raw)
		cancel()
		return nil
	})
	response := upstreamResponse("text/event-stream", reader)
	response.Body = reader
	if err := r.consumeStreamEvery(response, time.Millisecond); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if out.String() != eventBytes(object{"type": "keepalive"}) || r.started != nil || r.sequence != 0 || r.keepalives != 1 {
		t.Fatalf("heartbeat invented lifecycle or sequence: %q, sequence=%d count=%d", out.String(), r.sequence, r.keepalives)
	}
	d := r.deliverySnapshot()
	if d.UpstreamEvents != 0 || d.SentEvents != 1 || d.FirstSentAtMS == 0 || d.KeepaliveMode != "keepalive" {
		t.Fatalf("incorrect delivery observations: %+v", d)
	}
}

func TestBusyMetadataAndHeldToolDeltasCannotPostponeKeepalive(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	reader, writer := io.Pipe()
	defer writer.Close()
	created := object{"type": "response.created", "response": object{"id": "resp_fixture", "status": "in_progress", "output": []any{}}}
	native := nativeItem("exec_command", object{"cmd": "pwd"})
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		defer writer.Close()
		initial := eventBytes(created) +
			eventBytes(object{"type": "response.output_item.added", "output_index": 0, "item": native}) +
			eventBytes(object{"type": "response.output_item.added", "output_index": 1, "item": terminalText("held-suffix")})
		if _, err := io.WriteString(writer, initial); err != nil {
			return
		}
		tick := time.NewTicker(time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				metadata := clone(created)
				metadata["type"] = "response.in_progress"
				delta := object{"type": "response.function_call_arguments.delta", "output_index": 0, "delta": "unvalidated-argument"}
				if _, err := io.WriteString(writer, eventBytes(metadata)+eventBytes(delta)); err != nil {
					return
				}
			}
		}
	}()
	var out bytes.Buffer
	heartbeats := 0
	r := newRelay(ctx, mustPlan(t, fixtureServer(), fixtureRequest(), 7), func(raw []byte) error {
		out.Write(raw)
		if strings.Contains(string(raw), "event: keepalive\n") {
			heartbeats++
			if heartbeats == 2 {
				cancel()
			}
		}
		return nil
	})
	response := upstreamResponse("text/event-stream", reader)
	response.Body = reader
	err := r.consumeStreamEvery(response, 20*time.Millisecond)
	<-writerDone
	if !errors.Is(err, context.Canceled) || r.keepalives != 2 {
		t.Fatalf("metadata starved keepalive: err=%v count=%d", err, r.keepalives)
	}
	for _, forbidden := range []string{"run_officejs", "unvalidated-argument", "held-suffix"} {
		if strings.Contains(out.String(), forbidden) {
			t.Fatalf("released held content %s", forbidden)
		}
	}
	d := r.deliverySnapshot()
	if d.UpstreamEvents < 3 || d.BufferedEvents != 1 || d.PendingEvents != 1 || d.SuppressedTools == 0 || d.SentEvents != r.sequence+r.keepalives {
		t.Fatalf("incorrect staged/written counters: %+v", d)
	}
}

func TestKeepaliveWriteFailureDoesNotCountAsDelivery(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	reader, writer := io.Pipe()
	defer writer.Close()
	want := errors.New("fixture downstream write failed")
	r := newRelay(ctx, mustPlan(t, fixtureServer(), fixtureRequest(), 7), func([]byte) error { return want })
	response := upstreamResponse("text/event-stream", reader)
	response.Body = reader
	if err := r.consumeStreamEvery(response, time.Millisecond); !errors.Is(err, want) {
		t.Fatal(err)
	}
	d := r.deliverySnapshot()
	if r.keepalives != 0 || r.sequence != 0 || d.SentEvents != 0 || d.FirstSentAtMS != 0 {
		t.Fatalf("failed write counted as delivery: %+v", d)
	}
	if _, err := writer.Write([]byte("reader must be closed")); err == nil {
		t.Fatal("reader leaked after write failure")
	}
}

func TestAggregatedResponseDoesNotEmitKeepalive(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	reader, writer := io.Pipe()
	defer writer.Close()
	r := newRelay(ctx, mustPlan(t, fixtureServer(), fixtureRequest(), 7), nil)
	response := upstreamResponse("text/event-stream", reader)
	response.Body = reader
	if err := r.consumeStreamEvery(response, time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	d := r.deliverySnapshot()
	if r.keepalives != 0 || d.SentEvents != 0 || d.KeepaliveMode != "none" || d.KeepaliveIntervalMS != 0 {
		t.Fatalf("non-streaming request emitted heartbeat: %+v", d)
	}
}

func TestDeliverySnapshotsAndSequencesOnlyCountSuccessfulWrites(t *testing.T) {
	var out bytes.Buffer
	r := newRelay(context.Background(), mustPlan(t, fixtureServer(), fixtureRequest(), 7), func(raw []byte) error { _, err := out.Write(raw); return err })
	r.startDelivery(time.Second)
	r.delivery.started = time.Now().Add(-50 * time.Millisecond)
	if err := r.send(object{"type": "response.created"}); err != nil {
		t.Fatal(err)
	}
	first := r.deliverySnapshot()
	if err := r.writeEvent(object{"type": "keepalive"}); err != nil {
		t.Fatal(err)
	}
	second := r.deliverySnapshot()
	if first.SentEvents != 1 || second.SentEvents != 2 || r.sequence != 1 || first.MaxSendGapMS < 50 {
		t.Fatal("snapshot aliased or heartbeat changed sequence", first, second, r.sequence)
	}
	want := errors.New("write failed")
	r.emit = func([]byte) error { return want }
	if err := r.send(object{"type": "response.in_progress"}); !errors.Is(err, want) || r.sequence != 1 || r.deliverySnapshot().SentEvents != 2 {
		t.Fatal("failed event advanced sequence or count", err)
	}
}

func TestTerminalStopsHeartbeatAndKeepsRealCompletion(t *testing.T) {
	var out bytes.Buffer
	r := newRelay(context.Background(), mustPlan(t, fixtureServer(), fixtureRequest(), 7), func(raw []byte) error { _, err := out.Write(raw); return err })
	raw := eventBytes(object{"type": "response.completed", "response": completed(terminalText("done"))})
	if err := r.consumeStreamEvery(upstreamResponse("text/event-stream", strings.NewReader(raw)), time.Hour); err != nil {
		t.Fatal(err)
	}
	if r.terminal != "response.completed" || r.keepalives != 0 || strings.Contains(out.String(), "keepalive") {
		t.Fatal("terminal changed or emitted heartbeat", out.String())
	}
}
