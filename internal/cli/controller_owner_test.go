package cli

import (
	"reasonix/internal/control"
	"testing"
)

func newOwnedTestController(t testing.TB, options control.Options) *control.Controller {
	t.Helper()
	controller := control.New(options)
	t.Cleanup(controller.Close)
	return controller
}
