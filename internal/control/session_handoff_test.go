package control

import (
	"context"
	"testing"

	"reasonix/internal/agent"
	"reasonix/internal/event"
)

func TestSessionHandoffSealsAdmissionAndCanRollback(t *testing.T) {
	c := New(Options{Executor: agent.New(nil, nil, agent.NewSession(""), agent.Options{}, event.Discard), Sink: event.Discard})
	defer c.Close()
	reopen, err := c.BeginSessionHandoff(false)
	if err != nil {
		t.Fatal(err)
	}
	ran := false
	body := func(context.Context) error { ran = true; return nil }
	if got := c.runGuarded(body); got != turnDroppedRotating {
		t.Fatalf("async admitted during handoff: %v", got)
	}
	if err := c.runSynchronousTurn(context.Background(), nil, body); err == nil {
		t.Fatal("sync admitted during handoff")
	}
	if err := c.beginRotation(); err == nil {
		t.Fatal("rotation admitted during handoff")
	}
	if _, err := c.BeginSessionHandoff(true); err == nil {
		t.Fatal("competing handoff admitted")
	}
	if ran {
		t.Fatal("body executed")
	}
	reopen()
	reopen()
	if err := c.beginRotation(); err != nil {
		t.Fatal(err)
	}
	c.endRotation()
}
