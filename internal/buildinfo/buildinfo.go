// Package buildinfo carries the identity of THIS BUILD — stamped at link time, not guessed at
// runtime (F12 / UC071, CP-M3-III-68).
//
// THE DEFECT (Stage III gate)
// Every executor in the estate reported version "0.1.0". Not because they were all the same build —
// they were not — but because the number came from a hardcoded default in the deployment templates
// (docker-compose.byo-m3.yml `${ARGUS_VERSION:-0.1.0}`, k8srender's `in.Version = "0.1.0"`) and
// nothing anywhere derived it from the binary.
//
// That makes several product promises unkeepable at once, and they all failed silently:
//   - UC071's outdated-blocked mode cannot fire — nothing can be BELOW a minimum when everything
//     reports the same number;
//   - the web's EXECUTOR row cannot show "current" vs "behind"; it showed "0.1.0" to every instance
//     no matter how old;
//   - update.sh cannot tell whether an update actually changed anything;
//   - and an operator asking "what is this thing running?" was told a constant.
//
// A version that is the same for every build is not a version. This package makes it come from the
// build, so that the answer changes when the thing being described changes.
package buildinfo

import (
	"runtime/debug"
	"strings"
)

// Set at link time:
//
//	go build -ldflags "-X github.com/OneDro1d/argus-runner/internal/buildinfo.Version=<v> \
//	                   -X github.com/OneDro1d/argus-runner/internal/buildinfo.Commit=<sha> \
//	                   -X github.com/OneDro1d/argus-runner/internal/buildinfo.Date=<iso8601>"
//
// Empty in a plain `go build` (a developer's local binary), which is honest: that build has no
// release identity, and Version() says so rather than inventing one.
var (
	Version string
	Commit  string
	Date    string
)

// devVersion is what an UNSTAMPED build reports. It is deliberately not a plausible-looking release
// number: "0.1.0" reads like a version somebody chose, and that is exactly the confusion this
// package exists to end.
const devVersion = "0.0.0-dev"

// Version resolves the version this binary should report, in precedence order:
//
//  1. an explicit operator override (ARGUS_VERSION) — pinning or an experiment
//  2. the LINK-TIME stamp — the normal answer for anything built by the release path
//  3. the VCS revision Go embeds automatically — covers `go build` from a clean checkout
//  4. "0.0.0-dev" — genuinely unknown, and says so
//
// The override stays FIRST because operators occasionally need to lie to the version check on
// purpose (pinning during an incident). It is a deliberate act with a visible cause, unlike the
// hardcoded template default it replaces, which nobody chose and nobody could see.
// RenderedOverride is what a RENDERED MANIFEST must carry for ARGUS_VERSION: the operator's
// override verbatim, and EMPTY when they set none — never this binary's own stamp.
//
// INT-001 (2026-08-08). render-k8s used Resolve() here, so with no override set the ONBOARDING KIT's
// build stamp was baked into the executor Deployment as a literal. ARGUS_VERSION has the highest
// precedence inside the executor (see Resolve below), and update.sh:256 on the k8s path only runs
// `kubectl set image` — it never touches the env. Change the image, keep the env, and the executor runs
// binary A while reporting version B. Reproduced live: a pod running m3-iii32 reported
// "0.3.0+m3-iii34 (commit 5c67835, ...)" — a version and a commit that contradict each other inside one
// string.
//
// It matters because it fires on exactly the operations F14/F15 exist to perform: after a self-update
// the CP evaluates F13's minimum and F11's outdated-blocking against a stale string, so an executor
// below the minimum reports itself `current` and keeps taking work.
//
// Resolve() is still correct for what THIS process reports about ITSELF (the `version` command, the
// CP's CurrentVersion, a runner's own RunnerVersion). The distinction is the whole fix: reporting your
// own version is not the same act as stamping someone else's manifest.
func RenderedOverride(env string) string { return strings.TrimSpace(env) }

func Resolve(override string) string {
	if v := strings.TrimSpace(override); v != "" {
		return v
	}
	if v := strings.TrimSpace(Version); v != "" {
		return v
	}
	if v := vcsVersion(); v != "" {
		return v
	}
	return devVersion
}

// vcsVersion reads the revision Go stamps into any binary built inside a git work tree. It means a
// developer's `go build` still produces a DISTINGUISHABLE version without anyone remembering to pass
// ldflags — which matters, because the failure mode being fixed is precisely "everything reports the
// same thing and nobody notices".
func vcsVersion() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	var rev string
	var dirty bool
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		return ""
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	out := "0.0.0-src+" + rev
	if dirty {
		// An uncommitted build is not reproducible and must never be mistaken for the commit it
		// resembles.
		out += ".dirty"
	}
	return out
}

// Full is the human-facing detail line: version, commit and build date when known.
func Full(override string) string {
	v := Resolve(override)
	var extra []string
	if Commit != "" {
		extra = append(extra, "commit "+Commit)
	}
	if Date != "" {
		extra = append(extra, "built "+Date)
	}
	if len(extra) == 0 {
		return v
	}
	return v + " (" + strings.Join(extra, ", ") + ")"
}
