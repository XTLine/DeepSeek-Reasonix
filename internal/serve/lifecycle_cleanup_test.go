package serve

import (
	"testing"
	"time"

	"reasonix/internal/config"
	"reasonix/internal/control"
	"reasonix/internal/session"
)

// Close the host rather than its initial controller: resume and model changes
// can install a replacement that owns a different session writer.
func newLifecycleTestServer(t *testing.T, ctrl control.SessionAPI, bc *Broadcaster, cfg config.ServeConfig) *Server {
	t.Helper()
	server := New(ctrl, bc, cfg)
	t.Cleanup(func() {
		var service *session.Service
		var runtime *session.Runtime
		if owner, ok := server.ctl().(interface {
			SessionBinding() (*session.Service, *session.Runtime, bool)
		}); ok {
			service, runtime, _ = owner.SessionBinding()
		}
		server.Close()
		if service == nil || runtime == nil {
			return
		}
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if current, ok := service.Runtime(runtime.Ref()); !ok || current != runtime {
				return
			}
			time.Sleep(time.Millisecond)
		}
		t.Error("server session writer did not retire after close")
	})
	return server
}
