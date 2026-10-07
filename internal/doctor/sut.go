package doctor

import (
	"strconv"
	"strings"
	"time"
)

// The control plane's sut_state vocabulary (internal/control/sutstate.go). Repeated rather than
// imported: this package does no I/O and must not pull in the control plane.
const (
	sutReady       = "ready"
	sutUnreachable = "unreachable"
	sutNotChecked  = "not_checked"
)

// SUTStaleAfter mirrors control.SUTStaleAfter. The control plane applies the window (a reading older
// than it is published as not_checked); doctor applies it again to tell that case from a fresh
// not_checked, which means something else. A contract test in cmd/argus pins the two equal.
const SUTStaleAfter = 3 * time.Minute

// CheckSUTReachable reports whether each executor could reach its system under test, as the executor
// itself measured it (internal/runner/sutprobe.go: every target in its argus-config, dialled over TCP
// every 60 s) and the control plane published it on GET /api/instances.
//
// WHY THE EXECUTOR'S READING AND NOT A DIAL FROM HERE. Reachability depends on where you stand. On the
// compose tier base_url is the SUT's in-network name (http://documenso:3100), which the executor
// resolves and the operator's host does not; dialling from here would fail a correct config and pass
// a `localhost` one that the executor cannot use. It also keeps doctor's rule: it contacts nothing but
// the control plane, and the rows it reads are the ones executor-version already fetched.
//
// What it cannot say: the control plane keeps one boolean per executor, not which target failed, and
// an open port is not a healthy app. Detail says both.
func CheckSUTReachable(in ExecutorInput) Check {
	c := Check{
		ID:      "sut-reachable",
		What:    "the executor your runs will use can reach the system under test from its own network",
		Subject: "sut_state on GET /api/instances from " + orURL(in.ControlPlaneURL),
	}
	// The control plane was not asked, or did not answer: executor-version already fails (unknown) for
	// exactly that, so this one SKIPS rather than failing the same cause twice. The verdict still fails.
	if in.NotAsked != "" {
		c.Status = StatusSkip
		c.Detail = "not asked: " + in.NotAsked + ". Without the control plane's answer doctor cannot say whether the executor reaches your app (see executor-version)."
		return c
	}
	if in.Err != "" {
		c.Status = StatusSkip
		c.Detail = "could not read the executor list: " + in.Err + " — this is NOT a statement about the SUT (see executor-version)."
		return c
	}
	if in.Workspace != "" {
		c.Subject += " (workspace " + in.Workspace + ")"
	}
	execs := in.Executors
	if in.InstanceID != "" {
		execs = nil
		for _, e := range in.Executors {
			if e.InstanceID == in.InstanceID {
				execs = append(execs, e)
			}
		}
	}
	if len(execs) == 0 {
		// executor-version already fails for this; a second failure would only repeat it.
		c.Status = StatusSkip
		c.Detail = "no executor to read a SUT reading from (see executor-version)."
		return c
	}

	now := time.Now
	if in.Now != nil {
		now = in.Now
	}
	noTargets := in.ConfigTargets != nil && *in.ConfigTargets == 0

	c.Status = StatusOK
	skipped := 0
	var seen, details, fixes []string
	for _, e := range execs {
		seen = append(seen, e.InstanceID+" "+orNone(e.SUTState))
		s, d, f := judgeSUT(e, now(), noTargets)
		if s == StatusSkip {
			skipped++
		} else {
			c.Status = worse(c.Status, s)
		}
		details = append(details, d)
		if f != "" {
			fixes = append(fixes, f)
		}
	}
	if skipped == len(execs) {
		c.Status = StatusSkip // every reading is explained by a config with no targets: nothing to judge
	}
	c.Subject += ": " + strings.Join(seen, ", ")
	c.Detail = strings.Join(details, " ")
	c.Fix = strings.Join(fixes, " ")
	return c
}

