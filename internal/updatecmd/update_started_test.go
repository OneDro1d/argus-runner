package updatecmd

import (
	"strings"
	"testing"
)

// AC-D63 (#407): THE BLOCK SAYS IT HAS STARTED, BEFORE IT REPLACES ANYTHING.
//
// The page flipped to the target version at A-3 and the block ran on after it (5-8 s on success, 3 min 12 s on one
// failing attempt), because nothing in the instance payload said an update had begun. apply.sh now reports the
// start once, ahead of the first step, best-effort; its end is A-8b's report (which the control plane reads as the
// end and clears the marker with).

func TestApplyReportsTheStartBeforeAnyStepMovesAnything(t *testing.T) {
	for _, tier := range []string{"compose", "k3d", "managed"} {
		t.Run(tier, func(t *testing.T) {
			in := fixtureInput(tier)
			p, err := BuildPlan(in)
			if err != nil {
				t.Fatal(err)
			}
			apply := RenderApply(p, in)
			call := strings.Index(apply, "\nreport_started || ")
			if call < 0 {
				t.Fatalf("apply.sh never reports that the update has started:\n%s", apply)
			}
			firstStep := strings.Index(apply, "\nCURRENT='A-1'")
			if firstStep < 0 {
				t.Fatalf("apply.sh has no A-1 step to order against")
			}
			if call > firstStep {
				t.Errorf("the start is reported AFTER A-1 began (call at %d, A-1 at %d): the window #407 measured stays open", call, firstStep)
			}
			if first := strings.Index(apply, "APPLIED+=("); first >= 0 && call > first {
				t.Errorf("the start is reported after a step was registered for undo")
			}
			// the preflight guard comes first: a block refused by its own preflight has not started anything
			if guard := strings.Index(apply, "the preflight did not pass"); guard < 0 || guard > call {
				t.Errorf("the start is reported before the preflight guard has passed")
			}
		})
	}
}

// ⛔ BEST-EFFORT, BOTH WAYS: a control plane that is down, or an OLD one with no /fed/update-started route, must never
// stop an update the operator began (C-13) — the call sits in an `||` list, so `set -e` cannot take it.
func TestApplyStartReportIsBestEffortAndNamesTheTargetAndIdentity(t *testing.T) {
	in := fixtureInput("compose")
	p, err := BuildPlan(in)
	if err != nil {
		t.Fatal(err)
	}
	apply := RenderApply(p, in)
	if !strings.Contains(apply, "report_started || printf") {
		t.Errorf("the start report is not guarded by `||`: under set -e a failed report would end the update")
	}
	i := strings.Index(apply, "report_started() {")
	if i < 0 {
		t.Fatal("no report_started function in apply.sh")
	}
	body := apply[i : i+strings.Index(apply[i:], "\n}\n")]
	for _, want := range []string{"runner update started", `--instance-id "$INSTANCE"`, `--identity "$COMPOSE_DIR/identity.$INSTANCE.key"`,
		`--version "${ROLLBACK_TO:-$TARGET_VERSION}"`, `--rollback-to "$ROLLBACK_TO"`, `--control-plane "$CP_URL"`} {
		if !strings.Contains(body, want) {
			t.Errorf("report_started lacks %q:\n%s", want, body)
		}
	}
}

// the machine router moves alone and names no instance: there is nothing to say "started" about
func TestRouterOnlyApplyReportsNoStart(t *testing.T) {
	in := fixtureInput("compose")
	in.RouterOnly = true
	in.InstanceID = ""
	in.Observed = Observed{}
	p, err := BuildPlan(in)
	if err != nil {
		t.Fatal(err)
	}
	if apply := RenderApply(p, in); strings.Contains(apply, "\nreport_started || ") {
		t.Errorf("a router-only apply reports a started update for an instance that does not exist")
	}
}
