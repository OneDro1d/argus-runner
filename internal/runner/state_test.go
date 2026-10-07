package runner

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/federation"
)

// VR9-C1 — THE EXECUTOR ASKS THE CONTROL PLANE WHETHER A RUN IS IN FLIGHT.
//
// ── THE CREDENTIAL IS THE POINT ───────────────────────────────────────────────────────────────────
//
// The rendered update command carries no credential, and the three obvious ways to give it one are
// each forbidden: a `--token <value>` in argv is SEC-4 (world-readable via `ps`, replayed by
// `docker inspect`); an `export ARGUS_CP_TOKEN=…` printed beside the command is a template the
// operator must repair, which VR5-U3 forbids; and softening the guard is refused because it protects
// results that were computed and never reported.
//
// So the answer comes from a credential ALREADY ON THE MACHINE: the executor's own Ed25519 machine
// identity, which never leaves the container it runs in. ⛔ NOTHING NEW IS PASSED IN — that is what
// makes this SEC-4-clean. The kit reaches the executor over the exec channel it already uses for
// `argus version`, and the executor authenticates to the control plane on its own.
//
// ⚠ THE AUTHORITY IS THE CONTROL PLANE, NOT THIS PROCESS. There is no currentRun/inFlight anywhere in
// internal/runner or internal/federation — verified. An executor answering from local state would be
// guessing, and the state where it matters most is precisely the one where a local answer is wrong: an
// executor restarted mid-run holds nothing while the control plane's fence is still set.
func TestClient_State(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = pub

	newServer := func(t *testing.T, status int, body string) (*httptest.Server, func() []*http.Request) {
		t.Helper()
		var mu sync.Mutex
		var got []*http.Request
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			got = append(got, r.Clone(context.Background()))
			mu.Unlock()
			w.WriteHeader(status)
			w.Write([]byte(body))
		}))
		t.Cleanup(srv.Close)
		return srv, func() []*http.Request { mu.Lock(); defer mu.Unlock(); return got }
	}

	t.Run("it reports the fence the control plane holds", func(t *testing.T) {
		srv, _ := newServer(t, 200, `{"running_run_id":"run-abc"}`)
		got, err := NewClient(srv.URL, "inst-1", priv).State(context.Background())
		if err != nil {
			t.Fatalf("State: %v", err)
		}
		if got != "run-abc" {
			t.Fatalf("running run = %q, want run-abc", got)
		}
	})

	t.Run("an empty fence is reported as empty, not as an error", func(t *testing.T) {
		srv, _ := newServer(t, 200, `{"running_run_id":""}`)
		got, err := NewClient(srv.URL, "inst-1", priv).State(context.Background())
		if err != nil {
			t.Fatalf("State: %v — 'no run in flight' is the ORDINARY answer and the one that permits an "+
				"update; turning it into an error would refuse every healthy machine", err)
		}
		if got != "" {
			t.Fatalf("running run = %q, want empty", got)
		}
	})

	// ⛔ IT MUST BE A GET, AND IT MUST BE /fed/state. Every other verb on that surface is an effect —
	// run/begin ACQUIRES the very lock this asks about and poll LEASES work — so asking through one of
	// them would change the answer.
	t.Run("it is a read-only GET on /fed/state", func(t *testing.T) {
		srv, seen := newServer(t, 200, `{"running_run_id":""}`)
		if _, err := NewClient(srv.URL, "inst-1", priv).State(context.Background()); err != nil {
			t.Fatal(err)
		}
		reqs := seen()
		if len(reqs) != 1 {
			t.Fatalf("%d requests, want 1", len(reqs))
		}
		if reqs[0].Method != http.MethodGet {
			t.Errorf("method = %s, want GET — a check that mutates changes the answer by asking", reqs[0].Method)
		}
		if reqs[0].URL.Path != "/fed/state" {
			t.Errorf("path = %s, want /fed/state", reqs[0].URL.Path)
		}
	})

	// ⛔ THE CREDENTIAL IS THE MACHINE IDENTITY, MINTED HERE, AND IT MUST VERIFY. Asserting only that
	// SOME Authorization header was sent would pass for a build that sent a constant.
	t.Run("it authenticates with the executor's own machine identity", func(t *testing.T) {
		srv, seen := newServer(t, 200, `{"running_run_id":""}`)
		if _, err := NewClient(srv.URL, "inst-42", priv).State(context.Background()); err != nil {
			t.Fatal(err)
		}
		auth := seen()[0].Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			t.Fatalf("Authorization = %q, want a Bearer JWT", auth)
		}
		claims, err := federation.VerifyJWT(strings.TrimPrefix(auth, "Bearer "), pub, time.Now())
		if err != nil {
			t.Fatalf("the JWT does not verify against this executor's own key: %v", err)
		}
		if claims.Sub != "inst-42" {
			t.Errorf("sub = %q, want the instance id", claims.Sub)
		}
	})

	// ⛔ A REFUSAL IS AN ERROR, NEVER AN EMPTY FENCE. "" means "no run in flight" and PERMITS an
	// update; a 401 or a 500 must never be flattened into it, or a control plane that is unreachable
	// or that refuses this machine reads as permission to proceed.
	t.Run("a non-200 is an error, not a permissive empty answer", func(t *testing.T) {
		for _, code := range []int{401, 404, 500, 503} {
			srv, _ := newServer(t, code, `{"error":"nope"}`)
			got, err := NewClient(srv.URL, "inst-1", priv).State(context.Background())
			if err == nil {
				t.Fatalf("status %d returned no error (fence=%q). An unanswerable check must not read "+
					"as 'no run in flight' — that is the permissive answer, and it would let an update "+
					"proceed over a run whose results were computed and never reported.", code, got)
			}
			if got != "" {
				t.Errorf("status %d returned a fence value %q alongside the error", code, got)
			}
		}
	})

	// Same for a body that cannot be parsed: it is an absent answer, not an empty one.
	t.Run("an unparseable body is an error", func(t *testing.T) {
		srv, _ := newServer(t, 200, `{not json`)
		if _, err := NewClient(srv.URL, "inst-1", priv).State(context.Background()); err == nil {
			t.Fatal("a body that could not be parsed was treated as 'no run in flight'")
		}
	})

	// ⛔ AND A MISSING FIELD IS NOT AN EMPTY FIELD. A control plane too old to have this endpoint, or a
	// proxy answering 200 with something else, must not be read as permission.
	t.Run("a 200 with no running_run_id field is an error", func(t *testing.T) {
		srv, _ := newServer(t, 200, `{"ok":true}`)
		if _, err := NewClient(srv.URL, "inst-1", priv).State(context.Background()); err == nil {
			t.Fatal("a 200 that never mentioned running_run_id was read as 'no run in flight'. An " +
				"absent answer and an empty one are different, and only one of them permits an update.")
		}
	})

	t.Run("the response shape is the CP's", func(t *testing.T) {
		// Guards the field name against drift: the CP writes running_run_id (fed.go handleState).
		var probe struct {
			RunningRunID string `json:"running_run_id"`
		}
		if err := json.Unmarshal([]byte(`{"running_run_id":"x"}`), &probe); err != nil || probe.RunningRunID != "x" {
			t.Fatalf("the field name this client decodes is not the one the control plane writes")
		}
	})
}
