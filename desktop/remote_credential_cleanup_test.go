package main

import (
	"testing"

	"reasonix/internal/config"
)

// Desktop host onboarding no longer offers a credential-mode choice: hosts
// created through the desktop are always local-proxy (the desktop holds the
// key), while hand-configured modes survive desktop edits untouched.

func TestDesktopHostOnboardingForcesLocalProxy(t *testing.T) {
	home := t.TempDir()
	t.Setenv("REASONIX_HOME", home)
	t.Setenv("HOME", home)
	m := &desktopRemoteManager{}

	if _, err := m.AddHost(RemoteHostInput{Label: "gpu", Host: "gpu.local", Port: 22, User: "dev"}); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	host, ok := cfg.RemoteHost("gpu")
	if !ok {
		t.Fatal("host gpu missing after add")
	}
	if host.CredentialMode != "local-proxy" {
		t.Fatalf("new desktop host credential_mode = %q, want local-proxy", host.CredentialMode)
	}
}

func TestDesktopHostEditPreservesHandSetCredentialMode(t *testing.T) {
	home := t.TempDir()
	t.Setenv("REASONIX_HOME", home)
	t.Setenv("HOME", home)
	if err := editUserConfig(func(c *config.Config) error {
		return c.UpsertRemoteHost(config.RemoteHostEntry{
			Name: "legacy", Host: "legacy.local", Port: 22, User: "dev",
			// Hand-edited config: key material lives on the remote host.
			CredentialMode: "",
		})
	}); err != nil {
		t.Fatal(err)
	}
	m := &desktopRemoteManager{}

	if _, err := m.UpdateHost("legacy", RemoteHostInput{Label: "legacy", Host: "legacy2.local", Port: 2222, User: "ops"}); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	host, ok := cfg.RemoteHost("legacy")
	if !ok {
		t.Fatal("host legacy missing after update")
	}
	if host.CredentialMode != "" {
		t.Fatalf("desktop edit rewrote the hand-set credential mode: %q, want unchanged", host.CredentialMode)
	}
}

func TestDesktopHostReAddPreservesExistingCredentialMode(t *testing.T) {
	home := t.TempDir()
	t.Setenv("REASONIX_HOME", home)
	t.Setenv("HOME", home)
	if err := editUserConfig(func(c *config.Config) error {
		return c.UpsertRemoteHost(config.RemoteHostEntry{
			Name: "pinned", Host: "pinned.local", Port: 22, User: "dev",
			CredentialMode: "remote",
		})
	}); err != nil {
		t.Fatal(err)
	}
	m := &desktopRemoteManager{}

	if _, err := m.AddHost(RemoteHostInput{Label: "pinned", Host: "pinned.local", Port: 22, User: "dev"}); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	host, _ := cfg.RemoteHost("pinned")
	if host.CredentialMode != "remote" {
		t.Fatalf("re-add rewrote the existing credential mode: %q, want remote", host.CredentialMode)
	}
}
