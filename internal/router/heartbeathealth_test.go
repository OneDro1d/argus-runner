package router

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// VR9-I1 rule 4 — A PERSISTENT REJECTION IS REPORTED, NOT ABSORBED.
//
// ── WHY THIS IS NOT "log the error and move on" ───────────────────────────────────────────────────
//
// A heartbeat that does not land is DELIBERATELY non-fatal: the router's job is routing, and an agent
// whose router works does not care what the cloud believes. That decision is correct and stays. Its
// cost is that a router rejected FOREVER looks exactly like a router that is fine — the defect this
// requirement exists to close was invisible for precisely that reason.
//
// ⛔ THE THREE CONTRASTS ARE THE REQUIREMENT, NOT DECORATION. D7's basis is that a 401 is a DECISION by
// the control plane; a 503 during a CP rollout, or a connection refused, is a network event that merely
// arrives over HTTP. Escalating on `status >= 400` turns every routine CP restart into an alarm telling
// the operator to re-onboard a machine that is perfectly healthy — and an alarm that cries wolf is
// worse than none, because it teaches people to skip the one that matters.
func TestBeatOutcome_EscalatesOnlyOnPersistent401(t *testing.T) {
	unreg := &RejectedError{Status: http.StatusUnauthorized, Reason: "unregistered"}
	t0 := time.Date(2026, 8, 23, 10, 4, 11, 0, time.UTC)

	t.Run("three consecutive 401s escalate", func(t *testing.T) {
		dir := t.TempDir()
		var h HeartbeatHealth
		for i := 0; i < 3; i++ {
			h = mustRecord(t, dir, unreg, t0.Add(time.Duration(i)*time.Minute))
		}
		if !h.Escalated() {
			t.Fatalf("consecutive_401=%d did not escalate; D7 fixes the threshold at 3", h.Consecutive401)
		}
		if h.Consecutive401 != 3 {
			t.Errorf("consecutive_401 = %d, want 3", h.Consecutive401)
		}
		if !h.Since.Equal(t0) {
			t.Errorf("since = %v, want the FIRST 401 of the run (%v) — the operator's question is how "+
				"long this has been broken, not when it was last checked", h.Since, t0)
		}
		if h.LastReason != "unregistered" {
			t.Errorf("last_reason = %q, want unregistered", h.LastReason)
		}
	})

	// ⛔ TWO 401s BELOW THE LINE MUST NOT ESCALATE, or the threshold is decorative.
	t.Run("two 401s do not escalate", func(t *testing.T) {
		dir := t.TempDir()
		mustRecord(t, dir, unreg, t0)
		h := mustRecord(t, dir, unreg, t0.Add(time.Minute))
		if h.Escalated() {
			t.Fatalf("escalated at %d, want no escalation below 3", h.Consecutive401)
		}
	})

	// ⛔ CONTRAST 1 — the control plane is unreachable. Not a decision; not an escalation.
	t.Run("a network error never escalates", func(t *testing.T) {
		dir := t.TempDir()
		netErr := errors.New("heartbeat: dial tcp 127.0.0.1:8080: connect: connection refused")
		var h HeartbeatHealth
		for i := 0; i < 5; i++ {
			h = mustRecord(t, dir, netErr, t0.Add(time.Duration(i)*time.Minute))
		}
		if h.Consecutive401 != 0 {
			t.Fatalf("consecutive_401 = %d after 5 transport failures, want 0 — a router that cannot "+
				"reach the control plane has not been REJECTED by it", h.Consecutive401)
		}
	})

	// ⛔ CONTRAST 2 — 503. This is the one a naive `status >= 400` implementation fails, and it is the
	// most common state in the estate: every control-plane rollout produces a burst of them.
	t.Run("a 503 never escalates", func(t *testing.T) {
		dir := t.TempDir()
		unavailable := &RejectedError{Status: http.StatusServiceUnavailable}
		var h HeartbeatHealth
		for i := 0; i < 5; i++ {
			h = mustRecord(t, dir, unavailable, t0.Add(time.Duration(i)*time.Minute))
		}
		if h.Consecutive401 != 0 || h.Escalated() {
			t.Fatalf("consecutive_401 = %d after 5x503, want 0. A 503 during a CP rollout is a network "+
				"event that happens to arrive over HTTP, not a decision about this machine's identity.",
				h.Consecutive401)
		}
	})

	// ⛔ CONTRAST 3 — THE COUNTER RESETS. A LIFETIME counter escalates after three unrelated blips
	// spread over a month, which is an alarm about nothing.
	t.Run("an accepted beat resets the counter", func(t *testing.T) {
		dir := t.TempDir()
		mustRecord(t, dir, unreg, t0)
		mustRecord(t, dir, unreg, t0.Add(time.Minute))
		h := mustRecord(t, dir, nil, t0.Add(2*time.Minute)) // accepted
		if h.Consecutive401 != 0 {
			t.Fatalf("consecutive_401 = %d after an ACCEPTED beat, want 0", h.Consecutive401)
		}
		h = mustRecord(t, dir, unreg, t0.Add(3*time.Minute))
		if h.Escalated() {
			t.Fatalf("two 401s, an accepted beat, then one 401 escalated at %d. The counter is counting "+
				"a lifetime rather than a streak.", h.Consecutive401)
		}
		if !h.Since.Equal(t0.Add(3 * time.Minute)) {
			t.Errorf("since = %v, want the first 401 of the NEW streak — a reset must clear it too, or "+
				"the operator is told the outage began before a beat that demonstrably succeeded", h.Since)
		}
	})

	// A transport failure in the middle of a 401 streak must neither advance nor clear it: nothing was
	// learned about the control plane's opinion, so nothing about it may change.
	t.Run("a transport failure mid-streak leaves the streak alone", func(t *testing.T) {
		dir := t.TempDir()
		mustRecord(t, dir, unreg, t0)
		mustRecord(t, dir, unreg, t0.Add(time.Minute))
		h := mustRecord(t, dir, errors.New("heartbeat: connection reset by peer"), t0.Add(2*time.Minute))
		if h.Consecutive401 != 2 {
			t.Fatalf("consecutive_401 = %d, want 2 — an unreachable control plane is not evidence of "+
				"acceptance (which would reset) nor of rejection (which would escalate)", h.Consecutive401)
		}
	})
}

