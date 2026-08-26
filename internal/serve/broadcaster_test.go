package serve

import (
	"encoding/json"
	"strings"
	"testing"

	"reasonix/internal/event"
	"reasonix/internal/eventwire"
)

func TestBroadcasterFiltersSessions(t *testing.T) {
	b := NewBroadcaster()
	b.SetCurrentSession("/sessions/current.jsonl")
	current, stopCurrent := b.Subscribe()
	all, stopAll := b.SubscribeAll()
	defer stopCurrent()
	defer stopAll()

	b.Emit(event.Event{Kind: event.Text, Text: "current", SessionPath: "/sessions/current.jsonl"})
	b.Emit(event.Event{Kind: event.Text, Text: "background", SessionPath: "/sessions/background.jsonl"})
	b.Emit(event.Event{Kind: event.Text, Text: "legacy"})

	drain := func(ch <-chan []byte) []string {
		var frames []string
		for {
			select {
			case frame := <-ch:
				frames = append(frames, string(frame))
			default:
				return frames
			}
		}
	}
	if got := len(drain(current)); got != 2 {
		t.Fatalf("current subscription received %d frames, want 2", got)
	}
	if got := len(drain(all)); got != 3 {
		t.Fatalf("all-session subscription received %d frames, want 3", got)
	}
}

func TestBroadcasterFanOut(t *testing.T) {
	b := NewBroadcaster()
	a, ca := b.Subscribe()
	d, cd := b.Subscribe()
	defer ca()
	defer cd()

	if got := b.Subscribers(); got != 2 {
		t.Fatalf("subscribers = %d, want 2", got)
	}

	b.Emit(event.Event{Kind: event.Text, Text: "hi"})

	for i, ch := range []<-chan []byte{a, d} {
		var w eventwire.Event
		if err := json.Unmarshal(<-ch, &w); err != nil {
			t.Fatalf("subscriber %d: %v", i, err)
		}
		if w.Kind != "text" || w.Text != "hi" {
			t.Errorf("subscriber %d got %+v", i, w)
		}
	}
}

func TestBroadcasterEmitToHonorsCurrentSession(t *testing.T) {
	b := NewBroadcaster()
	b.SetCurrentSession("/sessions/b.jsonl")
	current, stopCurrent := b.Subscribe()
	all, stopAll := b.SubscribeAll()
	defer stopCurrent()
	defer stopAll()
	b.EmitTo(current, event.Event{Kind: event.ApprovalRequest, SessionPath: "/sessions/a.jsonl"})
	b.EmitTo(all, event.Event{Kind: event.ApprovalRequest, SessionPath: "/sessions/a.jsonl"})
	if len(current) != 0 {
		t.Fatal("current-only subscriber received a stale session replay")
	}
	if len(all) != 1 {
		t.Fatal("all-session subscriber lost a tagged background replay")
	}
	b.EmitTo(current, event.Event{Kind: event.ApprovalRequest, SessionPath: "/sessions/b.jsonl"})
	if len(current) != 1 {
		t.Fatal("current-only subscriber lost the current session replay")
	}
}

func TestBroadcasterEmitsRetryingJSON(t *testing.T) {
	b := NewBroadcaster()
	ch, cancel := b.Subscribe()
	defer cancel()

	b.Emit(event.Event{Kind: event.Retrying, RetryAttempt: 3, RetryMax: 10})

	s := string(<-ch)
	for _, want := range []string{`"kind":"retrying"`, `"retryAttempt":3`, `"retryMax":10`} {
		if !strings.Contains(s, want) {
			t.Fatalf("retrying broadcast JSON = %s, want it to contain %s", s, want)
		}
	}
}

func TestBroadcasterUnsubscribe(t *testing.T) {
	b := NewBroadcaster()
	_, cancel := b.Subscribe()
	if b.Subscribers() != 1 {
		t.Fatalf("want 1 subscriber")
	}
	cancel()
	if b.Subscribers() != 0 {
		t.Fatalf("unsubscribe should drop to 0, got %d", b.Subscribers())
	}
	// Emitting with no subscribers must not panic.
	b.Emit(event.Event{Kind: event.TurnDone})
}

func TestBroadcasterDropsSlowSubscriber(t *testing.T) {
	b := NewBroadcaster()
	ch, cancel := b.Subscribe()
	defer cancel()
	// Overfill far past the 64-slot buffer without reading; Emit must not block.
	for range 1000 {
		b.Emit(event.Event{Kind: event.Text, Text: "x"})
	}
	if len(ch) == 0 {
		t.Error("expected some buffered frames")
	}
}
