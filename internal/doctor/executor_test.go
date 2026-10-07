package doctor

import (
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/federation"
)

// The state every 0.3.36-era incident was recorded against: the control plane's floors as read from the
// argus-dev ConfigMap on 2026-09-23 (MIN_RUNNER_VERSION=0.3.0, MIN_EXECUTOR_VERSION_RECOMMENDED=0.3.32),
// the image it recommended (sha256:d7aff596…, which is 0.3.32), and its verdict on 0.3.36: `current`.
// Source: tester the tester notes.
func exec0336(id string) ExecutorStatus {
	return ExecutorStatus{
		InstanceID:       id,
		RunnerVersion:    "0.3.36",
		VersionState:     federation.FloorCurrent,
		MinSupported:     "0.3.0",
		MinRecommended:   "0.3.32",
		RecommendedImage: "argus-executor@sha256:d7aff596",
		UpdateCommand:    "bash onboarding/update.sh --image argus-executor@sha256:d7aff596",
	}
}

func asked(execs ...ExecutorStatus) ExecutorInput {
	return ExecutorInput{ControlPlaneURL: "https://argus-dev.onedroid.ai", Workspace: "ws-1", Executors: execs}
}

// dockerBlock is the state of 2026-09-29: the control plane recommends 0.3.44 and publishes the update
// block it renders for every host (DEC-U1) — `docker pull` the image, then `docker run` it twice.
func dockerBlock(e ExecutorStatus) ExecutorStatus {
	e.VersionState, e.MinRecommended = federation.FloorUpdateRecommended, "0.3.44"
	e.RecommendedImage = "ghcr.io/onedro1d/argus-runner@sha256:06dc0675"
	e.UpdateCommand = "(\nset -euo pipefail\ndocker pull \"$IMG\"\ndr() { docker run --rm \"$@\"; }\ndr \"$IMG\" update discover\n)"
	return e
}

// noDocker is a host where no Docker daemon answers, in Docker's own words (the Coder workspace, 09-29).
func noDocker(in ExecutorInput) ExecutorInput {
	in.HostDocker = DockerProbe{Probed: true, Reason: "failed to connect to the docker API at unix:///var/run/docker.sock; dial unix /var/run/docker.sock: connect: no such file or directory"}
	return in
}

