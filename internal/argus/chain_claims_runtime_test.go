package argus

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// VR12-E10 AT RUN TIME — the step is judged on the `## EXPECT` bullet, and moving the claim there
// changes NOTHING about when `${saved.…}` binds.
//
// These are the cases that would catch the migration having quietly changed what a scenario proves.
// The static half (the holdout diff of all 36 migrated files against their pre-migration assertion
// sets) lives outside the suite; this is the behavioural half.

func chainRunMD(base, trigger, expect string) string {
	return strings.Join([]string{
		"# Scenario: c", "",
		"## Metadata",
		"- **ID**: CHN-RT",
		"- **Layer**: Permissions",
		"- **Tags**: chain", "",
		"## TRIGGER",
		"POST `chain`", "",
		"```json",
		strings.ReplaceAll(trigger, "${BASE}", base),
		"```", "",
		"## EXPECT",
		expect,
		"## TIMEOUT", "60s", "",
		"## CLEANUP", "N/A — a unit-test fixture; it creates nothing.", "",
	}, "\n")
}

// TS-E10-1 — the step is judged on the EXPECT bullet. The server answers `{"id":"abc"}`; the claim
// demands `abc`, so it passes, and demanding something absent fails.
func TestChainRun_JudgesOnTheExpectBullet(t *testing.T) {
	ts := httptest.NewServer(fakeMCPTextServer(`{"id":"abc"}`))
	defer ts.Close()
	c := chainCfg(t, ts.URL)
	trig := `{"steps":[{"type":"mcp","name":"create","tool":"t","args":{}}]}`

	pass := scenario.Parse(chainRunMD(ts.URL, trig, "### Runnable\n- step create: body has id containing abc\n"))
	if r := runChainScenario(c, pass, "tr-e10-1", "testkit/ui"); r.Status != "passed" {
		t.Fatalf("a satisfied EXPECT claim must pass: %s / %+v", r.Status, r.Failure)
	}
	fail := scenario.Parse(chainRunMD(ts.URL, trig, "### Runnable\n- step create: body has id containing zzz\n"))
	r := runChainScenario(c, fail, "tr-e10-1b", "testkit/ui")
	if r.Status == "passed" {
		t.Fatal("an UNSATISFIED EXPECT claim must fail — otherwise the claims moved home and stopped " +
			"being executed, which is the defect V30-002 exists to remove")
	}
}

// TS-E10-2 — several bullets on one step are ANDed. The second is false, so the step fails even
// though the first holds; a parser that kept only the first would report green.
func TestChainRun_SeveralBulletsOnOneStepAreANDed(t *testing.T) {
	ts := httptest.NewServer(fakeMCPTextServer(`{"id":"abc"}`))
	defer ts.Close()
	c := chainCfg(t, ts.URL)
	s := scenario.Parse(chainRunMD(ts.URL,
		`{"steps":[{"type":"mcp","name":"create","tool":"t","args":{}}]}`,
		"### Runnable\n- step create: body has id containing abc\n- step create: body has id containing zzz\n"))
	if r := runChainScenario(c, s, "tr-e10-2", "testkit/ui"); r.Status == "passed" {
		t.Fatal("two claims on one step must BOTH hold — only the first was enforced")
	}
}

// TS-E10-3 — ⛔ `${saved.<var>}` still binds at RUN time, per step, from the capture store as it
// stands when that step runs. Moving the text out of the step JSON must not move the binding.
//
// The fake echoes its arguments back as the content text, so step 2's answer contains whatever
// step 1 saved — which is only true if the binding happened at execution time.
func TestChainRun_SavedBindingIsUnchangedByTheMove(t *testing.T) {
	ts := httptest.NewServer(fakeMCPTextServer("")) // echoes the args as the content text
	defer ts.Close()
	c := chainCfg(t, ts.URL)
	s := scenario.Parse(chainRunMD(ts.URL, `{"steps":[
  {"type":"mcp","name":"write","tool":"t","args":{"id":"doc-42"},"save":{"docId":"id"}},
  {"type":"mcp","name":"read","tool":"t","args":{"document_id":"${saved.docId}"}}
]}`, "### Runnable\n- step write: result.isError == false\n- step read: body has document_id containing ${saved.docId}\n"))
	r := runChainScenario(c, s, "tr-e10-3", "testkit/ui")
	if r.Status != "passed" {
		t.Fatalf("a ${saved.…} claim in ## EXPECT must bind at run time exactly as it did inside the "+
			"step: %s / %+v", r.Status, r.Failure)
	}
}

