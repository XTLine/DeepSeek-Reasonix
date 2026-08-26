package serve

import (
	"net/http"

	"reasonix/internal/agent"
	"reasonix/internal/control"
)

// commitLoadedResume moves an idle controller to a validated transcript while
// keeping write authority, event tags, and current-only publication atomic to
// observers. The bool reports whether the caller may publish its routing
// barrier and HTTP success response.
func (s *Server) commitLoadedResume(w http.ResponseWriter, cur control.SessionAPI, loaded *agent.Session, realPath string) bool {
	ctrl, concrete := cur.(*control.Controller)
	if concrete && s.leases != nil {
		// Issue target authority before Resume mutates the controller. A binding
		// failure can then restore the former lease without leaving an already
		// resumed controller behind an unpublished route.
		if err := s.leases.BindControllerAuthority(ctrl); err != nil {
			_ = s.rebindSessionLease(cur.SessionPath())
			http.Error(w, "session authority: unable to bind resumed session", http.StatusInternalServerError)
			return false
		}
	}
	var tag *sessionTagSink
	if concrete {
		tag = s.tagFor(ctrl)
		if tag != nil {
			tag.BufferPath(realPath)
		}
	}
	if hook := resumeBindHookForTest; hook != nil {
		hook()
	}
	cur.Resume(loaded, realPath)
	if !concrete {
		return true
	}
	if tag == nil {
		s.setControllerPath(ctrl, realPath)
		return true
	}
	// Publish current-only routing before releasing buffered Resume events so
	// every target-tagged warning/surface is marked foreground.
	if !s.publishControllerPathIfCurrent(ctrl, realPath) {
		http.Error(w, "session changed during resume", http.StatusConflict)
		return false
	}
	tag.Activate()
	return true
}
