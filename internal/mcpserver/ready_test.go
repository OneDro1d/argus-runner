package mcpserver

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// ── VR8-K2 (V26-003): a component that cannot serve must not report that it can ──────────────────
//
// THE DEFECT, measured 2026-08-20: `argus-router` was `Up 13 hours (healthy)` with restarts=0
// while its state directory had been deleted from the host, leaving a dangling bind mount. It held
// ZERO folders, so every agent on the machine was silently unroutable — and `router status` answered
// `{"folders": []}`, which is also what a brand-new router says.
//
// ⚠ THE FIX DOES NOT GO IN /healthz, and that is the whole architectural point. /healthz and
// /health/live are LIVENESS: "the process is up". Kubernetes kills a pod that fails liveness, and a
// restart does not recreate a deleted host directory — so making /healthz conditional would convert a
// silent degradation into an unbounded crash-loop. /ready is where a "cannot serve" answer belongs.

func TestReady_IsUnconditionalWhenNoProbeIsSupplied(t *testing.T) {
	// The control plane and the runner pass no probe. Their behaviour must not change.
	h := NewHTTPHandler(&Server{})
	for _, path := range []string{"/ready", "/health/ready"} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		if rr.Code != http.StatusOK {
			t.Errorf("%s returned %d with no readiness probe set; components that never had one "+
				"must keep answering ready", path, rr.Code)
		}
	}
}

func TestReady_ReportsNotReadyWhenTheProbeFails(t *testing.T) {
	h := NewHTTPHandler(&Server{})
	h.SetReady(func() error { return errNotReadyForTest{} })

	for _, path := range []string{"/ready", "/health/ready"} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		if rr.Code != http.StatusServiceUnavailable {
			t.Errorf("%s returned %d for a failing readiness probe — a router that cannot read its "+
				"own state answered as though it could serve, which is V26-003", path, rr.Code)
		}
		if body := rr.Body.String(); body == "ready" || body == "" {
			t.Errorf("%s said %q instead of naming the cause; 'not ready' without a reason is the "+
				"same dead end as 'healthy' was", path, body)
		}
	}
}

// ⛔ THE REGRESSION GUARD THAT MATTERS MOST. Liveness must stay unconditional even when readiness
// fails, or Kubernetes restarts the container forever for a condition a restart cannot fix.
func TestLiveness_StaysUnconditionalEvenWhenNotReady(t *testing.T) {
	h := NewHTTPHandler(&Server{})
	h.SetReady(func() error { return errNotReadyForTest{} })

	for _, path := range []string{"/healthz", "/health/live"} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		if rr.Code != http.StatusOK {
			t.Errorf("%s returned %d while readiness was failing.\n"+
				"  Liveness answers \"the process is up\", and that is still true. Kubernetes KILLS a\n"+
				"  pod that fails liveness, and a restart does not recreate a deleted host directory —\n"+
				"  so this would turn a silent degradation into an unbounded crash-loop.", path, rr.Code)
		}
	}
}

type errNotReadyForTest struct{}

func (errNotReadyForTest) Error() string { return "state directory is unreadable" }
