package mcp

// ⚠ VR12-E8 (V29-017): these fixtures moved from the scalar Expect.BodyContains /
// Expect.BodyMatches to Expect.Body, because the judge now evaluates EVERY declared assertion
// and ANDs them -- the old scalars kept only the FIRST of each kind. The BEHAVIOUR each case
// asserts is unchanged: a content miss must still fail on the body plane, and the scan must
// still read result.content[*].text rather than the raw envelope. Only the carrying field moved.

import (
	"encoding/json"
	"strings"
	"testing"
)

// VR10-S2 (V28-014): the judge scans result.content[*].text when the scenario asserts on the
// answer's CONTENT. Before this build Judge decided purely on the two planes and never read
// Content, so a scenario whose real claim was about the answer was green while the claim was
// false — measured: 2 of 21 green RACE steps proved their own claim.

func okWith(text string) CallResult {
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": "tr-1",
		"result": map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}, "isError": false}})
	return CallResult{IsError: false, Content: []ContentBlock{{Type: "text", Text: text}}, Raw: raw}
}

// Acceptance 1 (the positive control, single-call form): an EMPTY result set with
// `body has results containing argus-race` must FAIL — on today's tree it passes.
func TestJudge_BodyContains_MissFailsOnTheBodyPlane(t *testing.T) {
	v := Judge(okWith(`{"results":[]}`), Expect{ErrorPlane: PlaneNone, Body: []BodyAssert{{Op: BodyContainsOp, Value: "argus-race"}}})
	if v.Pass {
		t.Fatalf("a content miss must FAIL the step: %+v", v)
	}
	if v.Plane != PlaneBody {
		t.Errorf("plane = %q, want %q (a distinct plane for a content miss)", v.Plane, PlaneBody)
	}
	if v.Observed != BodyAssertObserved {
		t.Errorf("observed must be the reality-only constant, got %q", v.Observed)
	}
	// VR-C8: observed is shown to BOTH hats — the asserted value must never be echoed.
	if strings.Contains(v.Observed, "argus-race") {
		t.Errorf("observed echoes the asserted value: %q", v.Observed)
	}
}

// Acceptance 2: a hit still passes — no false reds introduced.
func TestJudge_BodyContains_HitPasses(t *testing.T) {
	v := Judge(okWith(`{"results":[{"id":"argus-race-7"}]}`), Expect{ErrorPlane: PlaneNone, Body: []BodyAssert{{Op: BodyContainsOp, Value: "argus-race"}}})
	if !v.Pass || v.Plane != "ok" {
		t.Fatalf("a content hit must pass on the ok plane: %+v", v)
	}
}

func TestJudge_BodyMatches_RegexHitAndMiss(t *testing.T) {
	re := `"count":\s*[1-9]`
	if hit := Judge(okWith(`{"count":3}`), Expect{ErrorPlane: PlaneNone, Body: []BodyAssert{{Op: BodyMatchesOp, Value: re}}}); !hit.Pass {
		t.Errorf("regex hit must pass: %+v", hit)
	}
	miss := Judge(okWith(`{"count":0}`), Expect{ErrorPlane: PlaneNone, Body: []BodyAssert{{Op: BodyMatchesOp, Value: re}}})
	if miss.Pass || miss.Plane != PlaneBody {
		t.Errorf("regex miss must fail on the body plane: %+v", miss)
	}
	if strings.Contains(miss.Observed, re) {
		t.Errorf("observed echoes the asserted regex: %q", miss.Observed)
	}
}

