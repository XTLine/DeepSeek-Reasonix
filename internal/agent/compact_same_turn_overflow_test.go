package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"reasonix/internal/provider"
)

func TestHardCeilingBypassesSameTurnSuccessLatch(t *testing.T) {
	sess := foldableSessionOverForce(6)
	a := agentOverForce(t, &fakeProvider{reply: "digest"}, sess)
	a.activeTurnCreatedAt.Store(42)

	if err := prepareContext(context.Background(), a, CompactionTriggerPressure); err != nil {
		t.Fatalf("first pressure fold: %v", err)
	}
	version := a.currentProjectionVersion()
	if version == 0 || a.sess.compaction.lastTurn.Load() != 42 {
		t.Fatalf("first fold version=%d lastTurn=%d", version, a.sess.compaction.lastTurn.Load())
	}

	big := strings.Repeat("word ", 400)
	for i := 0; i < 100 && a.estimatedVisibleRequestTokens(a.modelVisibleMessages()) < a.hardInputCeiling(); i++ {
		sess.Add(provider.Message{Role: provider.RoleAssistant, Content: big})
		sess.Add(provider.Message{Role: provider.RoleUser, Content: "continue"})
	}
	if est, hard := a.estimatedVisibleRequestTokens(a.modelVisibleMessages()), a.hardInputCeiling(); est < hard {
		t.Fatalf("fixture did not reach hard ceiling: %d < %d", est, hard)
	}

	if err := prepareContext(context.Background(), a, CompactionTriggerOverflow); err != nil {
		t.Fatalf("same-turn overflow recovery: %v", err)
	}
	if got := a.currentProjectionVersion(); got == version {
		t.Fatalf("same-turn overflow kept projection version %d; recovery did not run", got)
	}
	recoveryVersion := a.currentProjectionVersion()
	if got := a.sess.compaction.recoveryTurn.Load(); got != 42 {
		t.Fatalf("recovery turn = %d, want 42", got)
	}

	for i := 0; i < 100 && a.estimatedVisibleRequestTokens(a.modelVisibleMessages()) < a.hardInputCeiling(); i++ {
		sess.Add(provider.Message{Role: provider.RoleAssistant, Content: big})
		sess.Add(provider.Message{Role: provider.RoleUser, Content: "continue"})
	}
	err := prepareContext(context.Background(), a, CompactionTriggerOverflow)
	if !errors.Is(err, ErrCompactionRequired) {
		t.Fatalf("second same-turn recovery error = %v, want ErrCompactionRequired", err)
	}
	if got := a.currentProjectionVersion(); got != recoveryVersion {
		t.Fatalf("second same-turn recovery advanced projection version to %d, want %d", got, recoveryVersion)
	}
}
