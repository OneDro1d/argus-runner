package doctor

import (
	"strings"
	"time"

	"github.com/OneDro1d/argus-runner/internal/federation"
)

// ExecutorInput is what the control plane said about the executors in the caller's workspace, read
// with the caller's OWN credential (GET /api/instances, the list the Environments page shows). The
// caller decides whether it could ask at all; this package does no I/O.
type ExecutorInput struct {
	// ControlPlaneURL is the control plane that was asked (or would have been); used in Subject and fixes.
	ControlPlaneURL string
	// NotAsked says why the control plane was NOT asked: no URL, no usable credential. "" = it was asked.
	NotAsked string
	// Err is set when the control plane was asked and did not give an answer; ErrFix, when the caller
	// knows a better fix for it than "re-run" (a failed renewal needs a sign-in).
	Err, ErrFix string
	// CredentialNote says what was done to the credential to ask (an in-memory renewal); shown in Subject.
	CredentialNote string
	// Accepted says GET /api/instances answered with the caller's credential: the control plane's verdict on it.
	Accepted  bool
	Workspace string
	Executors []ExecutorStatus
	// InstanceID narrows the report to one executor (--instance / ARGUS_INSTANCE_ID); "" = every one.
	InstanceID string
	// HostDocker is whether a Docker daemon answers on THIS host, which the control plane's update block
	// needs. The zero value is "not probed", and then the report says nothing about Docker.
	HostDocker DockerProbe
	// Now is the clock sut-reachable measures a reading's age against; nil = time.Now. The package does
	// no I/O, and a test needs a time that does not move.
	Now func() time.Time
	// ConfigTargets is how many targets the --config argus-config declares (the enumeration
	// validate-config probes); nil = no --config, or it did not parse, so nothing is known about it.
	ConfigTargets *int
}

// DockerProbe is what the caller found out about a Docker daemon on this host. The control plane
// renders ONE update block for every host (DEC-U1: `docker pull` the new image, then `docker run` it
// to compute the update), so a host with no daemon (a Coder workspace onboarded --native) cannot run it.
type DockerProbe struct {
	Probed  bool   // false = not asked: unknown, never "absent"
	Answers bool   // a daemon answered
	Reason  string // why none answered, in Docker's words; shown in the fix
}

// ExecutorStatus is one row of GET /api/instances, reduced to what the version check reads. The JSON
// names are the control plane's (internal/control/instanceview.go), so a row decodes straight into it.
type ExecutorStatus struct {
	InstanceID           string `json:"instance_id"`
	RunnerVersion        string `json:"runner_version"`
	VersionState         string `json:"version_state"`
	VersionUnknownReason string `json:"version_unknown_reason"`
	MinSupported         string `json:"min_supported_version"`
	MinRecommended       string `json:"min_recommended_version"`
	RecommendedImage     string `json:"recommended_image"`
	UpdateCommand        string `json:"update_command"`
	// SUTState is the control plane's verdict on the executor's own SUT reachability probe (VR6-W1):
	// ready | unreachable | not_checked, staleness already applied (internal/control/sutstate.go).
	// "" = this control plane does not publish it. SUTCheckedAt is when the probe last ran; nil = never.
	SUTState     string     `json:"sut_state"`
	SUTCheckedAt *time.Time `json:"sut_checked_at"`
}

// knownFix is an executor fix a first run has tripped over when it was missing, with the release it
// first shipped in (`git tag --contains <merge commit>`). Caution marks a behaviour change — RIGHT, but
// it turns something red that was green — and says what to check before updating across it.
type knownFix struct {
	Version, ID, Effect string
	Caution             string
}