// The sidecar is a CONTRACT BETWEEN TWO PROCESSES: the long-lived `router serve` writes it, and the
// short-lived `router status` (a separate `docker run`) reads it. Neither can see the other's memory,
// which is the entire reason the file exists.
func TestHeartbeatHealth_SidecarContract(t *testing.T) {
	t0 := time.Date(2026, 8, 23, 10, 4, 11, 0, time.UTC)

	// ⛔ IT MUST NOT BE state.json. statState fingerprints that file's content, so writing the
	// counter into it would make the state watcher fire on the daemon's OWN write, once per beat,
	// forever — a reload loop caused by the health reporting.
	t.Run("it is a sidecar, and state.json is never touched", func(t *testing.T) {
		dir := t.TempDir()
		if err := SaveState(dir, State{Port: 9765}); err != nil {
			t.Fatal(err)
		}
		before, err := os.Stat(filepath.Join(dir, "state.json"))
		if err != nil {
			t.Fatal(err)
		}
		mustRecord(t, dir, &RejectedError{Status: 401, Reason: "unregistered"}, t0)
		if _, err := os.Stat(filepath.Join(dir, heartbeatHealthFile)); err != nil {
			t.Fatalf("the sidecar was not written: %v", err)
		}
		after, err := os.Stat(filepath.Join(dir, "state.json"))
		if err != nil {
			t.Fatal(err)
		}
		if !after.ModTime().Equal(before.ModTime()) || after.Size() != before.Size() {
			t.Fatal("recording a beat outcome changed state.json — statState fingerprints exactly that " +
				"file, so the state watcher would now fire on the daemon's own write, every beat")
		}
	})

	// The wire keys are the contract; a reader in another process cannot negotiate them.
	t.Run("the file's keys are the declared contract", func(t *testing.T) {
		dir := t.TempDir()
		mustRecord(t, dir, &RejectedError{Status: 401, Reason: "bad_signature"}, t0)
		blob, err := os.ReadFile(filepath.Join(dir, heartbeatHealthFile))
		if err != nil {
			t.Fatal(err)
		}
		var raw map[string]any
		if err := json.Unmarshal(blob, &raw); err != nil {
			t.Fatalf("the sidecar is not valid JSON: %v", err)
		}
		for _, k := range []string{"consecutive_401", "since", "last_reason"} {
			if _, ok := raw[k]; !ok {
				t.Errorf("key %q missing; the file is a cross-process contract, not an internal dump", k)
			}
		}
	})

	// It records a rejection, which names the machine and its state — 0600, like every other
	// credential-adjacent file the router writes.
	t.Run("the file is 0600", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("POSIX file modes are not meaningful on Windows")
		}
		dir := t.TempDir()
		mustRecord(t, dir, &RejectedError{Status: 401}, t0)
		fi, err := os.Stat(filepath.Join(dir, heartbeatHealthFile))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("mode = %v, want 0600", fi.Mode().Perm())
		}
	})

	// ⛔ ABSENT IS NOT UNHEALTHY, AND IT IS NOT AN ERROR. A router that has not beaten yet — or has
	// never been rejected — has nothing to say here, and `router status` must not fail because of it.
	t.Run("an absent sidecar reports nothing and no error", func(t *testing.T) {
		h, present, err := LoadHeartbeatHealth(t.TempDir())
		if err != nil {
			t.Fatalf("absent sidecar returned an error: %v — a router that has not beaten yet is not "+
				"broken, and status must not fail on it", err)
		}
		if present {
			t.Error("present = true for a directory with no sidecar")
		}
		if h.Escalated() {
			t.Error("an absent sidecar escalated")
		}
	})

	// Same for a truncated or half-written file: it is missing information, never a fault to report.
	t.Run("an unparseable sidecar reports nothing and no error", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, heartbeatHealthFile), []byte("{not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		h, present, err := LoadHeartbeatHealth(dir)
		if err != nil {
			t.Fatalf("unparseable sidecar returned an error: %v", err)
		}
		if present || h.Escalated() {
			t.Errorf("present=%v escalated=%v, want false/false", present, h.Escalated())
		}
	})

	// What one process wrote, the other reads — asserted across a real write/read, not in memory.
	t.Run("what serve writes, status reads", func(t *testing.T) {
		dir := t.TempDir()
		for i := 0; i < 3; i++ {
			mustRecord(t, dir, &RejectedError{Status: 401, Reason: "unregistered"}, t0.Add(time.Duration(i)*time.Minute))
		}
		h, present, err := LoadHeartbeatHealth(dir)
		if err != nil || !present {
			t.Fatalf("present=%v err=%v", present, err)
		}
		if h.Consecutive401 != 3 || !h.Escalated() || h.LastReason != "unregistered" || !h.Since.Equal(t0) {
			t.Fatalf("read back %+v, want 3 / escalated / unregistered / %v", h, t0)
		}
	})
}

