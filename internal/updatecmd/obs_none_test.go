package updatecmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// — `argus update` AND ITS ROLLBACK LEAVE AN `--obs none` COMPOSE INSTANCE WITHOUT OBSERVABILITY.
//
// Onboarding with `--obs none` on compose starts no loki, promtail or pushgateway and wires no Grafana
// datasource or dashboard. Step A-4 knew nothing of that: with the shared Grafana reachable
// it wrote the Prometheus target file, registered a Loki datasource for a Loki that is not running, imported a
// dashboard, and ran `dc up -d --force-recreate --no-deps promtail` — which STARTS the promtail onboarding
// deliberately did not. The instance is recognised by ARGUS_OBS_LOKI being SET AND EMPTY in its env file (what
// that onboarding writes), or ARGUS_OBS_MODE=none.

// obsNoneMachine is fakeMachine as an `--obs none` compose instance really is: the env file carries the two
// empties (and, optionally, the mode), and the project holds the executor only.
func obsNoneMachine(t *testing.T, root string, mutate func(in *PlanInput)) PlanInput {
	t.Helper()
	in := fakeMachine(t, root, "compose")
	env := filepath.Join(in.KitDir, "deploy", "compose", "env.i1")
	if err := os.WriteFile(env, []byte(readFile(t, env)+"\nARGUS_OBS_LOKI=\nARGUS_OBS_PUSHGATEWAY=\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	in.Observed.ObsNone = true
	in.Observed.Compose.Services = in.Observed.Compose.Services[:1] // the executor only
	if mutate != nil {
		mutate(&in)
	}
	return in
}

// callsOf is every recorded host call, one per line (docker, kubectl, curl — the stubs append to calls.log).
func callsOf(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	for _, l := range strings.Split(readFile(t, filepath.Join(root, "calls.log")), "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

func a4Of(t *testing.T, p Plan) Step {
	t.Helper()
	for _, s := range p.Steps {
		if s.ID == "A-4" {
			return s
		}
	}
	t.Fatal("the plan has no A-4")
	return Step{}
}

// ── the plan ────────────────────────────────────────────────────────────────────────────────────────

func TestPlan_A4IsSkippedForAnObsNoneComposeInstanceAndSaysWhy(t *testing.T) {
	in := fixtureInput("compose")
	in.Observed.ObsNone = true
	p, err := BuildPlan(in)
	if err != nil {
		t.Fatal(err)
	}
	s := a4Of(t, p)
	if s.SkipReason == "" {
		t.Fatal("A-4 is planned to run for an --obs none instance: it would write a target, a datasource and a dashboard and start promtail")
	}
	if !strings.Contains(s.SkipReason, "no observability") || !strings.Contains(s.SkipReason, "--obs none") {
		t.Errorf("the skip reason must say the instance has no observability (--obs none): %q", s.SkipReason)
	}
	// a rollback is the same plan with --rollback-to
	in = fixtureInput("compose")
	in.Observed.ObsNone = true
	in.RollbackTo, in.Version = "0.3.31", "0.3.31"
	in.Installed = Manifest{InstanceID: "i1", Tier: "compose", Version: "0.3.32", Previous: &Previous{Version: "0.3.31", Image: in.ImageDigest}}
	p, err = BuildPlan(in)
	if err != nil {
		t.Fatal(err)
	}
	if a4Of(t, p).SkipReason == "" || !strings.Contains(a4Of(t, p).SkipReason, "no observability") {
		t.Errorf("a rollback of an --obs none instance must skip A-4 for the same reason: %q", a4Of(t, p).SkipReason)
	}
}

func TestPlan_A4StillRunsForAnInstanceNotRecordedAsObsNone(t *testing.T) {
	in := fixtureInput("compose") // ObsNone false: the name is unset in its env file
	p, err := BuildPlan(in)
	if err != nil {
		t.Fatal(err)
	}
	if s := a4Of(t, p); s.SkipReason != "" {
		t.Errorf("A-4 is skipped for an instance with the obs name unset (Grafana reachable): %q", s.SkipReason)
	}
}

// ── discover: the env file decides ──────────────────────────────────────────────────────────────────

// ── the apply harness: the REAL generated preflight + apply.sh against stub docker/curl ──────────────────

// grafanaAndObsCalls are the recorded calls that touch what an --obs none instance does not have.
func grafanaAndObsCalls(calls []string) []string {
	var bad []string
	for _, c := range calls {
		switch {
		case strings.HasPrefix(c, "curl ") && (strings.Contains(c, ":3000") || strings.Contains(c, "/api/")):
			bad = append(bad, c) // any Grafana endpoint: the datasource, the dashboard, the health read, the capture
		case strings.HasPrefix(c, "docker ") &&
			(strings.Contains(c, " promtail") || strings.Contains(c, " loki") || strings.Contains(c, " pushgateway")) &&
			(strings.Contains(c, " up ") || strings.Contains(c, " restart ") || strings.Contains(c, " start ") || strings.Contains(c, " create ") || strings.Contains(c, " run ")):
			bad = append(bad, c) // a start / recreate of promtail, loki or pushgateway
		}
	}
	return bad
}

func runObsNone(t *testing.T, name string, build func(t *testing.T, root string) PlanInput) (out string, code int, root string, in PlanInput) {
	t.Helper()
	requireBash(t)
	root = t.TempDir()
	in = build(t, root)
	out, code = runApply(t, in, nil, nil)
	return out, code, root, in
}

// logCalls prints the recorded host calls (go test -v), the temp root and long mounts shortened.
func logCalls(t *testing.T, label, root string, calls []string) {
	t.Helper()
	for i, c := range calls {
		c = strings.ReplaceAll(c, root, "$ROOT")
		if len(c) > 150 {
			c = c[:150] + " ..."
		}
		t.Logf("%s call %02d: %s", label, i+1, c)
	}
}

func assertNoObservabilityTouched(t *testing.T, root string, in PlanInput, out string) {
	t.Helper()
	calls := callsOf(t, root)
	logCalls(t, "obs-none", root, calls)
	if bad := grafanaAndObsCalls(calls); len(bad) != 0 {
		t.Errorf("an --obs none instance's update touched observability:\n  %s\nall calls:\n  %s", strings.Join(bad, "\n  "), strings.Join(calls, "\n  "))
	}
	// the executor IS still recreated: the update did its job
	if !strings.Contains(strings.Join(calls, "\n"), "up -d --no-deps --force-recreate executor") {
		t.Errorf("the executor was not recreated:\n  %s", strings.Join(calls, "\n  "))
	}
	// no Prometheus target is written: the file the fixture carried is exactly as it was
	if got := readFile(t, filepath.Join(in.KitDir, "deploy", "compose", "targets.d", "i1.json")); got != `{"targets":["i1"]}` {
		t.Errorf("a Prometheus target was (re)written for an instance with no pushgateway: %s", got)
	}
	// the step is reported skipped, with the reason, in the output AND in progress.json
	if !strings.Contains(out, "skipped:") || !strings.Contains(out, "no observability") {
		t.Errorf("the output does not report A-4 skipped with its reason:\n%s", out)
	}
	if progress := readFile(t, filepath.Join(in.StageDir, "progress.json")); !strings.Contains(progress, `"id":"A-4","state":"skipped"`) {
		t.Errorf("progress.json does not record A-4 as skipped:\n%s", progress)
	}
}
