package serve

import (
	"encoding/json"
	"sync"
	"time"

	"reasonix/internal/agent"
	"reasonix/internal/billing"
	"reasonix/internal/event"
	"reasonix/internal/eventwire"
)

type subscription struct {
	all bool
}

// Broadcaster is the event.Sink the controllers emit to in server mode. It
// marshals each event once and fans it out to every connected SSE subscriber.
// A slow subscriber's buffer is allowed to drop rather than back-pressure the
// agent goroutine — a browser that can't keep up loses intermediate frames, not
// the whole session (it can refetch /history).
type Broadcaster struct {
	mu              sync.Mutex
	subs            map[chan []byte]subscription
	ledgers         map[string]*billing.Ledger
	current         string
	displayCurrency string
}

// NewBroadcaster returns an empty Broadcaster ready to accept subscribers.
func NewBroadcaster() *Broadcaster {
	return &Broadcaster{
		subs:    map[chan []byte]subscription{},
		ledgers: map[string]*billing.Ledger{},
	}
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

// SetCurrentSession records the controller shown by current-only subscribers.
// Untagged events remain compatible and are attributed to this session.
func (b *Broadcaster) SetCurrentSession(path string) {
	if b == nil {
		return
	}
	path = agent.CanonicalSessionPath(path)
	b.mu.Lock()
	b.current = path
	b.mu.Unlock()
}

// CurrentSession reports the session currently selected by Serve.
func (b *Broadcaster) CurrentSession() string {
	if b == nil {
		return ""
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.current
}

// ResetSession clears the current session usage ledger for legacy callers.
func (b *Broadcaster) ResetSession() {
	b.ResetSessionPath("")
}

// ResetSessionPath clears one session ledger without affecting detached
// sessions. Empty selects the current session.
func (b *Broadcaster) ResetSessionPath(path string) {
	if b == nil {
		return
	}
	b.mu.Lock()
	if path == "" {
		path = b.current
	} else {
		path = agent.CanonicalSessionPath(path)
	}
	delete(b.ledgers, path)
	b.mu.Unlock()
}

// SessionCostQuote returns the current aggregate quote without repricing.
func (b *Broadcaster) SessionCostQuote() billing.CostQuote {
	return b.SessionCostQuoteFor("")
}

// SessionCostQuoteFor returns one session's aggregate quote. Empty selects
// the current session so existing single-session callers keep their contract.
func (b *Broadcaster) SessionCostQuoteFor(path string) billing.CostQuote {
	if b == nil {
		return billing.AggregateQuotes(nil, "")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.ledgerLocked(path).Total(b.displayCurrency)
}

func (b *Broadcaster) ledgerLocked(path string) *billing.Ledger {
	if path == "" {
		path = b.current
	} else {
		path = agent.CanonicalSessionPath(path)
	}
	ledger := b.ledgers[path]
	if ledger == nil {
		ledger = billing.NewLedger()
		b.ledgers[path] = ledger
	}
	return ledger
}

// Emit marshals the event to JSON and delivers it to every subscriber. Drops to
// a subscriber whose buffer is full rather than blocking. A marshal failure is
// dropped silently — one bad event shouldn't stall the stream.
func (b *Broadcaster) Emit(e event.Event) {
	if e.SessionPath != "" {
		e.SessionPath = agent.CanonicalSessionPath(e.SessionPath)
	}
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
			continue
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
func (b *Broadcaster) Subscribe() (<-chan []byte, func()) {
	return b.subscribe(false)
}

// SubscribeAll receives tagged frames from current and detached sessions.
// Desktop uses it to maintain per-session runtime state; browser clients keep
// using Subscribe and see only the selected session.
func (b *Broadcaster) SubscribeAll() (<-chan []byte, func()) {
	return b.subscribe(true)
}

func (b *Broadcaster) subscribe(all bool) (<-chan []byte, func()) {
	ch := make(chan []byte, 64)
	b.mu.Lock()
	b.subs[ch] = subscription{all: all}
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
