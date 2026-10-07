package updatecmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// a6_scenarios_test.go — V32 Release QA finding (a): A-6 SAID SOMETHING FALSE AND LEFT STALE FILES.
//
// Measured 2026-09-14 on orderservice-compose: A-6 was skipped with "Path A scenarios live in the
// control plane's catalog, not in files on this machine". A Path A kit DOES hold them in files —
// <kit>/test-agent/scenarios, seeded from order-service-demo/scenarios-baked — and after the update
// those files were still the old format the new release refuses (0 of 33 carried `### Runnable`).
//
// The row's own design (V32-01 PO: "A-6 Path A scenarios, same file rules") is a refresh with a
// backup, like A-5's skills. ⛔ And ONLY for a Path A demo kit: anywhere else the scenario folder is
// the operator's own work and an update must never overwrite it.

const pathATestDirOnDisk = `C:\argus-kit\test-agent` // the native spelling observed.json records

func a6(t *testing.T, p Plan) Step {
	t.Helper()
	for _, s := range p.Steps {
		if s.ID == "A-6" {
			return s
		}
	}
	t.Fatal("the plan has no A-6")
	return Step{}
}

func TestPlan_A6RefreshesAPathAKitsScenarios(t *testing.T) {
	for _, tier := range []string{"compose", "k3d"} {
		in := fixtureInput(tier) // KitDir /c/argus-kit
		in.Observed.Env["ARGUS_TEST_DIR_HOST"] = pathATestDirOnDisk
		p, err := BuildPlan(in)
		if err != nil {
			t.Fatal(err)
		}
		if s := a6(t, p); s.SkipReason != "" {
			t.Errorf("%s: the test-agent folder IS <kit>/test-agent (a Path A kit), yet A-6 is skipped: %q", tier, s.SkipReason)
		}
	}
}

func TestPlan_A6NeverTouchesAnOperatorsOwnScenarios(t *testing.T) {
	in := fixtureInput("compose")
	in.Observed.Env["ARGUS_TEST_DIR_HOST"] = `C:\work\my-sut\test-agent`
	p, err := BuildPlan(in)
	if err != nil {
		t.Fatal(err)
	}
	s := a6(t, p)
	if s.SkipReason == "" {
		t.Fatal("A-6 would refresh a test folder OUTSIDE the kit — that is the operator's own scenario work")
	}
	if !strings.Contains(s.SkipReason, "own") {
		t.Errorf("the skip reason does not say whose files these are: %q", s.SkipReason)
	}
}

// ⛔ The measured false sentence must be gone from every skip reason.
func TestPlan_A6NoLongerClaimsScenariosAreNotFiles(t *testing.T) {
	cases := map[string]func(*PlanInput){
		"no test dir recorded": func(in *PlanInput) { delete(in.Observed.Env, "ARGUS_TEST_DIR_HOST") },
		"operator's own":       func(in *PlanInput) { in.Observed.Env["ARGUS_TEST_DIR_HOST"] = `C:\work\x` },
		"a rollback": func(in *PlanInput) {
			in.Observed.Env["ARGUS_TEST_DIR_HOST"] = pathATestDirOnDisk
			in.RollbackTo = "0.3.31"
			in.Version = "0.3.31"
			in.Installed = Manifest{InstanceID: "i1", Version: "0.3.32", Previous: &Previous{Version: "0.3.31", Image: in.ImageDigest}}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			in := fixtureInput("compose")
			mutate(&in)
			p, err := BuildPlan(in)
			if err != nil {
				t.Fatal(err)
			}
			s := a6(t, p)
			if s.SkipReason == "" {
				t.Fatalf("A-6 is planned to run for %q", name)
			}
			if strings.Contains(s.SkipReason, "not in files") {
				t.Errorf("the skip reason still claims the scenarios are not files: %q", s.SkipReason)
			}
		})
	}
}

// pathAMachine is fakeMachine with a Path A test-agent folder inside the kit holding OLD scenarios, and
// a staged kit that ships the NEW baked set.
func pathAMachine(t *testing.T, root, tier string) PlanInput {
	t.Helper()
	in := fakeMachine(t, root, tier)
	test := filepath.Join(in.KitDir, "test-agent")
	for p, s := range map[string]string{
		filepath.Join(test, "scenarios", "http-ingestion", "OLD-001.md"):                                           "OLD-FORMAT",
		filepath.Join(in.StageDir, "kit", "order-service-demo", "scenarios-baked", "http-ingestion", "NEW-001.md"): "NEW-FORMAT",
	} {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	in.Observed.Env["ARGUS_TEST_DIR_HOST"] = test
	return in
}