// knownFixes is hand-kept and deliberately short: only fixes whose absence was recorded as first-run
// friction (the incident rows in the replay tests), plus behaviour changes an update walks into. In
// release order. A fix that is not here is not reported missing — this is a list of known traps, not
// a changelog.
var knownFixes = []knownFix{
	{Version: "0.3.37", ID: "#135", Effect: `a chain's "always": true cleanup steps run on mcp steps too (before it, a chain that failed midway left what it had created behind)`},
	{Version: "0.3.37", ID: "#138 (AC-D27)", Effect: "a passing Web UI scenario no longer lists its evaluated checks as unexecuted"},
	{Version: "0.3.37", ID: "#140 (issue #139)", Effect: `a "not evaluated" Web UI row keeps the Playwright cause, so the report says why`},
	{Version: "0.3.37", ID: "#160 (#161)", Effect: "a quote or backslash in a ${VAR} value no longer empties a Web UI scenario's payload " +
		"(the run fell back, silently, to the TRIGGER url and the ambient APP_URL), and each ${VAR} is resolved once, not twice"},
	{Version: "0.3.37", ID: "#209 (AC-D43)", Effect: `a run is no longer abandoned as "executor lost" when freshly scaled replicas scale the executor back down mid-run`},
	{Version: "0.3.40", ID: "#286", Effect: "author_get_report reaches the executor, so an author can read why a check failed, not only that it did (from control plane rev 27, #316, with no runner pairing)"},
	{Version: "0.3.40", ID: "#292", Effect: "## TIMEOUT is enforced while a check runs: a check slower than its TIMEOUT turns red, and the failure names the timeout",
		Caution: "check each scenario's ## TIMEOUT against how long its checks really take"},
	// The kit ships in the image, so an executor's version is also the kit a re-onboard of it would use.
	{Version: "0.3.43", ID: "#308 (AC-D50)", Effect: "onboarding with the kit this release ships no longer destroys the instance it has just registered " +
		"(an older kit's host check sends no token, a control plane from rev 22 answers 401, and the exit trap tears the healthy instance down) — never re-onboard with an older kit"},
}

const updateBlockWhere = "the update block the control plane publishes for it (Environments page, or `update_command` from author__get_executor_status)"

// CheckExecutorVersion answers, per executor your runs can land on: is it one the control plane still
// gives work to, does it have the fixes whose absence first runs have tripped over, and would the
// control plane's own update block move it BACKWARDS. The last is the trap the control plane's verdict
// cannot show: on 2026-09-23 it ranked 0.3.36 `current` against a recommended floor of 0.3.32 while
// its update block installed 0.3.32 — a downgrade one paste away, and every fix since 0.3.36 missing.
func CheckExecutorVersion(in ExecutorInput) Check {
	c := Check{
		ID:      "executor-version",
		What:    "the executor your runs will use has the fixes first runs needed, is not refused work, and the control plane's update would not move it backwards",
		Subject: "GET /api/instances on " + orURL(in.ControlPlaneURL),
	}
	if in.NotAsked != "" {
		c.Status = StatusUnknown
		c.Detail = "not asked: " + in.NotAsked + ". Without the control plane's answer doctor cannot say which executor version your runs will use, or which fixes it lacks."
		if in.ControlPlaneURL == "" {
			c.Fix = "pass --control-plane <url> (or set ARGUS_CP_URL), or sign in once so the session records it: " + loginFix("")
		} else {
			c.Fix = "fix control-plane-credential first (its fix is above), then re-run argus doctor"
		}
		return c
	}
	if in.Err != "" {
		c.Status = StatusUnknown
		c.Detail = "could not read the executor list from " + orURL(in.ControlPlaneURL) + ": " + in.Err + " — this is NOT a statement about the executor."
		c.Fix = "re-run argus doctor; if it repeats, open " + orURL(in.ControlPlaneURL) + " — its Environments page shows the same list"
		if in.ErrFix != "" {
			c.Fix = in.ErrFix
		}
		return c
	}
	if in.Workspace != "" {
		c.Subject += " (workspace " + in.Workspace + ")"
	}
	if in.CredentialNote != "" {
		c.Subject += " [" + in.CredentialNote + "]"
	}

	execs := in.Executors
	if in.InstanceID != "" {
		execs = nil
		for _, e := range in.Executors {
			if e.InstanceID == in.InstanceID {
				execs = append(execs, e)
			}
		}
		if len(execs) == 0 {
			c.Status = StatusFail
			c.Detail = "no executor " + quote(in.InstanceID) + " in workspace " + orNone(in.Workspace) + "; it has: " + orNone(instanceIDs(in.Executors)) + "."
			c.Fix = "pass --instance with one of those (or correct ARGUS_INSTANCE_ID)"
			return c
		}
	}
	if len(execs) == 0 {
		c.Status = StatusFail
		c.Detail = "no executor is registered in workspace " + orNone(in.Workspace) + " on " + orURL(in.ControlPlaneURL) + ": a run has nowhere to go."
		c.Fix = "onboard one: onboarding/GETTING-STARTED.md (Path A step A1, or Path B step B4)"
		return c
	}

	c.Status = StatusOK
	var seen, details, fixes, blockedHere []string
	described := map[string]bool{} // a fix's effect is spelled out once per report, then named by id
	crossed := map[string]knownFix{}
	for _, e := range execs {
		seen = append(seen, e.InstanceID+" "+orNone(e.RunnerVersion))
		j := judgeExecutor(e, described)
		c.Status = worse(c.Status, j.status)
		details = append(details, j.detail)
		if j.fix != "" {
			fixes = append(fixes, j.fix)
			if BlockNeedsDocker(e.UpdateCommand) {
				blockedHere = append(blockedHere, e.InstanceID)
			}
		}
		for _, f := range j.crosses {
			crossed[f.ID] = f
		}
	}
	// Every fix above ends in "run the update block". Where that block cannot run, say so ONCE — on
	// evidence only: a probe that found no daemon, and a block that actually calls Docker.
	if d := in.HostDocker; d.Probed && !d.Answers && len(blockedHere) > 0 {
		fixes = append(fixes, "The control plane's update block (for "+strings.Join(blockedHere, ", ")+") cannot run on this host: "+
			"it runs `docker pull` and `docker run`, and no Docker daemon answers here ("+orNone(d.Reason)+"). "+
			"It is written for the kit and kubeconfig paths recorded when the instance was onboarded, so it will not run elsewhere unedited either, "+
			"and the control plane publishes no Docker-free form: ask your control-plane operator how to update it, "+
			"and do not re-onboard instead with a kit older than 0.3.43 (#308).")
	}
	// A behaviour change on the way is said ONCE, however many executors would cross it.
	for _, f := range knownFixes {
		if _, ok := crossed[f.ID]; ok {
			details = append(details, "An update to "+f.Version+" or later crosses a behaviour change — "+f.ID+": "+f.Effect+".")
			fixes = append(fixes, "Before you update to "+f.Version+" or later ("+f.ID+"): "+f.Caution+".")
		}
	}
	c.Subject += ": " + strings.Join(seen, ", ")
	c.Detail = strings.Join(details, " ")
	c.Fix = strings.Join(fixes, " ")
	return c
}

