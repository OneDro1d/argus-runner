package main

import (
	"errors"
	"net/http"

	"github.com/OneDro1d/argus-runner/internal/envname"
	"github.com/OneDro1d/argus-runner/internal/onboard"
)

// The `diagnose` line a refusal carries when `argus doctor` diagnoses its cause. A separate key, not
// folded into "error" or "hint": those texts are grepped by scripts and tests, and stay byte-identical.
//
// ⛔ WHY IT EXISTS. Measured on dev f2be4df (2026-09-30): outside doctor's own files, no message any
// command printed named `argus doctor`, and the bare `argus` listing gave it no description. The tool
// that turns "denied" into "which of the three variables is wrong, and the export that fixes it" was
// reachable only by someone who already knew it existed — which is the one person who did not need it.
//
// ⚠ Point at doctor ONLY from a refusal doctor actually diagnoses. A pointer to a diagnosis that then
// reports all-ok is worse than none: it teaches the reader to stop following it.
const (
	// the local (M2.5) hat gate: doctor's local-hats check (internal/doctor/hats.go CheckLocalHats).
	doctorPointerLocalHats = "argus doctor — read-only; its local-hats check says which of ARGUS_TOKEN, ARGUS_RUNNER_TOKEN, " +
		envname.ExecutorSecret + " is missing or mismatched (they are a role map: any two distinct strings), and the export that fixes it"
)

// cpForDiagnosis is the control plane the running cloud-* command talks to. sessionForCommand, which
// every one of them calls, records it, so the pointer on a refusal is a command that runs as printed —
// the same rule doctor holds its own fixes to.
var cpForDiagnosis string

// doctorPointerCP is the pointer for a control plane that refused the credential: doctor's
// control-plane-credential check. Without a recorded control plane it prints the flag's placeholder
// rather than guess a URL.
func doctorPointerCP() string {
	// onboard.ControlPlaneForCommand is the ONE renderer (scheme and host required; userinfo, query and fragment
	// dropped; shell-quoted when needed; the placeholder for anything else), shared with the `error` text of the
	// same refusal.
	cp := onboard.ControlPlaneForCommand(cpForDiagnosis)
	return "argus doctor --control-plane " + cp + " — read-only; it says which credential this command presented " +
		"(ARGUS_CP_AUTHOR_TOKEN outranks the session file), whether this control plane accepts it and as what, and the fix"
}

// displayControlPlane is the control-plane URL for a line that is read, not pasted: the credential banner and
// the doctor's check subjects. One implementation, in onboard.
func displayControlPlane(raw string) string { return onboard.ControlPlaneForDisplay(raw) }

// refusedCredential reports whether an emitErr argument is a control plane refusing the credential:
// HTTP 401 or 403 anywhere in an error's chain, or onboard.ErrNoRefresh. ErrNoRefresh is NOT itself a
// control-plane refusal: it means there is no refresh token locally. A static token (--token,
// ARGUS_CP_AUTHOR_TOKEN) answered 401 — the wrong-PAT case, the one that most needs the pointer (verify
// 2026-09-29, R4) — carries BOTH the 401 and ErrNoRefresh (Session.refusedWithNothingToRefresh), so the 401
// check catches it. ErrNoRefresh alone is Session.refreshOnce on the proactive path: an access token near
// expiry, no refresh token, and no request sent (a static JWT, or a session that expired with `cloud-login`
// never run). Either way the credential in hand cannot be used as-is, which doctor's control-plane-credential
// check reports.
//
// Decided from the error's type, never its text. A 5xx, a timeout, a 400 or a 404 is not a verdict on
// the credential, and doctor would report the credential fine — so they get no pointer.
//
// ⚠ A 403 is not always about the credential either: "not your workspace" (internal/control/web.go
// handleSwitch, oauth/endpoints.go ownsOrRefuse) is a 403 on a credential doctor reports fine. By type
// the two 403s are the same, so the pointer promises only what doctor answers in BOTH cases — whether
// the credential is accepted, and as what — and never a cause. "Accepted, as an author" still halves
// the search: what was refused is the resource, not the credential.
func refusedCredential(args []any) bool {
	for _, a := range args {
		err, ok := a.(error)
		if !ok {
			continue
		}
		if s := onboard.StatusOf(err); s == http.StatusUnauthorized || s == http.StatusForbidden {
			return true
		}
		if errors.Is(err, onboard.ErrNoRefresh) {
			return true
		}
	}
	return false
}
