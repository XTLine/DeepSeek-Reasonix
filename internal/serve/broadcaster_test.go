package serve

import (
	"encoding/json"
	"strings"
	"testing"

	"reasonix/internal/billing"
	"reasonix/internal/event"
	"reasonix/internal/eventwire"
	"reasonix/internal/provider"
)

func TestBroadcasterFanOut(t *testing.T) {
	b := NewBroadcaster()
	a, ca := b.Subscribe(false)
	d, cd := b.Subscribe(false)
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

func TestBroadcasterEmitsRetryingJSON(t *testing.T) {
	b := NewBroadcaster()
	ch, cancel := b.Subscribe(false)
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
	_, cancel := b.Subscribe(false)
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
	ch, cancel := b.Subscribe(false)
	defer cancel()
	// Overfill far past the 64-slot buffer without reading; Emit must not block.
	for range 1000 {
		b.Emit(event.Event{Kind: event.Text, Text: "x"})
	}
	if len(ch) == 0 {
		t.Error("expected some buffered frames")
	}
}

// TestBroadcasterSessionFiltering pins multi-session delivery: current-only
// subscribers receive the current session's frames plus untagged legacy
// frames; all-sessions subscribers receive every tagged frame.
func TestBroadcasterSessionFiltering(t *testing.T) {
	b := NewBroadcaster()
	b.SetCurrentSession("/s/cur.jsonl")

	currentOnly, cancelCurrent := b.Subscribe(false)
	all, cancelAll := b.Subscribe(true)
	defer cancelCurrent()
	defer cancelAll()

	b.Emit(event.Event{Kind: event.Text, SessionPath: "/s/cur.jsonl"})
	b.Emit(event.Event{Kind: event.Text, SessionPath: "/s/bg.jsonl"})
	b.Emit(event.Event{Kind: event.Text}) // untagged legacy frame

	drain := func(ch <-chan []byte) []string {
		var out []string
		for {
			select {
			case data := <-ch:
				out = append(out, string(data))
			default:
				return out
			}
		}
	}
	cur := drain(currentOnly)
	if len(cur) != 2 {
		t.Fatalf("current-only subscriber got %d frames, want 2 (current + untagged): %v", len(cur), cur)
	}
	every := drain(all)
	if len(every) != 3 {
		t.Fatalf("all-sessions subscriber got %d frames, want 3: %v", len(every), every)
	}
}

// TestBroadcasterLedgerBuckets pins per-session cost accounting: usage lands
// in the emitting session's bucket and ResetSession clears only that one.
func TestBroadcasterLedgerBuckets(t *testing.T) {
	b := NewBroadcaster()
	usage := func(path string) event.Event {
		return event.Event{
			Kind: event.Usage, SessionPath: path,
			Usage:     &provider.Usage{PromptTokens: 10},
			CostQuote: &billing.CostQuote{Original: billing.Money{Amount: "0.5", Currency: "USD"}},
		}
	}
	b.SetCurrentSession("/s/cur.jsonl")
	b.Emit(usage("/s/cur.jsonl"))
	b.Emit(usage("/s/bg.jsonl"))

	amount := func(path string) string {
		q := b.SessionCostQuote(path)
		if q.Original == (billing.Money{}) {
			return "0"
		}
		return q.Original.Amount
	}
	if amount("/s/cur.jsonl") == "0" {
		t.Fatal("current session bucket empty after its usage")
	}
	if amount("/s/bg.jsonl") == "0" {
		t.Fatal("background session bucket empty after its usage")
	}
	b.ResetSession("/s/cur.jsonl")
	if amount("/s/cur.jsonl") != "0" {
		t.Fatal("ResetSession(cur) must clear only the current bucket")
	}
	if amount("/s/bg.jsonl") == "0" {
		t.Fatal("ResetSession(cur) must not clear the background bucket")
	}
}
