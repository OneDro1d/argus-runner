// Package anatomy provides the the operator service-anatomy floor shared by both M3 deploy profiles
// (control and runner). At the I.0 skeleton it is the health-endpoint set (ADR-3): the anatomy-floor
// /healthz plus the Hub-pattern /health/live (liveness) + /health (readiness). The :9090/metrics
// surface, correlationId propagation, and graceful-shutdown helpers land as the services fatten
// (plan §5.4 item 3).
package anatomy

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"
)

// writeJSON emits a tiny JSON status body with the given HTTP code.
func writeJSON(w http.ResponseWriter, code int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(body))
}

// Health registers the anatomy-floor health endpoints on mux. ready reports READINESS (all
// dependencies wired and serving); if ready is nil the service is treated as always ready. Liveness
// (/healthz, /health/live) is OK for as long as the process is up — Kubernetes must not kill a live
// pod that is merely not-yet-ready.
func Health(mux *http.ServeMux, ready func() bool) {
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, `{"status":"ok"}`)
	})
	mux.HandleFunc("/health/live", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, `{"status":"live"}`)
	})
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		if ready == nil || ready() {
			writeJSON(w, http.StatusOK, `{"status":"ready"}`)
			return
		}
		writeJSON(w, http.StatusServiceUnavailable, `{"status":"not-ready"}`)
	})
}

// Serve runs srv on ln until ctx is cancelled, then gracefully shuts it down (SIGTERM → drain → exit),
// draining in-flight requests for at most drain. It returns nil on a clean shutdown (incl.
// http.ErrServerClosed) and the underlying error otherwise. This is the graceful-shutdown half of the
// service-anatomy floor, shared by both deploy profiles.
func Serve(ctx context.Context, srv *http.Server, ln net.Listener, drain time.Duration) error {
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), drain)
		defer cancel()
		return srv.Shutdown(sctx)
	}
}
