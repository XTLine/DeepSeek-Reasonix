package control

import (
	"fmt"
	"sync"
)

// BeginSessionHandoff seals all turn admission before a frontend waits for the
// active work to drain. Persistence remains authorized until the lease is
// actually released. The returned callback reopens admission after rollback or
// after publishing a different session; a yielded controller keeps the gate.
func (c *Controller) BeginSessionHandoff(allowRunning bool) (func(), error) {
	c.mu.Lock()
	if c.closed || c.rotating || c.sessionHandoff || len(c.parkedTurns) > 0 || !allowRunning && (c.running || c.finishing) {
		c.mu.Unlock()
		return nil, fmt.Errorf("session runtime is busy or already transferring")
	}
	c.sessionHandoff = true
	c.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			c.mu.Lock()
			c.sessionHandoff = false
			c.mu.Unlock()
			c.maybeDispatchInbox()
		})
	}, nil
}
