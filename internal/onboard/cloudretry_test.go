package onboard

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/retry"
)

// VR-F2b — every control-plane call onboarding makes is retried, once, by one policy.
//
// ── WHY THE RETRY IS IN GO AND NOT IN THE SHELL ───────────────────────────────────────────────────
//
// The rule says "ONE retry policy for EVERY control-plane call in onboarding (~10 of them)", and
// those ten are `cloudp cloud-*` invocations in onboard.sh. The obvious reading is a bash wrapper.
// It is the wrong one: the shell sees an exit code, not an HTTP status. A wrapper could only retry on
// "the command failed", which means it would hammer a 401 four times and take 45 seconds to tell
// someone their token was rejected — the precise opposite of what the rule asks for.
//
// Every one of those commands goes through CloudClient.do, which HAS the status. So the policy sits
// there and the shell needs no loop at all.

func newTestClient(t *testing.T, h http.HandlerFunc) (*CloudClient, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := NewCloudClient(srv.URL)
	c.Retry = retry.DoNoWait // the same policy, without spending its waits
	return c, srv
}

func TestCloudDo_RetriesA5xxAndSucceedsWhenItRecovers(t *testing.T) {
	var hits int32
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	var out struct {
		OK bool `json:"ok"`
	}
	status, err := c.getJSON(context.Background(), "/api/thing", "tok", &out)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if status != http.StatusOK || !out.OK {
		t.Errorf("status=%d out=%+v, want 200 and the parsed body", status, out)
	}
	if got := atomic.LoadInt32(&hits); got != 3 {
		t.Errorf("the control plane was called %d times, want 3 — two 503s then the recovery", got)
	}
}

// THE ONE THAT PROTECTS THE USER'S TIME. A rejected token is final; retrying it four times converts
// one clear answer into a long pause and the same answer.
func TestCloudDo_DoesNotRetryA401(t *testing.T) {
	var hits int32
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusUnauthorized)
	})
	status, err := c.getJSON(context.Background(), "/api/thing", "bad-token", nil)
	if err != nil {
		t.Fatalf("a 401 must come back as a STATUS, not an error: %v", err)
	}
	if status != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", status)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("a 401 was requested %d times, want 1.\n"+
			"  Repeating cannot change a rejected credential — it only delays telling the user.", got)
	}
}

func TestCloudDo_DoesNotRetryOther4xx(t *testing.T) {
	for _, code := range []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusConflict} {
		var hits int32
		c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&hits, 1)
			w.WriteHeader(code)
		})
		if _, err := c.getJSON(context.Background(), "/api/thing", "tok", nil); err != nil {
			t.Fatalf("%d: %v", code, err)
		}
		if got := atomic.LoadInt32(&hits); got != 1 {
			t.Errorf("status %d was requested %d times, want 1", code, got)
		}
	}
}

// A retried POST must send its body AGAIN. The first attempt consumes the reader, so without a
// rewind the second attempt posts nothing and the control plane answers 400 — turning a transient
// blip into a hard failure that looks like a malformed request, which is a far worse bug than the
// one being fixed.
func TestCloudDo_ReplaysThePostBodyOnEveryAttempt(t *testing.T) {
	var hits int32
	var bodies []string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, 512)
		n, _ := r.Body.Read(b)
		bodies = append(bodies, string(b[:n]))
		if atomic.AddInt32(&hits, 1) < 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	if _, err := c.postJSON(context.Background(), "/api/thing", []byte(`{"name":"alpha"}`), "tok", nil); err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(bodies) != 3 {
		t.Fatalf("saw %d request bodies, want 3", len(bodies))
	}
	for i, b := range bodies {
		if !strings.Contains(b, `"name":"alpha"`) {
			t.Errorf("attempt %d posted %q — the body was not rewound, so a retry would send an EMPTY "+
				"request and the CP would answer 400", i+1, b)
		}
	}
}

// A control plane that is simply not there: every attempt is spent and the caller gets a transport
// error, exactly as before the retry existed. The RETURN SHAPE must not change — callers inspect the
// status themselves, and a new shape would ripple through all ten of them.
func TestCloudDo_UnreachableStillReturnsATransportError(t *testing.T) {
	c := NewCloudClient("http://127.0.0.1:1") // nothing listens on port 1
	c.Retry = retry.DoNoWait
	status, err := c.getJSON(context.Background(), "/api/thing", "tok", nil)
	if err == nil {
		t.Fatal("an unreachable control plane returned no error")
	}
	if status != 0 {
		t.Errorf("status = %d, want 0 — we never reached the control plane at all", status)
	}
	if !strings.Contains(err.Error(), "attempt") {
		t.Errorf("the error does not say how hard we tried: %v\n"+
			"  VR-F6's refusal quotes this effort to a human; if it is silent, the refusal cannot be honest.", err)
	}
}

// A 5xx that NEVER recovers still comes back as (5xx, nil), which is what every existing caller
// expects. This adds attempts, not a new contract.
func TestCloudDo_Persistent5xxKeepsTheOldReturnShape(t *testing.T) {
	var hits int32
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusInternalServerError)
	})
	status, err := c.getJSON(context.Background(), "/api/thing", "tok", nil)
	if err != nil {
		t.Fatalf("err = %v, want nil — the caller reads the status", err)
	}
	if status != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", status)
	}
	if got := atomic.LoadInt32(&hits); got != retry.Attempts {
		t.Errorf("tried %d times, want %d", got, retry.Attempts)
	}
}