// REPLAY: one case per recorded incident this check claims (verify/2026-09-25-friction-investigation,
// incidents-{A,B}-*.tsv; tagging in verify/2026-09-28-doctor-incident-tagging). The recorded state goes
// in; the report must name the CAUSE (Detail) and the FIX (Fix). A:83 (a documented pin lagging the dev
// builds) is tagged to this check too, but its versions were not recorded, so it has no replay here.
func TestCheckExecutorVersion_Replay(t *testing.T) {
	cases := []struct {
		row, source string
		in          ExecutorInput
		detail      []string // every one must appear in Detail
		fix         []string // every one must appear in Fix (case-insensitive)
	}{
		{"A:89", `handoffs/2026-09-22-en7-takeover-onboard-running.md:33 "0.3.36 runner lacks #135: always cleanup on mcp steps ignored"`,
			asked(exec0336("sut-demo")), []string{"#135", `"always": true`, "mcp"}, []string{"0.3.37"}},
		{"A:94", `handoffs/2026-09-22-sut-demo-live-prs-154-156.md:29 "0.3.36 lacks #135/#138/#140: passing UI rows still list bullets as unexecuted"`,
			asked(exec0336("sut-demo")), []string{"#138", "#140", "unexecuted"}, []string{"0.3.37"}},
		{"B:15", `the tester notes "3 of 3 not evaluated (0.3.36 has no #139 cause line)"`,
			asked(exec0336("sut-demo")), []string{"#140", "#139", "cause"}, []string{"0.3.37"}},
		{"B:68", `handoffs/2026-09-25-hub-runner-window-mixup.md:47 "executor lost … fixed by #209, ships from v0.3.37-rc.1"`,
			asked(exec0336("hub-tester")), []string{"#209", "executor lost"}, []string{"0.3.37"}},
		{"B:77", `verify/2026-09-25-en7-sut-demo/PROOF-STATUS.md:17 "WEBUI-001 on the 0.3.36 executor lists all 4 enforced bullets under unexecuted"`,
			asked(exec0336("sut-demo")), []string{"AC-D27", "unexecuted"}, []string{"0.3.37"}},
		{"B:38", `the tester notes "The CP RECOMMENDS sha256:d7aff596… = 0.3.32, OLDER than ours"`,
			asked(exec0336("sut-demo")), []string{"0.3.32", "NEWER", "DOWNGRADE", "d7aff596"}, []string{"do not run", "update block"}},
		{"B:56", `verify/2026-09-23-hub-tester/ONBOARD-HUB-PIOTR.md:103 (recurrence of B:38)`,
			asked(exec0336("hub-tester")), []string{"0.3.32", "DOWNGRADE"}, []string{"do not run"}},
		{"B:60", `handoffs/2026-09-24-hub-harness-up-9of12-green.md:42 "Never run the CP's update_command" (recurrence of B:38)`,
			asked(exec0336("hub-tester")), []string{"0.3.32", "DOWNGRADE"}, []string{"do not run"}},
		{"B:80", `NOTES.md:43 (09-25) "CP update_command recommends d7aff596… = 0.3.32 — a DOWNGRADE" (recurrence of B:38)`,
			asked(exec0336("hub-tester")), []string{"0.3.32", "DOWNGRADE"}, []string{"do not run"}},
		{"A:95", `tester-upstream-notepad handoffs/2026-09-22-sut-demo-live-prs-154-156.md:30 "'\"' or '\\' in a ${VAR} value silently empties app_url (ui_scenario.go:54); app_url expanded twice."`,
			asked(exec0336("sut-demo")), []string{"#160", "quote or backslash", "${VAR}", "APP_URL"}, []string{"0.3.37"}},
		{"B:28", `handoffs/2026-09-23-sut-demo-59-59-prs-awaiting-merge.md:65 "quote/backslash in ${VAR} empties the UI payload, internal/argus/ui_scenario.go:53-56"`,
			asked(exec0336("sut-demo")), []string{"#160", "quote or backslash", "payload"}, []string{"0.3.37"}},
		// Rows recorded after the 09-25 investigation, each with its own source.
		{"T0928:312", `verify/2026-09-28-before-trial/trial-timeline.txt:87 "no runner id paired for this instance … before author_get_report can relay to it" — the trial agent then called 19 reds "Memstore gaps"`,
			asked(exec0336("sut-demo")), []string{"#286", "author_get_report", "why"}, []string{"0.3.40"}},
		{"GH:307", `issue #307 (AC-D50): "7 registrations, 7 destructions, 0 survivors across compose, k3d and managed AKS"; the 0.3.36 kit has the same probe (argus-kit-0.3.36/onboarding/onboard.sh:3784-3789)`,
			asked(exec0336("sut-demo")), []string{"#308", "AC-D50", "destroy"}, []string{"0.3.43"}},
		{"L0929:docker", `tester verify/2026-09-29-312-fix-check/NOTE.md § Addendum 2: the fix said "with the update block the control plane publishes", the block runs docker pull, and docker info here says "dial unix /var/run/docker.sock: connect: no such file or directory"`,
			noDocker(asked(dockerBlock(exec0336("sut-demo")))), []string{"#135"}, []string{"cannot run on this host", "docker.sock", "ask your control-plane operator", "kit and kubeconfig paths"}},
	}
	for _, tc := range cases {
		t.Run(tc.row, func(t *testing.T) {
			c := CheckExecutorVersion(tc.in)
			if c.ID != "executor-version" {
				t.Fatalf("id = %q", c.ID)
			}
			if c.Status != StatusWarn {
				t.Errorf("%s: status = %q, want warn (the run works, on an executor that lacks what the incident needed)\nsource: %s", tc.row, c.Status, tc.source)
			}
			for _, s := range tc.detail {
				if !strings.Contains(c.Detail, s) {
					t.Errorf("%s: detail does not name %q — the cause the incident had to discover\nsource: %s\ndetail: %s", tc.row, s, tc.source, c.Detail)
				}
			}
			for _, s := range tc.fix {
				if !strings.Contains(strings.ToLower(c.Fix), strings.ToLower(s)) {
					t.Errorf("%s: fix does not say %q\nsource: %s\nfix: %s", tc.row, s, tc.source, c.Fix)
				}
			}
			if !strings.Contains(c.Subject, tc.in.Executors[0].InstanceID) || !strings.Contains(c.Subject, "0.3.36") {
				t.Errorf("%s: subject does not say which executor and version were examined: %s", tc.row, c.Subject)
			}
		})
	}
}

