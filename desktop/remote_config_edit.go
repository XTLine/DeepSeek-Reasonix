package main

import (
	"fmt"
	"strings"

	"reasonix/internal/config"
)

// editUserConfig runs mutate against the user-global config under the edit lock
// and saves it there. Remote hosts are user-global (pinned in LoadForRoot).
func editUserConfig(mutate func(*config.Config) error) error {
	return editUserConfigIfChanged(func(cfg *config.Config) (bool, error) {
		if err := mutate(cfg); err != nil {
			return false, err
		}
		return true, nil
	})
}

// editUserConfigIfChanged keeps both the read and the no-op decision inside
// the edit lock. Callers can avoid rewriting an unchanged file without a
// stale read racing another process-local config mutation.
func editUserConfigIfChanged(mutate func(*config.Config) (bool, error)) error {
	unlock := config.LockUserConfigEdits()
	defer unlock()
	path := config.UserConfigPath()
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("cannot resolve user config path")
	}
	cfg := config.LoadForEdit(path)
	if cfg == nil {
		cfg = config.Default()
	}
	changed, err := mutate(cfg)
	if err != nil {
		return err
	}
	if !changed {
		return nil
	}
	return cfg.SaveTo(path)
}