// ⛔ TS-E10-6 (run half) STOOD HERE AND IS DELETED BY V31-002 (R2).
//
// It asserted VR12-X5: an un-migrated scenario — its claims inside each step's in-JSON `expect` —
// runs exactly as it does today, and its claims are still ENFORCED rather than merely tolerated.
// That was the right promise while the migration was in flight. The owner then ruled old-format
// scenarios unsupported (2026-09-11), so the key is removed from the struct and the runner and
// refused by name at authoring time: there is no un-migrated path left to run, which is the whole
// point of the removal. What replaces the promise is that the author is TOLD, at write time, in
// words that say what to do (validate.go's W1).

// A malformed `- step …` bullet must stop the chain at preflight, quoting the bullet — never be
// skipped so the chain runs asserting less than its author wrote.
func TestChainRun_UnparseableClaimStopsThePreflight(t *testing.T) {
	ts := httptest.NewServer(fakeMCPTextServer(`{"id":"abc"}`))
	defer ts.Close()
	c := chainCfg(t, ts.URL)
	s := scenario.Parse(chainRunMD(ts.URL,
		`{"steps":[{"type":"mcp","name":"create","tool":"t","args":{}}]}`,
		"### Runnable\n- step create returns result.isError == false\n"))
	r := runChainScenario(c, s, "tr-e10-x", "testkit/ui")
	if r.Status == "passed" {
		t.Fatal("a bullet that claims to be a chain claim and parses as none must not be silently skipped")
	}
	if r.Failure == nil || !strings.Contains(r.Failure.Observed, "claim parse error") {
		t.Errorf("the failure must name the parse error, got %+v", r.Failure)
	}
}

// V31-003 (VR13-CID) — a chain CLAIM naming the run's own id. The claims form's twin of
// TestRunChainScenario_CidBindsIntoTheAssertion (the legacy step-`expect` form): V30-002 moved the
// claims into `## EXPECT`, where they are read RAW, so the id stopped being filled in — which is
// what made the owner's RACE-002 and RACE-004 fail on values that WERE in the answer.
func TestChainRun_CorrelationIDInAClaimIsFilledIn(t *testing.T) {
	ts := httptest.NewServer(fakeMCPTextServer(""))
	defer ts.Close()
	const corr = "tr-cid-88"
	md := chainRunMD(ts.URL,
		`{"steps":[{"type":"mcp","name":"write","tool":"t","args":{"name":"m-${cid}","ref":"r-${correlation_id}"}}]}`,
		strings.Join([]string{
			"### Runnable",
			"- step write: body has name containing m-${cid}",
			"- step write: body has ref containing r-${correlation_id}", "",
			"### Non-runnable",
			"- the step echoes its arguments", "",
		}, "\n"))
	res := runChainScenario(chainCfg(t, ts.URL), scenario.Parse(md), corr, "testkit/ui")

	if res.Status != "passed" {
		obs := ""
		if res.Failure != nil {
			obs = res.Failure.Observed
		}
		t.Fatalf("status = %q (observed %q), want passed — the id was never filled into the claims", res.Status, obs)
	}
	if len(res.Steps) != 1 {
		t.Fatalf("want one step, got %d", len(res.Steps))
	}
	enforced := strings.Join(res.Steps[0].AssertionsEnforced, ";")
	for _, want := range []string{"m-" + corr, "r-" + corr} {
		if !strings.Contains(enforced, want) {
			t.Errorf("assertions_enforced = %q, want it to contain %q", enforced, want)
		}
	}
	if strings.Contains(enforced, "${") {
		t.Errorf("assertions_enforced still carries a placeholder: %q", enforced)
	}
}

// ⛔ TWO TESTS STOOD HERE AND ARE DELETED BY V31-002 (R2).
//
// TestParseChainSpec_StepExpectIsReadAsWritten and
// TestChainRun_LegacyExpectWithAnUnfilledPlaceholderStopsAtPreflight were written one commit
// earlier, by V31-003: the first proved an environment value is not filled into a step's deprecated
// `expect` (a check is printed in the report, so a secret in one would be published), and the second
// proved that a step `expect` still carrying a placeholder stops the chain rather than losing half
// its meaning. Both were right about the key THEN.
//
// V31-002 removes the key itself — from the struct, from the runner, and by name at authoring time —
// so there is no longer a step `expect` for either rule to govern. The property they protected
// survives, in the home the claims actually live in now: TestChainRun_CorrelationIDInAClaimIsFilledIn
// (below) and TestCheck_EnvironmentVariableIsNeverFilledIn (internal/argus/correlationid_test.go),
// which refuses an environment variable in any `### Runnable` check.
