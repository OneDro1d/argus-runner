package router

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// VR9-I1 rule 4 — a persistent rejection is REPORTED, not absorbed.
//
// ── THE PROBLEM THIS SOLVES ───────────────────────────────────────────────────────────────────────
//
// A heartbeat that does not land is deliberately non-fatal (see RunHeartbeat): the router's job is
// routing, and an agent whose router works does not care what the cloud believes. That decision is
// right and it stays. Its cost is that a router rejected FOREVER is indistinguishable from a healthy
// one — which is exactly how `router serve` signing with a stale key survived undetected.
//
// So the outcome of every beat is recorded, and three consecutive REJECTIONS become something an
// operator can read.
//
// ⛔ THE DISTINCTION THAT CARRIES THE WHOLE DESIGN: a 401 is a DECISION by the control plane about
// this machine's identity. A 503 during a control-plane rollout, or a refused connection, is a network
// event that merely arrives over HTTP. Escalating on `status >= 400` would raise an alarm on every
// routine CP restart telling the operator to re-onboard a machine that is perfectly fine — and an
// alarm that cries wolf is worse than no alarm, because it teaches people to skip the real one.

// heartbeatHealthFile is the sidecar's name inside the state dir.
//
// ⛔ A SIDECAR, AND NEVER state.json. The escalation must be readable by `router status`, which is a
// SEPARATE, short-lived process (a `docker run`) that cannot see the daemon's memory. The obvious
// alternative — a field in state.json — is a trap: statState fingerprints that file's content,
// so the daemon writing its own counter there would trip the state watcher on every beat, forever.
// statState reads `state.json` specifically, so a neighbouring file is invisible to it.
const heartbeatHealthFile = "heartbeat-health.json"

// heartbeatHealthFileFor names the sidecar of ONE record (control plane, user). V27-009 (0.3.29) runs one
// heartbeat loop per record, and two loops sharing a file would overwrite each other's streak: a machine
// refused for one account and accepted for another would flip between "escalated" and "clean" every
// beat. A nameless record (tests, a legacy single-record state) keeps the unkeyed file.
func heartbeatHealthFileFor(url, user string) string {
	if url == "" && user == "" {
		return heartbeatHealthFile
	}
	sum := sha256.Sum256([]byte(url + "\x00" + user))
	return "heartbeat-health." + hex.EncodeToString(sum[:6]) + ".json"
}

// EscalateAfter401s is D7's threshold: three consecutive rejections.
const EscalateAfter401s = 3

// HeartbeatHealth is the cross-process contract between `router serve` (writer) and `router status`
// (reader). The JSON keys ARE the contract — a reader in another process cannot negotiate them.
type HeartbeatHealth struct {
	// Consecutive401 counts the CURRENT streak, never a lifetime. A lifetime counter escalates after
	// three unrelated blips spread over a month, which is an alarm about nothing.
	Consecutive401 int `json:"consecutive_401"`
	// Since is when the current streak began — the operator's real question is how long this has been
	// broken, not when it was last checked.
	Since time.Time `json:"since"`
	// LastReason distinguishes the control plane's 401 branches, because ONLY "unregistered" means
	// re-onboarding is the remedy. See EscalationMessage.
	LastReason string `json:"last_reason"`
	// RecordURL and RecordUser name the record this streak belongs to (V27-009: one sidecar per record).
	RecordURL  string `json:"record_url,omitempty"`
	RecordUser string `json:"record_user,omitempty"`
}

// Escalated reports whether the streak has reached D7's threshold.
func (h HeartbeatHealth) Escalated() bool { return h.Consecutive401 >= EscalateAfter401s }

// EscalationMessage is what an operator should be told, or "" below the threshold.
//
// ⛔ THE MESSAGE IS THE REQUIREMENT. Under D5'+Q1 the reap fires on silence, so "the control plane
// deleted this router's row while the router is still running" is reachable BY DESIGN — and in that
// state "restart it" is FALSE ADVICE, because a restart cannot recreate a row that is gone. That false
// diagnostic is the thing rule 4 exists to remove, so the wording is load-bearing, not cosmetic.
func (h HeartbeatHealth) EscalationMessage() string {
	if !h.Escalated() {
		return ""
	}
	head := fmt.Sprintf("the control plane has refused this machine's last %d heartbeats", h.Consecutive401)
	if !h.Since.IsZero() {
		head += " (since " + h.Since.UTC().Format(time.RFC3339) + ")"
	}
	switch h.LastReason {
	case "unregistered":
		// The row is gone. Nothing local can recreate it — onboarding is what registers a machine.
		return head + ": this machine is no longer registered. Onboard it again — nothing done " +
			"locally can recreate a registration the control plane no longer has."
	case "bad_signature", "expired", "malformed", "subject_mismatch":
		// The registration exists; the KEY no longer matches it. Re-onboarding would be a wildly
		// disproportionate remedy for something `argus router register` fixes.
		return head + ": its identity key no longer matches the one registered. Register this machine's " +
			"router again against the same control-plane account."
	default:
		// ⛔ AN OLD CONTROL PLANE SENDS NO REASON on two of the three branches, so all the router knows
		// is "refused". It must still escalate — it IS being rejected — but it must not invent which
		// remedy applies. Naming the wrong one costs an operator a teardown they did not need.
		return head + " without saying why. Check the control plane's own logs for this machine before " +
			"changing anything here."
	}
}