type judgement struct {
	status      Status
	detail, fix string
	crosses     []knownFix // behaviour changes an update of this executor would cross
}

// judgeExecutor is one executor's verdict, detail and fix, each prefixed with its id so a
// multi-executor report stays readable.
func judgeExecutor(e ExecutorStatus, described map[string]bool) judgement {
	id := e.InstanceID
	v, ok := federation.ParseSemverCore(e.RunnerVersion)
	if !ok || strings.HasPrefix(strings.TrimSpace(e.RunnerVersion), "0.0.0-") {
		// Mirrors federation.FloorState: a source or dev build has no release to compare.
		reason := e.VersionUnknownReason
		if reason == "" {
			reason = "it is not a release version"
		}
		// A WARN, not Unknown: Summarize treats Unknown as blocking, yet the control plane still gives
		// this executor work. Only FloorUpdateRequired refuses work.
		return judgement{status: StatusWarn,
			detail: id + " runs " + quote(e.RunnerVersion) + ", which the control plane cannot rank: " + reason +
				". It still gets work, but doctor cannot say which fixes it has.",
			fix: id + ": if this is a source or dev build on purpose, nothing to do; otherwise run a released executor (" + updateBlockWhere + ")."}
	}

	var lacks, crosses []knownFix
	for _, f := range knownFixes {
		fv, _ := federation.ParseSemverCore(f.Version)
		if compareCore(v, fv) >= 0 {
			continue
		}
		if f.Caution != "" {
			crosses = append(crosses, f)
		} else {
			lacks = append(lacks, f)
		}
	}
	rec, recOK := federation.ParseSemverCore(e.MinRecommended)
	// The trap the control plane's verdict cannot show: a recommendation BELOW what runs.
	downgrade := recOK && compareCore(v, rec) > 0 && (e.RecommendedImage != "" || e.UpdateCommand != "")

	j := judgement{status: StatusOK}
	var detail, fix []string
	switch e.VersionState {
	case federation.FloorUpdateRequired:
		j.status = StatusFail
		detail = append(detail, id+" runs "+e.RunnerVersion+", below the control plane's absolute floor "+e.MinSupported+
			": the control plane refuses it work, so every run on it fails.")
	case federation.FloorUpdateRecommended:
		j.status = StatusWarn
		detail = append(detail, id+" runs "+e.RunnerVersion+", below the control plane's recommended floor "+e.MinRecommended+
			": it works, and the control plane asks for an update.")
	}
	if downgrade {
		j.status = worse(j.status, StatusWarn)
		detail = append(detail, id+" runs "+e.RunnerVersion+", NEWER than the control plane's recommended floor "+e.MinRecommended+
			": its update block installs the image it recommends ("+orNone(e.RecommendedImage)+"), which may be that older release — "+
			"running the block as published can DOWNGRADE this executor.")
		fix = append(fix, id+": do not run the control plane's update block as published — check the version of "+orNone(e.RecommendedImage)+
			" first, and to move forward replace the image digest in the block (`--image-digest`) with a newer release's.")
	}
	if len(lacks) > 0 {
		j.status = worse(j.status, StatusWarn)
		var names []string
		for _, f := range lacks {
			if described[f.ID] {
				names = append(names, f.ID)
				continue
			}
			described[f.ID] = true
			names = append(names, f.ID+": "+f.Effect)
		}
		detail = append(detail, id+" ("+e.RunnerVersion+") lacks fixes first runs have tripped over — "+strings.Join(names, "; ")+".")
	}

	// ONE update instruction per executor, whichever reasons asked for it.
	if j.status != StatusOK && (len(lacks) > 0 || e.VersionState == federation.FloorUpdateRequired || e.VersionState == federation.FloorUpdateRecommended) {
		least := ""
		if len(lacks) > 0 {
			least = lacks[len(lacks)-1].Version
		}
		lsv, _ := federation.ParseSemverCore(least)
		switch {
		case downgrade || (least != "" && (!recOK || compareCore(rec, lsv) < 0)):
			// The control plane's recommendation does not have the fixes (or points backwards): its
			// block would not get there, so name the release and say to pin it.
			fix = append(fix, id+": move it to "+newestKnown()+" (the newest this doctor knows; "+arrivals(lacks)+") "+
				"by replacing the image digest in the update block — the control plane's recommendation ("+orNone(e.MinRecommended)+") does not have them.")
		case least != "":
			fix = append(fix, id+": move it to "+e.MinRecommended+" ("+arrivals(lacks)+") with "+updateBlockWhere+".")
		default:
			fix = append(fix, id+": move it to "+e.MinRecommended+" with "+updateBlockWhere+".")
		}
		j.crosses = crosses
	}
	if len(detail) == 0 {
		detail = append(detail, id+" runs "+e.RunnerVersion+": current, with every fix this doctor knows of.")
	}
	j.detail, j.fix = strings.Join(detail, " "), strings.Join(fix, " ")
	return j
}

