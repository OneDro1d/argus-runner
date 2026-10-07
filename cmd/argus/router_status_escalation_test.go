package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/router"
)

// VR9-I1 rule 4 — "Surface it in `router status`."
//
// ── WHY THIS IS THE ASSERTION THAT MATTERS ────────────────────────────────────────────────────────
//
// `router serve` is a long-lived daemon and `router status` is a separate, short-lived `docker run`.
// They share no memory, so the counter reaching an operator depends entirely on the sidecar crossing
// the process boundary. Everything either side of that boundary can be perfect while the boundary
// itself is never crossed — and the symptom would be a status command reporting nothing wrong about a
// machine the control plane has been refusing every 60 seconds. That is this round's whole theme: a
// value that is present, well-formed, and empty of information.
//
// ⛔ SO THIS DRIVES cmdRouterStatus ITSELF and reads what it PRINTS, rather than asserting that the
// reader function works. The reader is already tested in internal/router; what is untested is whether
// anything calls it.
func TestRouterStatus_SurfacesTheEscalation(t *testing.T) {
	t0 := time.Date(2026, 8, 23, 10, 4, 11, 0, time.UTC)

	t.Run("an escalated router says so, and names re-onboarding", func(t *testing.T) {
		dir := t.TempDir()
		if err := router.SaveState(dir, router.State{Port: 9765}); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 3; i++ {
			if _, err := router.RecordBeatOutcome(dir, "", "",
				&router.RejectedError{Status: 401, Reason: "unregistered"},
				t0.Add(time.Duration(i)*time.Minute)); err != nil {
				t.Fatal(err)
			}
		}
		out := captureEmit(t, func() {
			if rc := cmdRouterStatus([]string{"--state", dir}); rc != 0 {
				t.Fatalf("router status exited %d", rc)
			}
		})
		streak, ok := out["heartbeat_401_streak"]
		if !ok {
			t.Fatalf("router status printed no escalation at all: %v — the sidecar crossed no process "+
				"boundary, so the operator's only view of a rejected router is still silence", out)
		}
		if n, _ := streak.(float64); int(n) != 3 {
			t.Errorf("heartbeat_401_streak = %v, want 3", streak)
		}
		note, _ := out["heartbeat_note"].(string)
		if !strings.Contains(strings.ToLower(note), "onboard") {
			t.Errorf("heartbeat_note = %q — an escalated status must carry the REMEDY, not just a "+
				"count. A number with no next step is a puzzle, not a diagnostic.", note)
		}
		if reason, _ := out["heartbeat_401_reason"].(string); reason != "unregistered" {
			t.Errorf("heartbeat_401_reason = %q, want unregistered", reason)
		}
	})

	// ⛔ BELOW THE THRESHOLD IT REPORTS THE STREAK BUT CLAIMS NOTHING. Two 401s is a fact worth seeing
	// while debugging; it is not yet a diagnosis, and printing the remedy at 1 would train operators to
	// re-onboard on any blip.
	t.Run("below the threshold there is a count but no remedy", func(t *testing.T) {
		dir := t.TempDir()
		if err := router.SaveState(dir, router.State{Port: 9765}); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 2; i++ {
			if _, err := router.RecordBeatOutcome(dir, "", "",
				&router.RejectedError{Status: 401, Reason: "unregistered"},
				t0.Add(time.Duration(i)*time.Minute)); err != nil {
				t.Fatal(err)
			}
		}
		out := captureEmit(t, func() { _ = cmdRouterStatus([]string{"--state", dir}) })
		if n, _ := out["heartbeat_401_streak"].(float64); int(n) != 2 {
			t.Errorf("heartbeat_401_streak = %v, want 2", out["heartbeat_401_streak"])
		}
		if note, _ := out["heartbeat_note"].(string); note != "" {
			t.Errorf("heartbeat_note = %q below the threshold, want none", note)
		}
	})

	// ⛔ ABSENCE IS NOT HEALTH. A router that has never beaten has no record, and the status must not
	// render that as "fine" — it must say it has nothing to report. This project has shipped the
	// opposite mistake before: a missing signal drawn as a healthy one.
	t.Run("no record says so, rather than reading as healthy", func(t *testing.T) {
		dir := t.TempDir()
		if err := router.SaveState(dir, router.State{Port: 9765}); err != nil {
			t.Fatal(err)
		}
		out := captureEmit(t, func() {
			if rc := cmdRouterStatus([]string{"--state", dir}); rc != 0 {
				t.Fatalf("router status exited %d on a router with no beat record — a missing "+
					"diagnostic file must never make the diagnostic tool fail", rc)
			}
		})
		if _, ok := out["heartbeat_401_streak"]; ok {
			t.Errorf("a streak was reported for a router that has never beaten: %v", out["heartbeat_401_streak"])
		}
		health, _ := out["heartbeat_health"].(string)
		if health == "" {
			t.Fatal("nothing at all was said about the heartbeat. Silence here reads as health, and " +
				"this router may have been refused since boot — status simply cannot see it yet.")
		}
		if strings.Contains(strings.ToLower(health), "ok") || strings.Contains(strings.ToLower(health), "healthy") {
			t.Errorf("heartbeat_health = %q claims health from an ABSENT record", health)
		}
	})

	// An unparseable sidecar is missing information, not a fault: status still exits 0.
	t.Run("an unreadable record never fails the command", func(t *testing.T) {
		dir := t.TempDir()
		if err := router.SaveState(dir, router.State{Port: 9765}); err != nil {
			t.Fatal(err)
		}
		if err := writeFileForTest(dir, "heartbeat-health.json", "{truncated"); err != nil {
			t.Fatal(err)
		}
		out := captureEmit(t, func() {
			if rc := cmdRouterStatus([]string{"--state", dir}); rc != 0 {
				t.Fatalf("router status exited %d on an unparseable sidecar", rc)
			}
		})
		if _, ok := out["heartbeat_401_streak"]; ok {
			t.Error("a streak was reported from an unparseable file")
		}
	})
}

func writeFileForTest(dir, name, body string) error {
	return os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600)
}
