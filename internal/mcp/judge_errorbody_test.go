package mcp

import (
	"encoding/json"
	"strings"
	"testing"
)

// V31-005 (VR13-EE) — A CHECK ON AN EXPECTED ERROR IS EVALUATED.
//
// Until 0.3.32 a scenario that expected `result.isError == true` (or a protocol error) had its
// `body …` checks SKIPPED entirely: the plane matched, the judge returned Pass, and the content
// assertion the author wrote was never looked at. An error-contract test could therefore state the
// error it expected and prove only that SOME error came back — measured on 0.3.31 against fifteen
// shipped examples, every one of which named a message nothing compared.
//
// The owner reversed V28-014's part B for this: a `### Runnable` check on an expected error is
// EVALUATED. On the tool plane it is matched against `result.content[*].text`, as on success; on the
// protocol plane, against the WHOLE JSON-RPC error object — his words, "the whole error object" —
// so `code`, `message` and `data` are all reachable.
//
// ⚠ What this does NOT change: the plane still decides first. A wrong plane, a wrong code, a
// throttle or an unreachable endpoint is reported on its own plane and no body is ever read.

// ⚠ `toolErr` already exists in judge_ratelimit_test.go:9 — same package, so it is reused.

func TestJudge_ToolErrorExpected_BodyIsJudged(t *testing.T) {
	const answer = "object not found"

	t.Run("a check that holds passes on the tool plane", func(t *testing.T) {
		v := Judge(toolErr(answer), Expect{ErrorPlane: PlaneTool,
			Body: []BodyAssert{{Op: BodyContainsOp, Value: "object not found"}}})
		if !v.Pass {
			t.Fatalf("want Pass, got %+v", v)
		}
		if v.Plane != "tool" {
			t.Errorf("Plane = %q, want %q", v.Plane, "tool")
		}
	})

	t.Run("a check that misses FAILS — this is the whole row", func(t *testing.T) {
		v := Judge(toolErr(answer), Expect{ErrorPlane: PlaneTool,
			Body: []BodyAssert{{Op: BodyContainsOp, Value: "never-there"}}})
		if v.Pass {
			t.Fatalf("the scenario PASSED while its own content assertion was never satisfied: %+v", v)
		}
		if v.Plane != PlaneBody {
			t.Errorf("Plane = %q, want %q", v.Plane, PlaneBody)
		}
		if v.Observed != BodyAssertObservedToolError {
			t.Errorf("Observed = %q, want BodyAssertObservedToolError", v.Observed)
		}
		// VR-C8: Observed reaches the PRODUCT hat, so it must never echo the asserted value.
		if strings.Contains(v.Observed, "never-there") {
			t.Errorf("Observed leaks the asserted value: %q", v.Observed)
		}
	})

	t.Run("a regex check", func(t *testing.T) {
		if v := Judge(toolErr(answer), Expect{ErrorPlane: PlaneTool,
			Body: []BodyAssert{{Op: BodyMatchesOp, Value: "^object"}}}); !v.Pass {
			t.Errorf("want Pass, got %+v", v)
		}
	})
}

// The Social answer shape this release's examples now assert on (V31-004 rewrote them to check
// stable fields). It is a JSON object inside the tool error's text, so the field-scoped forms have
// to resolve through it.
const socialERR001 = `{"constraint":{"field":"since_days","got":0,"max":90,"min":1,"rule":"since_days_invalid"},"error_code":"CONTENT_VALIDATION_FAILED","message":"since_days must be an integer 1..90 inclusive.","provider":"ayrshare","suggested_action":"Supply since_days between 1 and 90."}`

func TestJudge_ToolErrorExpected_FieldScopedOnAJSONError(t *testing.T) {
	for _, c := range []struct {
		name string
		a    BodyAssert
		pass bool
	}{
		{"error_code", BodyAssert{Field: "error_code", Op: BodyContainsOp, Value: "CONTENT_VALIDATION_FAILED"}, true},
		{"constraint.rule", BodyAssert{Field: "constraint.rule", Op: BodyContainsOp, Value: "since_days_invalid"}, true},
		// a number is rendered as its JSON text, so `^0$` pins it exactly — `containing 0` would
		// also pass on 10, which is why the examples use `matching`.
		{"constraint.got", BodyAssert{Field: "constraint.got", Op: BodyMatchesOp, Value: "^0$"}, true},
		{"the wrong rule", BodyAssert{Field: "constraint.rule", Op: BodyContainsOp, Value: "id_empty"}, false},
		{"a field that is not there", BodyAssert{Field: "no_such_field", Op: BodyExistsOp}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			v := Judge(toolErr(socialERR001), Expect{ErrorPlane: PlaneTool, Body: []BodyAssert{c.a}})
			if v.Pass != c.pass {
				t.Fatalf("Pass = %v, want %v (%+v)", v.Pass, c.pass, v)
			}
			if !c.pass && v.Plane != PlaneBody {
				t.Errorf("Plane = %q, want %q", v.Plane, PlaneBody)
			}
		})
	}
}

func TestJudge_ErrorExpected_AllChecksAreANDed(t *testing.T) {
	// VR12-E8 R1: every bullet must hold. One that passes must not carry one that does not.
	v := Judge(toolErr("object not found"), Expect{ErrorPlane: PlaneTool, Body: []BodyAssert{
		{Op: BodyContainsOp, Value: "object"},
		{Op: BodyContainsOp, Value: "zzz"},
	}})
	if v.Pass {
		t.Fatalf("two checks, one of them missing, PASSED: %+v", v)
	}
	if v.Plane != PlaneBody {
		t.Errorf("Plane = %q, want %q", v.Plane, PlaneBody)
	}
}