// windowWords renders a staleness window for a message: "3 minutes", not "3m0s".
func windowWords(d time.Duration) string {
	if d%time.Minute == 0 {
		n := int(d / time.Minute)
		if n == 1 {
			return "1 minute"
		}
		return strconv.Itoa(n) + " minutes"
	}
	return d.String()
}

// judgeSUT is one executor's status, detail and fix, each prefixed with its id so a multi-executor
// report stays readable.
//
// not_checked WITH a time has two causes, told apart by the reading's age against SUTStaleAfter:
//   - fresh: the probe ran and returned no verdict (the executor sends reachable=null, checked_at=now,
//     and the control plane keeps it, VR7-J1; its sutState answers not_checked because reachable is nil);
//   - stale: the reading is older than the window, so it says nothing about now.
//
// noTargets is the caller's word that --config declares no targets at all.
func judgeSUT(e ExecutorStatus, now time.Time, noTargets bool) (Status, string, string) {
	id := e.InstanceID
	at := ""
	if e.SUTCheckedAt != nil {
		at = e.SUTCheckedAt.UTC().Format(time.RFC3339)
	}
	switch e.SUTState {
	case sutReady:
		return StatusOK, id + " reached every target in its argus-config at " + at +
			" (a TCP connection: an open port, not proof the app behind it is healthy).", ""
	case sutUnreachable:
		return StatusFail, id + " could NOT reach at least one target in its argus-config at " + at +
				". The dial is made from the executor's network, not from this host, and the control plane records only the overall answer, not which target.",
			id + ": in argus-config, name each target the way the executor sees it — on the compose tier the SUT's service name on the network in deploy.network " +
				"(http://<service>:<port>, jdbc:postgresql://<db-service>:5432/<db>), never localhost, which is the executor itself — and check the SUT is up. " +
				"To see which target fails, run argus validate-config --config <argus-config.yaml> where the executor runs (or the runner__validate_config tool)."
	case sutNotChecked:
		if at == "" {
			// Not judged by version: the probe predates this repository (its first commit, c2477ab, already
			// has it), and 0.3.36 executors were read live on argus-dev on 2026-10-01 reporting "ready".
			// ARGUS_VALIDATE_NO_PROBE is not a cause here: it makes the probe run and report no verdict (fresh).
			return StatusUnknown, id + " has never reported a SUT reading. Either it was started without an argus-config, " +
					"or it has not polled since it started.",
				id + ": start it with the argus-config your runs use; meanwhile argus validate-config --config <argus-config.yaml> where the executor runs reports reachability."
		}
		if now.Sub(*e.SUTCheckedAt) > SUTStaleAfter {
			return StatusUnknown, id + "'s last SUT reading, at " + at + ", is older than " + windowWords(SUTStaleAfter) +
					", so it is not evidence about now: the executor has stopped polling or reporting.",
				id + ": check the executor is running (the Environments page shows when it last polled), then re-run argus doctor."
		}
		if noTargets {
			return StatusSkip, id + "'s probe ran at " + at + " and found nothing to dial: the argus-config you gave declares no targets, " +
				"so the executor has nothing to probe. Reachability shows in the runs themselves.", ""
		}
		return StatusUnknown, id + ": the probe ran at " + at + " and could not conclude: it returned no verdict, which is not a failure and not a pass. " +
				"Causes: ARGUS_VALIDATE_NO_PROBE is set in the executor's environment, or a target has no dialable address " +
				"(an external list form, an empty jdbc_url, a broker with neither url nor management_url, a scheme with no default port), " +
				"or its argus-config declares no targets at all.",
			id + ": run argus validate-config --config <argus-config.yaml> where the executor runs — it names the target the probe could not dial."
	case "":
		return StatusUnknown, "the control plane does not publish sut_state for " + id + ", so doctor cannot say whether it reaches the SUT.",
			id + ": ask your control-plane operator to update the control plane; meanwhile argus validate-config --config <argus-config.yaml> where the executor runs reports reachability."
	default:
		return StatusUnknown, id + " has a sut_state this doctor does not know: " + quote(e.SUTState) + ".",
			"update argus, then re-run argus doctor."
	}
}
