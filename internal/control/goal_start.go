package control

// goalActivationState is host-only; loading history never grants execution.
type goalActivationState struct {
	disarmed bool
}

// goalLaunchState is the one-shot "user just started this Goal" flag.
// It is host-only and never persisted.
type goalLaunchState struct {
	explicit bool
}

// disarmAfterError prevents an unrelated later message from silently restarting
// an unsuccessful Goal run. An explicit resume owns the next activation.
func (g *goalMachine) disarmAfterError(epoch uint64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.continuationEpoch == epoch {
		g.disarmed = true
		g.continuationEpoch++
	}
}

func (g *goalMachine) markExplicitStart() {
	g.mu.Lock()
	g.launch.explicit = true
	g.mu.Unlock()
}

func (g *goalMachine) consumeExplicitStart() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	ok := g.launch.explicit
	g.launch.explicit = false
	return ok
}

func (c *Controller) consumeExplicitGoalStart() bool {
	if c == nil {
		return false
	}
	return c.goals.consumeExplicitStart()
}
