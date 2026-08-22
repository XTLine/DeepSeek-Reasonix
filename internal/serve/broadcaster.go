package serve

import (
	"encoding/json"
	"sync"
	"time"

	"reasonix/internal/billing"
	"reasonix/internal/event"
	"reasonix/internal/eventwire"
)

// subscription is one SSE client. all=false keeps only the CURRENT session's
// frames (the single-session web client); all=true receives every session's
// tagged frames (the desktop pump, which routes per session).
type subscription struct {
	ch  chan []byte
	all bool
}

// Broadcaster is the event.Sink the controllers emit to in server mode. It
// marshals each event once and fans it out to every connected SSE subscriber.
// A slow subscriber's buffer is allowed to drop rather than back-pressure the
// agent goroutine — a browser that can't keep up loses intermediate frames,
// not the whole session (it can refetch /history).
//
// Sessions: the serve may hold one current controller plus detached
// background controllers (busy sessions switched away from). Each controller
// stamps its events with event.SessionPath, so the usage ledgers are bucketed
// per session and subscriptions can filter to the current session.
type Broadcaster struct {
	mu              sync.Mutex
	subs            map[chan []byte]subscription
	ledgers         map[string]*billing.Ledger // keyed by session path; "" = untagged
	current         string                     // session path of the current controller
	displayCurrency string
}

// NewBroadcaster returns an empty Broadcaster ready to accept subscribers.
func NewBroadcaster() *Broadcaster {
	return &Broadcaster{subs: map[chan []byte]subscription{}, ledgers: map[string]*billing.Ledger{}}
}

// SetDisplayCurrency rebinds the session ledger to a stored valuation. Empty
// keeps automatic mode: a single original currency is selected and mixed
// currencies remain buckets.
func (b *Broadcaster) SetDisplayCurrency(currency string) {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.displayCurrency = billing.NormalizeCurrency(currency)
	b.mu.Unlock()
}

// SetCurrentSession records the current controller's session path so
// current-only subscriptions can filter, and untagged legacy events land in
// the current bucket.
func (b *Broadcaster) SetCurrentSession(path string) {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.current = path
	b.mu.Unlock()
}

// CurrentSession reports the recorded current session path.
func (b *Broadcaster) CurrentSession() string {
	if b == nil {
		return ""
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.current
}

// ResetSession clears the usage ledger of one session for /new, /resume and
// /fork. Background sessions keep their ledgers.
func (b *Broadcaster) ResetSession(path string) {
	if b == nil {
		return
	}
	b.mu.Lock()
	delete(b.ledgers, path)
	b.mu.Unlock()
}

// SessionCostQuote returns the session's aggregate quote without repricing;
// the empty path falls back to the current session's bucket.
func (b *Broadcaster) SessionCostQuote(path string) billing.CostQuote {
	if b == nil {
		return billing.AggregateQuotes(nil, "")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.ledgerLocked(path).Total(b.displayCurrency)
}

// ledgerLocked returns the ledger for path, assuming b.mu is held. The empty
// path maps onto the current session's bucket (single-session compatibility).
func (b *Broadcaster) ledgerLocked(path string) *billing.Ledger {
	if path == "" {
		path = b.current
	}
	ledger, ok := b.ledgers[path]
	if !ok {
		ledger = billing.NewLedger()
		b.ledgers[path] = ledger
	}
	return ledger
}

// Emit marshals the event to JSON and delivers it to every matching
// subscriber: all-sessions subscribers receive everything; current-only
// subscribers receive the current session's frames plus untagged legacy
// frames. Drops to a subscriber whose buffer is full rather than blocking. A
// marshal failure is dropped silently — one bad event shouldn't stall the
// stream.
func (b *Broadcaster) Emit(e event.Event) {
	data, err := json.Marshal(eventwire.ToWire(e))
	if err != nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if e.Kind == event.Usage && e.Usage != nil && e.CostQuote != nil {
		b.ledgerLocked(e.SessionPath).Add(*e.CostQuote, billing.UsageTokens{
			PromptTokens: e.Usage.PromptTokens, CompletionTokens: e.Usage.CompletionTokens,
			CacheHitTokens: e.Usage.CacheHitTokens, CacheMissTokens: e.Usage.CacheMissTokens,
			CacheWriteTokens: e.Usage.CacheWriteTokens, CacheWriteBilledTokens: e.Usage.CacheWriteBilledTokens,
			Estimated: e.Usage.Estimated,
		}, time.Now().UTC())
	}
	for ch, sub := range b.subs {
		if !sub.all && e.SessionPath != "" && e.SessionPath != b.current {
			continue // background session frame; this subscriber only wants the current one
		}
		select {
		case ch <- data:
		default: // subscriber is behind; drop this frame for it
		}
	}
}

// EmitTo delivers an event only to the supplied subscriber. It is used for
// connection-local recovery frames, such as replaying a prompt to a browser
// that attached after the original event was emitted. Normal runtime events
// should continue to use Emit so every subscriber receives them.
func (b *Broadcaster) EmitTo(target <-chan []byte, e event.Event) {
	data, err := json.Marshal(eventwire.ToWire(e))
	if err != nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		if (<-chan []byte)(ch) != target {
			continue
		}
		select {
		case ch <- data:
		default: // subscriber is behind; drop this frame rather than blocking.
		}
		return
	}
}

// Subscribe registers a new SSE client and returns its channel plus an
// unsubscribe func the handler must call (defer) when the client disconnects.
// all=false limits delivery to the current session's frames.
func (b *Broadcaster) Subscribe(all bool) (<-chan []byte, func()) {
	ch := make(chan []byte, 64)
	b.mu.Lock()
	b.subs[ch] = subscription{ch: ch, all: all}
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		if _, ok := b.subs[ch]; ok {
			delete(b.subs, ch)
			close(ch)
		}
		b.mu.Unlock()
	}
}

// Subscribers reports the current connection count (for diagnostics/tests).
func (b *Broadcaster) Subscribers() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs)
}