// ⛔ THE MESSAGE IS THE REQUIREMENT, NOT A NICETY. Under D5'+Q1 the reap DOES fire on silence, so a
// deleted row with a still-running router is reachable BY DESIGN — and in that state "restart it" is
// false advice, because a restart cannot recreate a row the control plane deleted.
//
// Only `unregistered` means re-onboarding. The signature reasons mean the KEY changed, where
// re-onboarding is a wildly disproportionate remedy for something `router register` fixes.
func TestHeartbeatHealth_EscalationMessage(t *testing.T) {
	esc := func(reason string) HeartbeatHealth {
		return HeartbeatHealth{Consecutive401: 3, Since: time.Now().Add(-time.Hour), LastReason: reason}
	}

	t.Run("unregistered names re-onboarding and never a restart", func(t *testing.T) {
		msg := strings.ToLower(esc("unregistered").EscalationMessage())
		if !strings.Contains(msg, "onboard") {
			t.Fatalf("the message does not name re-onboarding: %q", msg)
		}
		if strings.Contains(msg, "restart") {
			t.Fatalf("the message advises a restart, which CANNOT help once the control plane has "+
				"deleted the row — this is the exact false diagnostic VR9-I1 rule 4 exists to remove: %q", msg)
		}
	})

	t.Run("a signature rejection does not send the operator to re-onboard", func(t *testing.T) {
		for _, reason := range []string{"bad_signature", "expired", "malformed", "subject_mismatch"} {
			msg := strings.ToLower(esc(reason).EscalationMessage())
			if strings.Contains(msg, "onboard") {
				t.Errorf("reason %q advises re-onboarding; the key changed, and `router register` is "+
					"the proportionate remedy: %q", reason, msg)
			}
			if msg == "" {
				t.Errorf("reason %q produced no message at all", reason)
			}
		}
	})

	// ⛔ THE TWO-PLANE WINDOW AGAIN. An OLD control plane sends no reason on two of the three branches,
	// so the router learns "401" and nothing else. It must still escalate — it IS being rejected — but
	// it must not invent which remedy applies.
	t.Run("an unknown reason escalates without claiming a remedy", func(t *testing.T) {
		msg := esc("").EscalationMessage()
		if msg == "" {
			t.Fatal("a 401 with no reason produced no message; the machine is still being rejected")
		}
		low := strings.ToLower(msg)
		if strings.Contains(low, "onboard") || strings.Contains(low, "restart") {
			t.Fatalf("the message names a remedy the router has no evidence for: %q", msg)
		}
	})

	t.Run("no message below the threshold", func(t *testing.T) {
		h := HeartbeatHealth{Consecutive401: 2, LastReason: "unregistered"}
		if got := h.EscalationMessage(); got != "" {
			t.Fatalf("EscalationMessage = %q below the threshold, want empty", got)
		}
	})
}

