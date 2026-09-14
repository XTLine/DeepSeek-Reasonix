package control

import "testing"

func newOwnedTestController(t testing.TB, options Options) *Controller {
	t.Helper()
	controller := New(options)
	t.Cleanup(controller.Close)
	return controller
}