func newestKnown() string { return knownFixes[len(knownFixes)-1].Version }

// arrivals says which release brings which of the missing fixes ("the fixes above arrive in 0.3.37:
// #135, #138; 0.3.43: #308"), so a reader choosing a release can see what each step up buys. lacks is
// in release order, as knownFixes is.
func arrivals(lacks []knownFix) string {
	var parts []string
	for i, f := range lacks {
		short := strings.Fields(f.ID)[0]
		if i > 0 && lacks[i-1].Version == f.Version {
			parts[len(parts)-1] += ", " + short
			continue
		}
		parts = append(parts, f.Version+": "+short)
	}
	return "the fixes above arrive in " + strings.Join(parts, "; ")
}

// BlockNeedsDocker is whether the control plane's update block calls Docker on the host. Read from the
// block itself, not assumed, so a future Docker-free block needs no change here.
func BlockNeedsDocker(block string) bool {
	return strings.Contains(block, "docker pull") || strings.Contains(block, "docker run")
}

func instanceIDs(es []ExecutorStatus) string {
	var ids []string
	for _, e := range es {
		ids = append(ids, e.InstanceID)
	}
	return strings.Join(ids, ", ")
}

// worse ranks statuses the way Summarize reads them: fail over unknown (both block), then warn, then ok.
func worse(a, b Status) Status {
	rank := map[Status]int{StatusOK: 0, StatusWarn: 1, StatusUnknown: 2, StatusFail: 3}
	if rank[b] > rank[a] {
		return b
	}
	return a
}

func compareCore(a, b [3]int) int {
	for i := 0; i < 3; i++ {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}