// The classifier reads the control plane's ANSWER, so it is driven by a server rather than by a
// hand-built error: the failure mode being closed is a router that cannot tell WHY it was refused.
func TestSend_ClassifiesTheRejection(t *testing.T) {
	id, err := LoadOrCreateIdentity(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name       string
		status     int
		body       string
		wantReason string
	}{
		{"unregistered", 401, `{"error":"this router is not registered","reason":"unregistered"}`, "unregistered"},
		{"bad signature", 401, `{"reason":"bad_signature","server_time":"2026-08-23T10:00:00Z"}`, "bad_signature"},
		{"subject mismatch", 401, `{"error":"subject does not match","reason":"subject_mismatch"}`, "subject_mismatch"},
		{"an old control plane sends no reason", 401, `{"error":"nope"}`, ""},
		{"a 503 is not a rejection", 503, `upstream unavailable`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(c.status)
				w.Write([]byte(c.body))
			}))
			defer srv.Close()
			hb := Heartbeat{CPURL: srv.URL, Host: "PC-1", Port: 9765}
			serr := hb.Send(id, time.Now())
			if serr == nil {
				t.Fatal("Send returned nil for a non-200 answer")
			}
			var re *RejectedError
			if !errors.As(serr, &re) {
				t.Fatalf("Send returned %T (%v), want a *RejectedError — the caller cannot decide "+
					"whether this was a DECISION or a network event without the status", serr, serr)
			}
			if re.Status != c.status {
				t.Errorf("status = %d, want %d", re.Status, c.status)
			}
			if re.Reason != c.wantReason {
				t.Errorf("reason = %q, want %q", re.Reason, c.wantReason)
			}
		})
	}
}

func mustRecord(t *testing.T, dir string, err error, now time.Time) HeartbeatHealth {
	t.Helper()
	h, rerr := RecordBeatOutcome(dir, "", "", err, now)
	if rerr != nil {
		t.Fatalf("RecordBeatOutcome: %v", rerr)
	}
	return h
}