// VR10-S2-3 (owner-locked): the match is against result.content[*].text ONLY — never Raw. The
// envelope echoes the request (id, _meta.request_id), so matching it would let a scenario pass on
// its own correlation id.
func TestJudge_BodyScansContentTextNeverTheRawEnvelope(t *testing.T) {
	r := CallResult{IsError: false, Content: []ContentBlock{{Type: "text", Text: `{"results":[]}`}},
		Raw: json.RawMessage(`{"jsonrpc":"2.0","id":"argus-race-tr-1","_meta":{"request_id":"argus-race"},` +
			`"result":{"content":[{"type":"text","text":"{\"results\":[]}"}],"isError":false}}`)}
	if v := Judge(r, Expect{ErrorPlane: PlaneNone, Body: []BodyAssert{{Op: BodyContainsOp, Value: "argus-race"}}}); v.Pass {
		t.Fatalf("the value appears only in the envelope (the request echo) — must NOT pass: %+v", v)
	}
}

// Every content block is scanned (concatenated), not only content[0].
func TestJudge_BodyScansEveryContentBlock(t *testing.T) {
	r := CallResult{Content: []ContentBlock{{Type: "text", Text: "first block"}, {Type: "text", Text: `{"id":"argus-race-9"}`}}}
	if v := Judge(r, Expect{ErrorPlane: PlaneNone, Body: []BodyAssert{{Op: BodyContainsOp, Value: "argus-race-9"}}}); !v.Pass {
		t.Fatalf("a value in the second content block must be found: %+v", v)
	}
}

// ⛔ TestJudge_ErrorPlaneExpected_NotGatedByBody LIVED HERE and is DELETED (V31-005).
//
// It pinned V28-014 part B — "a step expecting isError:true is NOT additionally gated by a body
// assertion, its claim IS the error state" — which the owner reversed on 2026-09-11: a `### Runnable`
// check on an expected error is EVALUATED. Its failure against the new judge was the fix working, and
// judge_errorbody_test.go asserts the behaviour that replaced it. The rule it pinned is history and
// the row that made it is not edited.

// VR10-S2-2, as amended by V31-005: body is judged only AFTER the expected plane matched; a plane
// failure still reports the plane.
func TestJudge_PlaneFailureIsReportedBeforeBody(t *testing.T) {
	v := Judge(CallResult{IsError: true, Content: []ContentBlock{{Type: "text", Text: "x"}}},
		Expect{ErrorPlane: PlaneNone, Body: []BodyAssert{{Op: BodyContainsOp, Value: "x"}}})
	if v.Pass || v.Plane != "ok" || v.Observed != "responder returned result.isError:true" {
		t.Fatalf("a tool error with success expected is a PLANE failure, not a body one: %+v", v)
	}
}

// VR10-S2-13: softWarn stays advisory — a body HIT with a buried error marker still passes,
// and the soft warning still fires.
func TestJudge_BodyHit_SoftWarningStaysAdvisory(t *testing.T) {
	v := Judge(okWith(`{"error_code":"NONE","id":"argus-race-1"}`), Expect{ErrorPlane: PlaneNone, Body: []BodyAssert{{Op: BodyContainsOp, Value: "argus-race-1"}}})
	if !v.Pass || v.SoftWarning == "" {
		t.Fatalf("body hit must pass with the advisory soft warning intact: %+v", v)
	}
}

// VR-C8 regression guard for the MCP constant (mirrors argus.bodyAssertObserved's test): the
// reality-only message carries no echoed value and no template that would interpolate one.
func TestBodyAssertObserved_IsRealityOnly(t *testing.T) {
	if strings.TrimSpace(BodyAssertObserved) == "" {
		t.Fatal("BodyAssertObserved must be a non-empty reality-only message")
	}
	for _, leaky := range []string{"contains '", "match /", "containing", "= '", "expected value is", "%s", "%q"} {
		if strings.Contains(BodyAssertObserved, leaky) {
			t.Fatalf("BodyAssertObserved must not echo the asserted value (found %q): %q", leaky, BodyAssertObserved)
		}
	}
	if !strings.Contains(BodyAssertObserved, "result.content") {
		t.Fatalf("BodyAssertObserved should name what was measured (result.content): %q", BodyAssertObserved)
	}
}
