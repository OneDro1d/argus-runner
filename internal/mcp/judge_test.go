package mcp

import (
	"strings"
	"testing"
)

// UC-23 / VR-J1: success plane — isError:false, no JSON-RPC error → pass.
func TestJudge_SuccessPlane(t *testing.T) {
	v := Judge(CallResult{IsError: false}, Expect{ErrorPlane: PlaneNone})
	if !v.Pass || v.Plane != "ok" {
		t.Fatalf("success should pass on the ok plane: %+v", v)
	}
}

// UC-24 / VR-J2: protocol plane — a top-level JSON-RPC error with the expected code.
func TestJudge_ProtocolPlane(t *testing.T) {
	v := Judge(CallResult{JSONRPCError: &RPCError{Code: -32601, Message: "method not found"}}, Expect{ErrorPlane: PlaneProtocol, ErrorCode: -32601})
	if !v.Pass || v.Plane != "protocol" {
		t.Fatalf("protocol error should pass on the protocol plane: %+v", v)
	}
}

func TestJudge_ProtocolPlane_WrongCode(t *testing.T) {
	v := Judge(CallResult{JSONRPCError: &RPCError{Code: -32602}}, Expect{ErrorPlane: PlaneProtocol, ErrorCode: -32601})
	if v.Pass {
		t.Fatalf("a -32602 must not satisfy an expected -32601: %+v", v)
	}
}

// UC-25 / VR-J3: tool plane — result.isError:true (no top-level error).
func TestJudge_ToolPlane(t *testing.T) {
	v := Judge(CallResult{IsError: true}, Expect{ErrorPlane: PlaneTool})
	if !v.Pass || v.Plane != "tool" {
		t.Fatalf("isError:true should pass on the tool plane: %+v", v)
	}
}

// A tool failure when SUCCESS was expected → fail (the planes bind pass/fail).
func TestJudge_SuccessExpected_ButToolError(t *testing.T) {
	v := Judge(CallResult{IsError: true}, Expect{ErrorPlane: PlaneNone})
	if v.Pass {
		t.Fatalf("isError:true must fail a success-expected scenario: %+v", v)
	}
}

// UC-30 / VR-J8: unreachable is reported DISTINCTLY (gate-infra), not a tool failure.
func TestJudge_Unreachable(t *testing.T) {
	v := Judge(CallResult{Unreachable: true}, Expect{ErrorPlane: PlaneNone})
	if v.Pass || v.Plane != "unreachable" {
		t.Fatalf("unreachable must be a distinct non-pass plane: %+v", v)
	}
}

// UC-29 / VR-J7: a transport/handshake failure is a distinct requirements failure.
func TestJudge_TransportFailure(t *testing.T) {
	v := Judge(CallResult{TransportErr: "declared streamable-http but server is legacy-sse"}, Expect{ErrorPlane: PlaneNone})
	if v.Pass || v.Plane != "transport" {
		t.Fatalf("transport mismatch must be a distinct non-pass plane: %+v", v)
	}
}

// UC-26 / VR-J4: a content-buried error on a server that reports success →
// the binding verdict still follows the planes (pass), but the soft-warning FIRES.
func TestJudge_SoftWarning_ContentBuriedError(t *testing.T) {
	v := Judge(CallResult{IsError: false, Content: []ContentBlock{{Type: "text", Text: `{"error_code":"BAD","message":"upstream rejected"}`}}}, Expect{ErrorPlane: PlaneNone})
	if !v.Pass {
		t.Fatalf("the binding verdict must follow the planes (success), got fail: %+v", v)
	}
	if v.SoftWarning == "" {
		t.Fatalf("a content-buried error must raise the advisory soft-warning: %+v", v)
	}
}

// VR-J12 / VR-C8: Observed is reality-only — it never says "expected" / echoes the expectation.
func TestJudge_ObservedIsRealityOnly(t *testing.T) {
	v := Judge(CallResult{IsError: false}, Expect{ErrorPlane: PlaneTool}) // expected tool-error, got success
	if v.Pass {
		t.Fatalf("precondition: this should be a failure")
	}
	if strings.Contains(strings.ToLower(v.Observed), "expect") {
		t.Fatalf("Observed must describe responder reality only, never the expectation: %q", v.Observed)
	}
	if !strings.Contains(v.Observed, "isError:false") {
		t.Fatalf("Observed should name the actual responder behaviour: %q", v.Observed)
	}
}
