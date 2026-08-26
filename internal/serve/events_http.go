package serve

import (
	"fmt"
	"net/http"
	"time"

	"reasonix/internal/event"
)

// Keepalives prevent quiet turns from being closed by common 30–60 s proxies.
const sseKeepaliveInterval = 15 * time.Second

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	var ch <-chan []byte
	var unsubscribe func()
	currentPath := s.ctl().SessionPath()
	s.ctl().ReplayPendingPromptsWith(func() event.Sink {
		if r.URL.Query().Get("all") == "1" {
			ch, unsubscribe = s.bc.SubscribeAll()
		} else {
			ch, unsubscribe = s.bc.Subscribe()
		}
		return event.FuncSink(func(e event.Event) {
			if currentPath != "" {
				e.SessionPath = currentPath
			}
			s.bc.EmitTo(ch, e)
		})
	})
	defer unsubscribe()
	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()
	keepalive := time.NewTicker(sseKeepaliveInterval)
	defer keepalive.Stop()
	for {
		select {
		case data, ok := <-ch:
			if !ok {
				return
			}
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		case <-keepalive.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}
