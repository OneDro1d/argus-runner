package doctor

import (
	"strings"
	"testing"
	"time"
)

var sutReadAt = time.Date(2026, 10, 1, 11, 0, 0, 0, time.UTC)

// at returns an input whose clock reads `age` after sutReadAt.
func at(in ExecutorInput, age time.Duration) ExecutorInput {
	in.Now = func() time.Time { return sutReadAt.Add(age) }
	return in
}

func noTargets(in ExecutorInput) ExecutorInput {
	zero := 0
	in.ConfigTargets = &zero
	return in
}

func withSUT(e ExecutorStatus, state string, checkedAt *time.Time) ExecutorStatus {
	e.SUTState, e.SUTCheckedAt = state, checkedAt
	return e
}

// documenso is the first open-source SUT onboarded on the compose tier (tester
// verify/2026-10-01-documenso-native): its base_url is the in-network name http://documenso:3100/api/v2,
// which the executor can dial and the operator's host cannot.
func documenso(state string, checkedAt *time.Time) ExecutorStatus {
	e := exec0336("documenso-local")
	e.RunnerVersion = "0.3.49"
	return withSUT(e, state, checkedAt)
}

func TestCheckSUTReachable(t *testing.T) {
	cases := []struct {
		name   string
		in     ExecutorInput
		status Status
		detail []string // every one must appear in Detail
		fix    []string // every one must appear in Fix (case-insensitive); nil = Fix must be empty
	}{
		{"ready is ok, and says an open port is not health",
			asked(documenso("ready", &sutReadAt)), StatusOK,
			[]string{"documenso-local", "reached", "2026-10-01T11:00:00Z", "not proof"}, nil},
		{"unreachable fails, naming the vantage and the in-network name",
			asked(documenso("unreachable", &sutReadAt)), StatusFail,
			[]string{"documenso-local", "could NOT reach", "2026-10-01T11:00:00Z", "executor's network"},
			[]string{"localhost", "service name", "validate-config"}},
		{"never measured is unknown and names the causes",
			asked(documenso("not_checked", nil)), StatusUnknown,
			[]string{"never reported", "argus-config"},
			[]string{"validate-config"}},
		{"a stale reading is unknown, not its old verdict",
			at(asked(documenso("not_checked", &sutReadAt)), 10*time.Minute), StatusUnknown,
			[]string{"2026-10-01T11:00:00Z", "older than 3 minutes", "not evidence about now"},
			[]string{"executor"}},
		{"a fresh not_checked is a probe that ran and could not conclude, not a stale reading",
			at(asked(documenso("not_checked", &sutReadAt)), 30*time.Second), StatusUnknown,
			[]string{"the probe ran at 2026-10-01T11:00:00Z", "could not conclude", "ARGUS_VALIDATE_NO_PROBE", "declares no targets"},
			[]string{"validate-config --config <argus-config.yaml>", "names the target"}},
		{"a reading exactly at the window is still fresh",
			at(asked(documenso("not_checked", &sutReadAt)), SUTStaleAfter), StatusUnknown,
			[]string{"could not conclude"}, []string{"validate-config"}},
		{"a fresh not_checked with a config that declares no targets is a skip",
			noTargets(at(asked(documenso("not_checked", &sutReadAt)), 30*time.Second)), StatusSkip,
			[]string{"declares no targets", "nothing to probe", "runs themselves"}, nil},
		{"a stale not_checked stays unknown even when the config declares no targets",
			noTargets(at(asked(documenso("not_checked", &sutReadAt)), 10*time.Minute)), StatusUnknown,
			[]string{"older than 3 minutes"}, []string{"executor"}},
		{"a control plane that publishes no sut_state is unknown",
			asked(documenso("", nil)), StatusUnknown,
			[]string{"does not publish", "sut_state"},
			[]string{"control plane"}},
		// executor-version is already unknown for these two; failing the same cause twice is noise.
		{"not asked is a skip that points at the check that explains it",
			ExecutorInput{ControlPlaneURL: "https://argus-dev.onedroid.ai", NotAsked: "no control-plane credential to ask with"}, StatusSkip,
			[]string{"not asked", "no control-plane credential", "executor-version"}, nil},
		{"an unreadable list is a skip and is not a statement about the SUT",
			ExecutorInput{ControlPlaneURL: "https://argus-dev.onedroid.ai", Err: "dial tcp: connection refused"}, StatusSkip,
			[]string{"connection refused", "NOT a statement about the SUT", "executor-version"}, nil},
		{"no executor is a skip: executor-version already fails for it",
			asked(), StatusSkip,
			[]string{"no executor"}, nil},
		{"the worst executor decides",
			asked(documenso("ready", &sutReadAt), withSUT(exec0336("sut-demo"), "unreachable", &sutReadAt)), StatusFail,
			[]string{"documenso-local", "sut-demo", "could NOT reach"},
			[]string{"sut-demo"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := CheckSUTReachable(tc.in)
			if c.ID != "sut-reachable" {
				t.Fatalf("ID = %q", c.ID)
			}
			if c.Status != tc.status {
				t.Fatalf("status = %s, want %s: %+v", c.Status, tc.status, c)
			}
			if strings.Contains(c.Detail, "could not conclude") && strings.Contains(c.Detail, "older than") {
				t.Errorf("a fresh reading must not be called old:\n%s", c.Detail)
			}
			if strings.Contains(c.Detail, "never reported") && strings.Contains(c.Detail, "ARGUS_VALIDATE_NO_PROBE") {
				t.Errorf("ARGUS_VALIDATE_NO_PROBE is a fresh-unknown cause, not a never-reported one:\n%s", c.Detail)
			}
			for _, d := range tc.detail {
				if !strings.Contains(c.Detail, d) {
					t.Errorf("Detail lacks %q:\n%s", d, c.Detail)
				}
			}
			if tc.fix == nil && c.Fix != "" {
				t.Errorf("Fix = %q, want none", c.Fix)
			}
			for _, f := range tc.fix {
				if !strings.Contains(strings.ToLower(c.Fix), strings.ToLower(f)) {
					t.Errorf("Fix lacks %q:\n%s", f, c.Fix)
				}
			}
		})
	}
}