func TestJudge_ProtocolErrorExpected_BodyIsJudgedAgainstTheErrorObject(t *testing.T) {
	answer := CallResult{JSONRPCError: &RPCError{
		Code:    -32602,
		Message: "since_days must be an integer",
		Data:    json.RawMessage(`{"field":"since_days"}`),
	}}

	t.Run("every part of the object is reachable", func(t *testing.T) {
		for _, c := range []struct {
			name string
			a    BodyAssert
		}{
			{"message", BodyAssert{Field: "message", Op: BodyContainsOp, Value: "since_days"}},
			{"code", BodyAssert{Field: "code", Op: BodyContainsOp, Value: "-32602"}},
			{"a path into data", BodyAssert{Field: "data.field", Op: BodyContainsOp, Value: "since_days"}},
			{"the whole object", BodyAssert{Op: BodyContainsOp, Value: "-32602"}},
		} {
			t.Run(c.name, func(t *testing.T) {
				if v := Judge(answer, Expect{ErrorPlane: PlaneProtocol, Body: []BodyAssert{c.a}}); !v.Pass {
					t.Errorf("want Pass, got %+v", v)
				}
			})
		}
	})

	t.Run("a miss is the body plane, with its own reality-only observed", func(t *testing.T) {
		v := Judge(answer, Expect{ErrorPlane: PlaneProtocol,
			Body: []BodyAssert{{Field: "message", Op: BodyContainsOp, Value: "nope"}}})
		if v.Pass {
			t.Fatalf("want a failure, got %+v", v)
		}
		if v.Plane != PlaneBody {
			t.Errorf("Plane = %q, want %q", v.Plane, PlaneBody)
		}
		if v.Observed != BodyAssertObservedProtocolError {
			t.Errorf("Observed = %q, want BodyAssertObservedProtocolError", v.Observed)
		}
	})

	t.Run("a wrong CODE is reported before any body is read", func(t *testing.T) {
		v := Judge(answer, Expect{ErrorPlane: PlaneProtocol, ErrorCode: -32601,
			Body: []BodyAssert{{Field: "message", Op: BodyContainsOp, Value: "since_days"}}})
		if v.Pass {
			t.Fatalf("want a failure, got %+v", v)
		}
		if v.Plane != "protocol" {
			t.Errorf("Plane = %q, want %q — the plane decides first", v.Plane, "protocol")
		}
	})

	t.Run("the object is serialized as the SUT wrote it", func(t *testing.T) {
		// ⛔ Go's json encoder escapes <, > and & by default. With escaping left on, a message
		// containing `a<b & "c"` would be searched as `a<b & "c"` and an honest
		// `body contains a<b &` would miss — a false red on the SUT's own text.
		esc := CallResult{JSONRPCError: &RPCError{Code: -32602, Message: `a<b & "c"`}}
		if v := Judge(esc, Expect{ErrorPlane: PlaneProtocol,
			Body: []BodyAssert{{Op: BodyContainsOp, Value: `a<b &`}}}); !v.Pass {
			t.Errorf("HTML escaping is still on — the SUT's own characters were rewritten: %+v", v)
		}
		// The object IS JSON, so a quote inside the message appears escaped there. That is a real
		// property of `body contains …` on this plane, and the doc says so.
		if v := Judge(esc, Expect{ErrorPlane: PlaneProtocol,
			Body: []BodyAssert{{Op: BodyContainsOp, Value: `\"c\"`}}}); !v.Pass {
			t.Errorf("the JSON-escaped form should match the serialized object: %+v", v)
		}
		if v := Judge(esc, Expect{ErrorPlane: PlaneProtocol,
			Body: []BodyAssert{{Op: BodyContainsOp, Value: `"c"`}}}); v.Pass {
			t.Errorf("bare quotes should NOT match the serialized object: %+v", v)
		}
	})
}

// GUARD — the plane still decides first. An expected tool error with a body check, answered by a
// SUCCESS, is a plane failure and never reaches the body.
func TestJudge_ErrorPlaneMismatch_IsReportedBeforeTheBody(t *testing.T) {
	ok := CallResult{Content: []ContentBlock{{Type: "text", Text: "object not found"}}}
	v := Judge(ok, Expect{ErrorPlane: PlaneTool,
		Body: []BodyAssert{{Op: BodyContainsOp, Value: "object not found"}}})
	if v.Pass {
		t.Fatalf("a success answer satisfied an expected tool error: %+v", v)
	}
	if v.Plane != "tool" {
		t.Errorf("Plane = %q, want %q — the plane is the finding, not the body", v.Plane, "tool")
	}
}

// GUARD — both new observed strings are reality-only (VR-J12 / VR-C8). They reach the product hat.
func TestErrorPlaneBodyAssertObserved_IsRealityOnly(t *testing.T) {
	for _, c := range []struct{ name, s, must string }{
		{"tool", BodyAssertObservedToolError, "result.content"},
		{"protocol", BodyAssertObservedProtocolError, "error object"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.s == "" {
				t.Fatal("the constant is empty")
			}
			if !strings.Contains(c.s, c.must) {
				t.Errorf("%q does not name what was checked (%q)", c.s, c.must)
			}
			if strings.Contains(strings.ToLower(c.s), "expect") {
				t.Errorf("%q says what was EXPECTED — Observed is reality-only and reaches the product hat", c.s)
			}
		})
	}
}
