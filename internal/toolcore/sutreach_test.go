package toolcore

import "testing"

// VR6-W1 (V23-011) — collapsing a Reachability report into the ONE tri-state the control plane carries.
//
// ── WHY A TRI-STATE AND NOT A BOOLEAN ─────────────────────────────────────────────────────────────
//
// Confirmed twice by measurement. `memstore-compose` ran TWO HOURS with zero error lines while its SUT had
// been dead since a reboot. `social-mcp-k3d` showed HEALTHY while its SUT had been deaf ~2.5 hours with
// 13 messages queued against zero consumers. Both times the executor was healthy and reporting
// faithfully — the Environments page simply has no channel through which the SUT's state can arrive.
//
// A boolean would open a second one. `false` would have to mean both "I dialled it and it refused" and
// "I have not dialled it", and the page would render one of those as a red verdict on a SUT nobody
// measured. Grey is a state, not a rendering of missing data.
//
// ── THE PRECEDENCE, AND WHY IT IS IN THIS ORDER ───────────────────────────────────────────────────
//
//	any UNREACHABLE      -> false   a measured failure is the loudest true fact on the report
//	else any not_checked -> nil     partial evidence is not evidence; summarize() already says so:
//	                                "nothing failed, but that is not the same as everything passing"
//	else at least one OK -> true
//	else (no targets)    -> nil     an empty report is not a pass

func boolPtrStr(p *bool) string {
	if p == nil {
		return "nil (NOT MEASURED)"
	}
	if *p {
		return "true (READY)"
	}
	return "false (UNREACHABLE)"
}

func TestSUTReachableTriState_NoTargetsIsNotMeasuredRatherThanReachable(t *testing.T) {
	got := SUTReachableTriState(summarize(nil))
	if got != nil {
		t.Fatalf("a SUT with no dialable targets reported %s, want nil.\n"+
			"An empty report is the absence of evidence. Rendering it as a verdict — in either "+
			"direction — is the failure this whole requirement exists to remove", boolPtrStr(got))
	}
}

// 🚩 THE ONE THAT MATTERS — memstore-compose and social-mcp-k3d, verbatim.
func TestSUTReachableTriState_ADeadTargetIsFalse(t *testing.T) {
	got := SUTReachableTriState(summarize([]TargetReach{
		{Target: "http", Status: ReachOK},
		{Target: "database", Status: ReachDown},
	}))
	if got == nil || *got {
		t.Fatalf("a SUT with a target that REFUSED the dial reported %s, want false.\n"+
			"memstore-compose ran two hours with zero error lines in exactly this state", boolPtrStr(got))
	}
}

// Precedence: a measured failure outranks a missing measurement. Otherwise one unprobeable target could
// hide a genuinely dead one behind grey.
func TestSUTReachableTriState_DownOutranksNotChecked(t *testing.T) {
	got := SUTReachableTriState(summarize([]TargetReach{
		{Target: "http", Status: ReachDown},
		{Target: "mcp", Status: ReachUnknown},
	}))
	if got == nil || *got {
		t.Fatalf("down + not_checked reported %s, want false — a measured failure must not be "+
			"downgraded to grey by an unrelated gap in coverage", boolPtrStr(got))
	}
}

// ⚠ THE PARTIAL CASE. summarize() already phrases this correctly in prose — "nothing failed, but that is
// not the same as everything passing" — and the tri-state must not contradict its own summary line.
func TestSUTReachableTriState_SomeReachableAndSomeNotCheckedIsNotMeasured(t *testing.T) {
	got := SUTReachableTriState(summarize([]TargetReach{
		{Target: "http", Status: ReachOK},
		{Target: "database", Status: ReachUnknown},
	}))
	if got != nil {
		t.Fatalf("one reachable + one unchecked reported %s, want nil.\n"+
			"Reporting READY here promises the operator something nobody measured: the unchecked "+
			"target is exactly where a dead SUT would be hiding", boolPtrStr(got))
	}
}

// The probe kill-switch must produce silence, never a green nobody earned.
func TestSUTReachableTriState_EverythingUncheckedIsNotMeasured(t *testing.T) {
	got := SUTReachableTriState(summarize([]TargetReach{
		{Target: "http", Status: ReachUnknown},
		{Target: "mcp", Status: ReachUnknown},
	}))
	if got != nil {
		t.Fatalf("an all-unchecked report gave %s, want nil", boolPtrStr(got))
	}
}

func TestSUTReachableTriState_AllReachableIsTrue(t *testing.T) {
	got := SUTReachableTriState(summarize([]TargetReach{
		{Target: "http", Status: ReachOK},
		{Target: "database", Status: ReachOK},
	}))
	if got == nil || !*got {
		t.Fatalf("an all-reachable report gave %s, want true", boolPtrStr(got))
	}
}

// It must agree with the AllReachable field on every report rather than re-deriving a second, subtly
// different opinion. Two answers to one question is how a page ends up contradicting its own tooltip.
func TestSUTReachableTriState_TrueExactlyWhenAllReachable(t *testing.T) {
	cases := [][]TargetReach{
		nil,
		{{Target: "http", Status: ReachOK}},
		{{Target: "http", Status: ReachDown}},
		{{Target: "http", Status: ReachUnknown}},
		{{Target: "http", Status: ReachOK}, {Target: "mcp", Status: ReachDown}},
		{{Target: "http", Status: ReachOK}, {Target: "mcp", Status: ReachUnknown}},
		{{Target: "http", Status: ReachOK}, {Target: "mcp", Status: ReachOK}},
	}
	for _, tc := range cases {
		r := summarize(tc)
		got := SUTReachableTriState(r)
		isTrue := got != nil && *got
		if isTrue != r.AllReachable {
			t.Errorf("tri-state=%s but AllReachable=%v for %v — the two must never disagree",
				boolPtrStr(got), r.AllReachable, tc)
		}
	}
}