// ⛔ AND IT MUST ACTUALLY BE WIRED. Every part above can be correct while the beat loop never calls the
// recorder — the sidecar would then be a perfectly-specified file that nothing ever writes, and
// `router status` would report health forever on a machine being refused every 60 seconds. That is the
// same shape of defect as VR9-I1 rule 1 (a value read once, in a place no test could see), so it gets
// its own assertion ON THE WORLD: the file, on disk, after a real loop against a real refusing server.
func TestRunHeartbeat_RecordsTheEscalationOnDisk(t *testing.T) {
	dir := t.TempDir()
	id, err := LoadOrCreateIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"this router is not registered","reason":"unregistered"}`))
	}))
	defer srv.Close()

	var mu sync.Mutex
	var errs []string
	stop := make(chan struct{})
	var stopOnce sync.Once
	stopLoop := func() { stopOnce.Do(func() { close(stop) }) }
	done := make(chan struct{})
	go func() {
		defer close(done)
		RunHeartbeat(
			Heartbeat{CPURL: srv.URL, Host: "PC-1", Port: 9765, Interval: 10 * time.Millisecond, StateDir: dir,
				Identity: func() Identity { return id }}, stop,
			func(e error) { mu.Lock(); errs = append(errs, e.Error()); mu.Unlock() })
	}()
	// Closing stop does not stop a beat already in flight: it can still be writing the health file
	// into dir when the test returns, and TempDir's RemoveAll then fails "directory not empty"
	// (CI run 36336014674). Cleanups run LIFO, so this one waits the loop out before dir goes.
	t.Cleanup(func() { stopLoop(); <-done })

	deadline := time.After(3 * time.Second)
	for {
		h, present, _ := LoadHeartbeatHealth(dir)
		if present && h.Escalated() {
			stopLoop()
			if h.LastReason != "unregistered" {
				t.Fatalf("last_reason = %q, want unregistered", h.LastReason)
			}
			// The operator watching the log must be told too — `router status` is a question someone
			// has to think to ask, and nobody asks it about a machine they believe is fine.
			//
			// The loop writes the file FIRST and calls the sink after, so the file can show the
			// escalation a moment before the sink has heard of it. Wait for the sink rather than reading
			// it once: CI failed here on the gap (run 36010212797), not on a missing call.
			var joined string
			for {
				mu.Lock()
				joined = strings.ToLower(strings.Join(errs, " | "))
				mu.Unlock()
				if strings.Contains(joined, "onboard") {
					return
				}
				select {
				case <-deadline:
					t.Fatalf("the beat loop never surfaced the escalation to its error sink; it saw: %q", joined)
				case <-time.After(10 * time.Millisecond):
				}
			}
		}
		select {
		case <-deadline:
			stopLoop()
			t.Fatalf("no escalation was recorded on disk after 3s of 401s (present=%v, %+v) — the beat "+
				"loop is not calling the recorder, so the sidecar is a file nothing ever writes", present, h)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// V27-009: one heartbeat loop per record means one sidecar per record. A clean beat for one account
// must not reset another account's streak on the same machine, and status reports the worst record.
func TestHeartbeatHealth_OneSidecarPerRecord_WorstWins(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 9, 6, 1, 0, 0, 0, time.UTC)
	rej := &RejectedError{Status: 401, Reason: "unregistered"}
	for i := 0; i < 3; i++ {
		if _, err := RecordBeatOutcome(dir, "https://cp/mcp", "user_a", rej, now.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := RecordBeatOutcome(dir, "https://cp/mcp", "user_b", rej, now); err != nil {
		t.Fatal(err)
	}
	if _, err := RecordBeatOutcome(dir, "https://cp/mcp", "user_b", nil, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	all := LoadAllHeartbeatHealth(dir)
	if len(all) != 2 {
		t.Fatalf("want 2 sidecars (one per record), got %d: %+v", len(all), all)
	}
	worst, present, _ := LoadHeartbeatHealth(dir)
	if !present || worst.Consecutive401 != 3 || worst.RecordUser != "user_a" {
		t.Fatalf("the worst record must win: present=%v %+v", present, worst)
	}
	if !worst.Escalated() {
		t.Fatal("three 401s on one record must escalate even though the other record is clean")
	}
	for _, h := range all {
		if h.RecordUser == "user_b" && h.Consecutive401 != 0 {
			t.Fatalf("user_b's clean beat did not reset its own streak: %+v", h)
		}
	}
}
