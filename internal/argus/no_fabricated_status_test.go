package argus

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// V29-016 — NEITHER OF THE TWO SITES MAY INVENT A `202`.
//
// The row named ONE site (DeriveProps). The V31 build found the second (judge) and verified it in
// the tree before writing it down; VR12-E7 had made that second site MORE reachable, not less,
// because an undeclared scenario now yields an empty slice and lands squarely on it. Closing one
// without the other leaves half the defect standing, so both are asserted here, side by side.

// SITE 2 — judge(). ⭐ This is the test the row asks for by name: "judge(codes, nil, nil) must NOT
// silently pass a 202 response. Today it does, and no test covers it."
func TestJudge_WithNoDeclaredStatus_DoesNotInvent202(t *testing.T) {
	pass, observed := judge([]int{202}, nil, nil)
	if pass {
		t.Fatalf("judge PASSED a 202 against an expectation nobody declared — the fabricated " +
			"`default contract` is still there (V29-016 site 2)")
	}
	if !strings.Contains(observed, "declares no expected status") {
		t.Errorf("observed must name the ABSENCE of a declaration so the report is legible, got %q", observed)
	}
	// …and it must not have quietly become "fail everything": a declared expectation still decides.
	if ok, _ := judge([]int{202}, []int{202}, nil); !ok {
		t.Error("a DECLARED status that matches must still pass")
	}
	if ok, _ := judge([]int{200}, []int{202}, nil); ok {
		t.Error("a DECLARED status that does not match must still fail")
	}
}

// SITE 1 — DeriveProps. The property is ABSENT, not defaulted.
func TestDeriveProps_EmitsNoStatusWhenNoneIsDeclared(t *testing.T) {
	p := propsFor(t, scen("HTTP Ingestion", "### Runnable\n- body has order_id containing 01\n"))
	if v, ok := p["expect.status"]; ok {
		t.Errorf("expect.status = %q — no status was declared, and `202` appears nowhere in the "+
			"scenario file (V29-016 site 1)", v)
	}
	// The declared case is untouched.
	q := propsFor(t, scen("HTTP Ingestion", "### Runnable\n- status=200\n"))
	if q["expect.status"] != "200" {
		t.Errorf("a DECLARED status must still be emitted, got %q", q["expect.status"])
	}
}

// THE VERDICT — a scenario that cannot be judged is reported as `error`, never as a SUT `failed`,
// and the runner never fires its request. Calling it `failed` is the mis-attribution the row is
// about, one level up: it sends a triager into the SUT for a defect that lives in the scenario file.
func TestRunAll_UndeclaredStatus_IsErroredAndNeverFired(t *testing.T) {
	dir := t.TempDir()
	sc := filepath.Join(dir, "scenarios")
	if err := os.MkdirAll(filepath.Join(sc, "http-ingestion"), 0o755); err != nil {
		t.Fatal(err)
	}
	md := strings.Join([]string{
		"# Scenario: legacy", "",
		"## Metadata",
		"- **ID**: LEG-001",
		"- **Layer**: HTTP Ingestion",
		"- **Tags**: http, order", "",
		"## TRIGGER",
		"GET `${INGESTION_URL}/health`", "",
		"## EXPECT",
		"### Runnable",
		"- body has status containing ok", "",
		"## TIMEOUT", "30s", "",
		"## CLEANUP", "N/A — a unit-test fixture; it creates nothing.", "",
	}, "\n")
	if err := os.WriteFile(filepath.Join(sc, "http-ingestion", "LEG-001.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}

	rec := &recordingRunner{}
	rr, err := RunAll(httpConfig(), sc, filepath.Join(dir, "results"), "p", "", "", "", "", rec)
	if err != nil {
		t.Fatalf("RunAll: %v", err)
	}
	res, _ := rr.Report.Find("LEG-001")
	if res == nil {
		t.Fatal("LEG-001 is missing from the report — a scenario that cannot be judged must still be REPORTED")
	}
	if res.Status != report.StatusError {
		t.Errorf("status = %q, want %q — nothing about the SUT was measured, so this is not a SUT failure",
			res.Status, report.StatusError)
	}
	if res.Failure == nil || !strings.Contains(res.Failure.Observed, "declares no expected status") {
		t.Errorf("the observed must name the missing declaration, got %+v", res.Failure)
	}
	if res.Failure != nil && res.Failure.Expected != nil {
		t.Error("nothing was compared, so there is no `expected` to report (same rule as the dead-rig branch)")
	}
	if len(rec.props) != 0 {
		t.Errorf("the runner fired %d request(s) for a scenario that can decide nothing", len(rec.props))
	}

	// TIER 3 — and it is NAMED in the unexecuted list, which is what tells the author why.
	if len(res.Unexecuted) == 0 {
		t.Fatal("tier 3 must name the missing status — the defect was invisible precisely because " +
			"nothing in the file pointed at it (V29-016 (c))")
	}
	var found bool
	for _, u := range res.Unexecuted {
		if strings.Contains(u.Bullet, "status=") && strings.Contains(u.Reason, "no code was invented") {
			found = true
		}
	}
	if !found {
		t.Errorf("the tier-3 entry must name the missing `status=` bullet, got %+v", res.Unexecuted)
	}
}

// THE KEYING — the rule fires on exactly the set the RUNTIME judges by response code, and on no
// other. A layer-keyed rule would have wrongly refused 84 of the 119 example scenarios; this test
// is what stops that regression being re-introduced by someone who reads "status layer" and acts.
func TestJudgedByResponseCode_MatchesTheRuntimeDispatch(t *testing.T) {
	cases := []struct {
		name  string
		tags  string
		layer string
		want  bool
	}{
		{"plain http scenario", "order", "HTTP Ingestion", true},
		{"error path", "order", "Error Path", true},
		{"chain scenario claiming an HTTP layer", "chain", "HTTP Ingestion", false},
		{"mcp scenario claiming an HTTP layer", "mcp", "HTTP Ingestion", false},
		{"ui scenario claiming an HTTP layer", "ui", "HTTP Ingestion", false},
		{"database state", "order", "Database State", false},
		{"message flow", "order", "Message Flow", false},
		{"external delivery", "order", "External Delivery", false},
		{"saga presence", "saga-presence", "HTTP Ingestion", false},
		{"chained terminal layer decides", "order", "HTTP Ingestion -> Database State", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := scenario.Parse(scenTagged(c.layer, c.tags, "### Runnable\n- status=200\n"))
			if got := scenario.JudgedByResponseCode(s); got != c.want {
				t.Errorf("JudgedByResponseCode = %v, want %v — the validator and the runner must agree "+
					"about which engine runs this scenario", got, c.want)
			}
		})
	}
}
