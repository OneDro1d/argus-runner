package onboard

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// VR6-I2 (V23-014) — an onboard reports success only when a poll has been ACCEPTED.
//
// ── THE DEFECT ────────────────────────────────────────────────────────────────────────────────────
//
// Onboarding's completion check is ExecutorRegistered: does this instance appear under my workspace?
// Registration returning 200 is not evidence that the executor WORKS.
//
// Measured on orderservice-compose, 2026-08-18: the instance was registered and unusable from the
// moment it was created. 7,747 `bad_signature (clock compensated)` rejections, the first ONE SECOND
// after container creation, RestartCount 0. It never polled successfully once — and the Environments
// page said REGISTERED for twenty hours while its executor container sat there healthy, reachable, and
// refused.
//
// Ruled out by measurement, so nobody re-tests them: not the clock (3 s skew), not the reboot
// (rejections began ~17 h earlier), not a lost key (64 B on overlay, mtime unchanged), not the network
// (the CP answered 200 from inside that container), not machine-wide (sibling executors: 0 rejections).
//
// ── WHY THIS CHECK IS CAUSE-AGNOSTIC, AND THEREFORE WORTH ITS COST ────────────────────────────────
//
// VR6-I3 removes the KNOWN way to manufacture a split identity. This catches ANY way — including ones
// not yet found. It is one round trip at the end of a flow that already waits for the executor.
//
// ── THE SIGNAL ALREADY EXISTS ─────────────────────────────────────────────────────────────────────
//
// TouchLastSeen runs on every poll — "the poll IS the heartbeat" (D-FED.4) — and RegisterInstance's
// INSERT does not set last_seen at all. `store.Instance.LastSeen` is a *time.Time, so it serialises as
// null until a poll lands, and InstanceFields already publishes it on /api/instances.
//
//	last_seen != null  ==  a poll was ACCEPTED
//
// So this needs no new endpoint, no protocol change and no new field. It needs the client to read a
// value that has been sitting in the response all along.

func instancesServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/instances" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// 🚩 THE ONE THAT MATTERS. Registered, and last_seen still null — orderservice-compose exactly.
// The old check called this a success.
func TestExecutorPollAccepted_RegisteredButNeverPolledIsNotAccepted(t *testing.T) {
	srv := instancesServer(t, `{"instances":[{"instance_id":"i1","status":"registered","last_seen":null}]}`)

	accepted, seen, err := NewCloudClient(srv.URL).ExecutorPollAccepted(context.Background(), "sess", "i1")
	if err != nil {
		t.Fatalf("ExecutorPollAccepted: %v", err)
	}
	if accepted {
		t.Fatal("a registered instance that has NEVER polled was reported as accepted. That is the " +
			"defect verbatim: orderservice-compose was registered and refused for twenty hours, and " +
			"onboarding called it a successful onboard")
	}
	if !seen.IsZero() {
		t.Errorf("last-seen = %v, want the zero time when the CP has never heard a poll", seen)
	}
}

// The healthy case: a poll landed, so the CP stamped last_seen.
func TestExecutorPollAccepted_APolledInstanceIsAccepted(t *testing.T) {
	srv := instancesServer(t, `{"instances":[{"instance_id":"i1","status":"registered","last_seen":"2026-08-18T09:48:30Z"}]}`)

	accepted, seen, err := NewCloudClient(srv.URL).ExecutorPollAccepted(context.Background(), "sess", "i1")
	if err != nil {
		t.Fatalf("ExecutorPollAccepted: %v", err)
	}
	if !accepted {
		t.Fatal("an instance the CP has heard from was not reported as accepted")
	}
	if seen.IsZero() {
		t.Error("the accepted time was not returned — the caller needs it to say WHEN, not just WHETHER")
	}
}

// An instance that is not there at all is not accepted, and is not an error. Registration may simply
// not have propagated yet, and the caller polls.
func TestExecutorPollAccepted_AbsentInstanceIsNotAcceptedAndNotAnError(t *testing.T) {
	srv := instancesServer(t, `{"instances":[{"instance_id":"someone-else","last_seen":"2026-08-18T09:48:30Z"}]}`)

	accepted, _, err := NewCloudClient(srv.URL).ExecutorPollAccepted(context.Background(), "sess", "i1")
	if err != nil {
		t.Fatalf("an absent instance must not be an error: %v", err)
	}
	if accepted {
		t.Error("accepted=true for an instance that is not in the list")
	}
}

// ⚠ A TRANSPORT FAILURE IS AN ERROR, NEVER A VERDICT. This is the rule the whole round is about: a
// check that could not run must say so rather than answer. Returning (false, nil) here would make an
// unreachable control plane indistinguishable from a refused executor, and the caller would fail an
// onboard that was fine.
func TestExecutorPollAccepted_ACheckThatCannotRunErrorsRatherThanAnswering(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	accepted, _, err := NewCloudClient(srv.URL).ExecutorPollAccepted(context.Background(), "sess", "i1")
	if err == nil {
		t.Fatal("a 500 from the control plane produced a VERDICT instead of an error. 'I could not " +
			"check' and 'the executor is refused' are different facts with different remedies, and " +
			"collapsing them is the failure mode this build exists to remove")
	}
	if accepted {
		t.Error("accepted=true alongside an error")
	}
}

// A 401 is likewise a failure to check, not a verdict. Measured on 7 of 7 teardowns: the one check that
// could have caught a ghost printed 401 while every local check passed.
func TestExecutorPollAccepted_UnauthorisedIsAlsoNotAVerdict(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)

	if _, _, err := NewCloudClient(srv.URL).ExecutorPollAccepted(context.Background(), "sess", "i1"); err == nil {
		t.Error("a 401 produced a verdict rather than an error")
	}
}

// The returned time is the CP's, parsed — not a local guess. The caller prints it, and a fabricated
// timestamp beside a real one is worse than none.
func TestExecutorPollAccepted_ReturnsTheControlPlanesTimestamp(t *testing.T) {
	srv := instancesServer(t, `{"instances":[{"instance_id":"i1","last_seen":"2026-08-18T09:48:30Z"}]}`)

	_, seen, err := NewCloudClient(srv.URL).ExecutorPollAccepted(context.Background(), "sess", "i1")
	if err != nil {
		t.Fatalf("ExecutorPollAccepted: %v", err)
	}
	want, _ := time.Parse(time.RFC3339, "2026-08-18T09:48:30Z")
	if !seen.Equal(want) {
		t.Errorf("last-seen = %v, want %v", seen, want)
	}
}