func TestCheckExecutorVersion(t *testing.T) {
	current := ExecutorStatus{InstanceID: "sut-demo", RunnerVersion: "0.3.44", VersionState: federation.FloorCurrent,
		MinSupported: "0.3.0", MinRecommended: "0.3.44", RecommendedImage: "argus-executor@sha256:06dc0675"}

	// THE RULE: a check that could not ask is unknown, never ok — and says why it did not ask.
	t.Run("not asked is unknown, with the reason and a fix", func(t *testing.T) {
		c := CheckExecutorVersion(ExecutorInput{NotAsked: "no control-plane URL"})
		if c.Status != StatusUnknown || !strings.Contains(c.Detail, "no control-plane URL") || c.Fix == "" {
			t.Fatalf("got %+v", c)
		}
	})
	t.Run("an error asking is unknown, and is not a statement about the executor", func(t *testing.T) {
		c := CheckExecutorVersion(ExecutorInput{ControlPlaneURL: "https://cp", Err: "HTTP 503"})
		if c.Status != StatusUnknown || !strings.Contains(c.Detail, "HTTP 503") || !strings.Contains(c.Detail, "NOT a statement about the executor") {
			t.Fatalf("got %+v", c)
		}
	})
	t.Run("no executor in the workspace fails: a run has nowhere to go", func(t *testing.T) {
		c := CheckExecutorVersion(asked())
		if c.Status != StatusFail || !strings.Contains(c.Detail, "ws-1") || !strings.Contains(c.Fix, "GETTING-STARTED") {
			t.Fatalf("got %+v", c)
		}
	})
	t.Run("current, with every known fix, is ok", func(t *testing.T) {
		c := CheckExecutorVersion(asked(current))
		if c.Status != StatusOK || c.Fix != "" {
			t.Fatalf("got %+v", c)
		}
	})
	t.Run("below the absolute floor fails: the control plane refuses it work", func(t *testing.T) {
		e := current
		e.RunnerVersion, e.VersionState, e.MinSupported = "0.3.20", federation.FloorUpdateRequired, "0.3.30"
		c := CheckExecutorVersion(asked(e))
		if c.Status != StatusFail || !strings.Contains(c.Detail, "0.3.30") || !strings.Contains(c.Detail, "refuse") {
			t.Fatalf("got %+v", c)
		}
	})
	// The update path crosses 0.3.40, which ENFORCES ## TIMEOUT (#292): the fix must say so before
	// anyone updates, or the update turns slow checks red and looks like a regression.
	t.Run("below the recommended floor warns, and names the TIMEOUT change on the way", func(t *testing.T) {
		e := current
		e.RunnerVersion, e.VersionState = "0.3.38", federation.FloorUpdateRecommended
		c := CheckExecutorVersion(asked(e))
		if c.Status != StatusWarn || !strings.Contains(c.Detail, "0.3.40") || !strings.Contains(c.Detail, "#292") ||
			!strings.Contains(c.Fix, "## TIMEOUT") {
			t.Fatalf("got %+v", c)
		}
	})
	// (, reported by Talos from a Windows run): the control plane still
	// gives work to an executor it cannot rank, so doctor must not call that blocking.
	t.Run("a version nobody can rank is a warning, with the control plane's reason and what to do", func(t *testing.T) {
		for _, ver := range []string{"0.0.0-src+abc", "0.0.0-dev", "not-a-version", ""} {
			e := current
			e.RunnerVersion, e.VersionState, e.VersionUnknownReason = ver, federation.FloorUnknown, "a source build carries no release version"
			c := CheckExecutorVersion(asked(e))
			if c.Status != StatusWarn || !strings.Contains(c.Detail, "cannot rank") ||
				!strings.Contains(c.Detail, "a source build carries no release version") || c.Fix == "" {
				t.Fatalf("version %q: got %+v", ver, c)
			}
			if rep := Summarize([]Check{c}); rep.Verdict != VerdictWarn || len(rep.Failing) != 0 {
				t.Fatalf("version %q: summary blocks: %+v", ver, rep)
			}
		}
	})
	t.Run("an unrankable executor beside an update-required one still fails", func(t *testing.T) {
		bad := current
		bad.InstanceID, bad.RunnerVersion, bad.VersionState, bad.MinSupported = "old", "0.3.20", federation.FloorUpdateRequired, "0.3.30"
		dev := current
		dev.InstanceID, dev.RunnerVersion, dev.VersionState = "dev", "0.0.0-dev", federation.FloorUnknown
		if c := CheckExecutorVersion(asked(dev, bad)); c.Status != StatusFail {
			t.Fatalf("got %+v", c)
		}
	})
	t.Run("the worst executor sets the status, and every one is named", func(t *testing.T) {
		c := CheckExecutorVersion(asked(current, exec0336("hub-tester")))
		if c.Status != StatusWarn || !strings.Contains(c.Subject, "sut-demo") || !strings.Contains(c.Subject, "hub-tester") {
			t.Fatalf("got %+v", c)
		}
	})
	// Found on the first live run (2026-09-28, two executors on 0.3.36): the report repeated every fix's
	// description and the TIMEOUT caution per executor, and gave each executor two update instructions.
	t.Run("two executors on one old version: each description, the caution and each update instruction appear once", func(t *testing.T) {
		a, b := exec0336("sut-demo"), exec0336("hub-tester")
		a.VersionState, a.MinRecommended, b.VersionState, b.MinRecommended = federation.FloorUpdateRecommended, "0.3.40", federation.FloorUpdateRecommended, "0.3.40"
		c := CheckExecutorVersion(asked(a, b))
		for _, s := range []string{"on mcp steps too", "## TIMEOUT is enforced"} {
			if n := strings.Count(c.Detail, s); n != 1 {
				t.Errorf("detail says %q %d times, want once:\n%s", s, n, c.Detail)
			}
		}
		if n := strings.Count(c.Fix, "check each scenario's ## TIMEOUT"); n != 1 {
			t.Errorf("fix gives the TIMEOUT caution %d times, want once:\n%s", n, c.Fix)
		}
		for _, id := range []string{"sut-demo: move it", "hub-tester: move it"} {
			if n := strings.Count(c.Fix, id); n != 1 {
				t.Errorf("fix has %d update instructions for %q, want one:\n%s", n, id, c.Fix)
			}
		}
		if !strings.Contains(c.Detail, "hub-tester (0.3.36) lacks fixes first runs have tripped over — #135; ") {
			t.Errorf("the second executor's fixes are not named by id only:\n%s", c.Detail)
		}
	})
	// The Docker sentence is said only on evidence: a daemon that answers, a probe that never ran, or a
	// block that does not use Docker all leave the fix exactly as it was.
	t.Run("no Docker sentence unless the block needs Docker AND a probe found none", func(t *testing.T) {
		answers := asked(dockerBlock(exec0336("sut-demo")))
		answers.HostDocker = DockerProbe{Probed: true, Answers: true}
		noDockerInBlock := dockerBlock(exec0336("sut-demo"))
		noDockerInBlock.UpdateCommand = "bash onboarding/update.sh --image argus-executor@sha256:06dc0675"
		noBlock := dockerBlock(exec0336("sut-demo"))
		noBlock.UpdateCommand = ""
		for name, in := range map[string]ExecutorInput{
			"a daemon answers":          answers,
			"not probed":                asked(dockerBlock(exec0336("sut-demo"))),
			"the block does not use it": noDocker(asked(noDockerInBlock)),
			"no block was published":    noDocker(asked(noBlock)),
		} {
			c := CheckExecutorVersion(in)
			if strings.Contains(c.Fix, "cannot run on this host") || strings.Contains(c.Fix, "Docker daemon") {
				t.Errorf("%s: fix speaks about Docker without evidence:\n%s", name, c.Fix)
			}
			if !strings.Contains(c.Fix, "update block") {
				t.Errorf("%s: fix lost its update instruction:\n%s", name, c.Fix)
			}
		}
	})
	t.Run("two executors on a Docker-less host: the Docker sentence is said once", func(t *testing.T) {
		c := CheckExecutorVersion(noDocker(asked(dockerBlock(exec0336("sut-demo")), dockerBlock(exec0336("hub-tester")))))
		if n := strings.Count(c.Fix, "cannot run on this host"); n != 1 {
			t.Errorf("fix says the block cannot run %d times, want once:\n%s", n, c.Fix)
		}
	})
	t.Run("a fix a release carries is named with that release", func(t *testing.T) {
		c := CheckExecutorVersion(asked(dockerBlock(exec0336("sut-demo"))))
		for _, s := range []string{"0.3.37: #135", "0.3.40: #286", "0.3.43: #308"} {
			if !strings.Contains(c.Fix, s) {
				t.Errorf("fix does not say %q:\n%s", s, c.Fix)
			}
		}
	})
	t.Run("--instance narrows the report to that executor", func(t *testing.T) {
		in := asked(current, exec0336("hub-tester"))
		in.InstanceID = "sut-demo"
		c := CheckExecutorVersion(in)
		if c.Status != StatusOK || strings.Contains(c.Subject, "hub-tester") {
			t.Fatalf("got %+v", c)
		}
	})
	t.Run("an --instance the workspace does not have fails, and lists what it has", func(t *testing.T) {
		in := asked(current)
		in.InstanceID = "typo-tester"
		c := CheckExecutorVersion(in)
		if c.Status != StatusFail || !strings.Contains(c.Detail, "typo-tester") || !strings.Contains(c.Detail, "sut-demo") {
			t.Fatalf("got %+v", c)
		}
	})
}

// The fix table is hand-kept, so hold it to a shape a reader can check: every entry names a parseable
// release and an id, and the table is in release order (the report lists fixes in the order they shipped).
func TestKnownFixesTable(t *testing.T) {
	if len(knownFixes) == 0 {
		t.Fatal("knownFixes is empty")
	}
	var prev [3]int
	for i, f := range knownFixes {
		v, ok := federation.ParseSemverCore(f.Version)
		if !ok || f.ID == "" || f.Effect == "" {
			t.Fatalf("entry %d is incomplete: %+v", i, f)
		}
		if i > 0 && compareCore(v, prev) < 0 {
			t.Fatalf("entry %d (%s) is out of release order", i, f.ID)
		}
		prev = v
	}
}
