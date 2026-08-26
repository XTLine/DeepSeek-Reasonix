package main

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// swapHook keeps the App-level event hook immutable while tests replace its
// target concurrently with background emissions.
type swapHook struct {
	mu sync.Mutex
	fn func(name string, payload any)
}

func (s *swapHook) set(fn func(name string, payload any)) {
	s.mu.Lock()
	s.fn = fn
	s.mu.Unlock()
}

func (s *swapHook) add(name string, payload any) {
	s.mu.Lock()
	fn := s.fn
	s.mu.Unlock()
	if fn != nil {
		fn(name, payload)
	}
}

func waitForTabState(t *testing.T, a *App, tabID, want string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		a.remoteTabMu.Lock()
		tab := a.remoteTabs[tabID]
		state := ""
		if tab != nil {
			state = tab.state
		}
		a.remoteTabMu.Unlock()
		if state == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("remote tab %s state = %q, want %q", tabID, state, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitForRemoteTabError(t *testing.T, a *App, tabID, want string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		a.remoteTabMu.Lock()
		tab := a.remoteTabs[tabID]
		message := ""
		if tab != nil {
			message = tab.err
		}
		a.remoteTabMu.Unlock()
		if strings.Contains(message, want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("remote tab %s error = %q, want text %q", tabID, message, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