// RejectedError is a NON-200 answer from the control plane, carrying the two facts the caller needs to
// decide what it means: the status (was this a decision, or a transport-level event?) and the reason
// (which decision?).
//
// ⚠ Before this existed, Send returned `fmt.Errorf("heartbeat: control plane answered %d", status)` — a
// string. A caller could not act on it without parsing prose, so it acted on nothing.
type RejectedError struct {
	Status int
	// Reason is the control plane's machine-readable reason, or "" when it did not send one — which is
	// the ordinary state during a two-plane rollout, not an anomaly.
	Reason string
	// Body is the truncated response body, for the operator's error line only.
	Body string
}

func (e *RejectedError) Error() string {
	msg := fmt.Sprintf("heartbeat: control plane answered %d", e.Status)
	if e.Reason != "" {
		msg += " (" + e.Reason + ")"
	}
	return msg
}

// Rejected reports whether this was a 401 — a DECISION about this machine's identity — as opposed to
// any other non-200 answer, which is an event that merely arrived over HTTP.
func (e *RejectedError) Rejected() bool { return e.Status == 401 }

// RecordBeatOutcome folds one beat's outcome into the record's sidecar and returns the new health.
//
// The three cases are deliberately NOT symmetric:
//
//   - accepted (err == nil) -> the streak RESETS, `since` and the reason clear with it. Leaving `since`
//     behind would tell the operator an outage began before a beat that demonstrably succeeded.
//   - a 401 -> the streak advances; `since` is set on the first 401 of the streak and never moved.
//   - anything else -> UNCHANGED, and the file is not rewritten. An unreachable control plane is not
//     evidence of acceptance (which would reset) nor of rejection (which would escalate). Nothing was
//     learned, so nothing may change.
//
// url + user name the record (V27-009); each record has its own sidecar, so one record's clean beat
// never resets another record's streak.
func RecordBeatOutcome(dir, url, user string, beatErr error, now time.Time) (HeartbeatHealth, error) {
	name := heartbeatHealthFileFor(url, user)
	cur, _, _ := loadHeartbeatHealthFile(dir, name)

	switch {
	case beatErr == nil:
		next := HeartbeatHealth{RecordURL: url, RecordUser: user}
		if cur == next {
			return next, nil // already clean; do not churn the file every 60s
		}
		return next, saveHeartbeatHealth(dir, name, next)
	case isRejection(beatErr):
		next := cur
		next.RecordURL, next.RecordUser = url, user
		next.Consecutive401++
		if cur.Consecutive401 == 0 {
			next.Since = now
		}
		next.LastReason = rejectionReason(beatErr)
		return next, saveHeartbeatHealth(dir, name, next)
	default:
		return cur, nil
	}
}

func isRejection(err error) bool {
	var re *RejectedError
	return errors.As(err, &re) && re.Rejected()
}

func rejectionReason(err error) string {
	var re *RejectedError
	if errors.As(err, &re) {
		return re.Reason
	}
	return ""
}

// LoadHeartbeatHealth reads the sidecars BEST-EFFORT and returns the WORST record's health — the
// longest 401 streak (the earliest `since` on a tie). With one record on the machine that is simply its
// sidecar; with several, `router status` also lists them all (LoadAllHeartbeatHealth).
//
// ⛔ ABSENT IS NOT UNHEALTHY, AND IT IS NEVER AN ERROR. A router that has not beaten yet, or has never
// been rejected, has nothing to say here — and `router status` failing because of a missing file would
// make the diagnostic tool the thing that breaks during a diagnosis. Same for a truncated or
// half-written file: that is missing information, not a fault to report.
//
// The bool says whether anything was actually read, so the caller can distinguish "no record" from "a
// record saying zero" and avoid rendering the first as health.
func LoadHeartbeatHealth(dir string) (HeartbeatHealth, bool, error) {
	all := LoadAllHeartbeatHealth(dir)
	if len(all) == 0 {
		return HeartbeatHealth{}, false, nil
	}
	worst := all[0]
	for _, h := range all[1:] {
		if h.Consecutive401 > worst.Consecutive401 ||
			(h.Consecutive401 == worst.Consecutive401 && !h.Since.IsZero() && (worst.Since.IsZero() || h.Since.Before(worst.Since))) {
			worst = h
		}
	}
	return worst, true, nil
}

// LoadAllHeartbeatHealth reads every readable sidecar in the state dir, one per record, in file-name
// order. Unreadable or half-written files are skipped, never reported.
func LoadAllHeartbeatHealth(dir string) []HeartbeatHealth {
	names, _ := filepath.Glob(filepath.Join(dir, "heartbeat-health*.json"))
	var out []HeartbeatHealth
	for _, n := range names {
		if h, ok, _ := loadHeartbeatHealthFile(dir, filepath.Base(n)); ok {
			out = append(out, h)
		}
	}
	return out
}

func loadHeartbeatHealthFile(dir, name string) (HeartbeatHealth, bool, error) {
	blob, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return HeartbeatHealth{}, false, nil
	}
	var h HeartbeatHealth
	if err := json.Unmarshal(blob, &h); err != nil {
		return HeartbeatHealth{}, false, nil
	}
	return h, true, nil
}

// saveHeartbeatHealth writes one record's sidecar at 0600 — it records a rejection, which names this
// machine and its identity state, so it gets the same mode as every other credential-adjacent file the
// router writes. Written via a temp file + rename so `router status` never reads a half-written record.
func saveHeartbeatHealth(dir, name string, h HeartbeatHealth) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	blob, err := json.MarshalIndent(h, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, name+".tmp")
	if err := os.WriteFile(tmp, append(blob, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, name))
}