// --instance narrows the report the way executor-version's does: another executor's dead SUT is not
// this run's problem.
func TestCheckSUTReachable_InstanceNarrows(t *testing.T) {
	in := asked(documenso("ready", &sutReadAt), withSUT(exec0336("sut-demo"), "unreachable", &sutReadAt))
	in.InstanceID = "documenso-local"
	if c := CheckSUTReachable(in); c.Status != StatusOK || strings.Contains(c.Detail, "sut-demo") {
		t.Fatalf("narrowed to documenso-local: %+v", c)
	}
	in.InstanceID = "absent"
	if c := CheckSUTReachable(in); c.Status != StatusSkip {
		t.Fatalf("an unknown --instance is executor-version's to fail, not a second failure here: %+v", c)
	}
}

// An old executor that has not reported is NOT told the probe is newer than it. The probe predates
// this repository, and 0.3.36 executors (sut-demo, hub-tester) were read live on argus-dev on
// 2026-10-01 reporting "ready" (tester verify/2026-10-01-doctor-sut-reachable/live-doctor.out).
func TestCheckSUTReachable_NeverMeasuredIsNotBlamedOnTheVersion(t *testing.T) {
	c := CheckSUTReachable(asked(withSUT(exec0336("sut-demo"), sutNotChecked, nil)))
	if c.Status != StatusUnknown || strings.Contains(c.Detail+c.Fix, "0.3.3") {
		t.Fatalf("a 0.3.36 executor with no reading: %+v; want unknown, with no version advice", c)
	}
}
