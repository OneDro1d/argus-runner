package chain

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/mcp"
)

// / — a NUMERIC claim whose threshold is one whole ${saved.<var>} has
// that variable validated as a finite number, so it is not a credential: its failed claim shows the
// field's real observed text, not the placeholder (the claim stays AS WRITTEN).

func TestFailedClaims_HTTP_NumericSavedClaim_EqualObservedShowsTheNumber(t *testing.T) {
	// The tester's shape: acks saved as 5, the field reads 5, `>` misses.
	srv := newSeqServer(t, map[string][]string{"/b": {`{"message_stats":{"ack":5}}`}})
	st := httpStepWith(srv.URL, 200, nil, mcp.BodyAssert{Field: "message_stats.ack", Op: mcp.BodyGTOp, Value: "${saved.acks}"}).
		Run("cid", map[string]string{"acks": "5"})
	got := failedClaimsOf(t, st)
	if len(got) != 1 || got[0].Claim != "field message_stats.ack > ${saved.acks}" {
		t.Fatalf("want one failed claim as written, got %+v", got)
	}
	if got[0].Observed != "5" {
		t.Errorf("want the real observed 5, got %q", got[0].Observed)
	}
}

func TestFailedClaims_HTTP_NumericSavedClaim_ShortSavedDoesNotGarbleLongerObserved(t *testing.T) {
	// Saved 5, the field reads 15, `<` misses: 15 must not become 1${saved.acks}.
	srv := newSeqServer(t, map[string][]string{"/b": {`{"count":15}`}})
	st := httpStepWith(srv.URL, 200, nil, mcp.BodyAssert{Field: "count", Op: mcp.BodyLTOp, Value: "${saved.acks}"}).
		Run("cid", map[string]string{"acks": "5"})
	got := failedClaimsOf(t, st)
	if len(got) != 1 || got[0].Observed != "15" {
		t.Fatalf("want observed 15 intact, got %+v", got)
	}
}

func TestFailedClaims_HTTP_NumericSavedClaim_LongEqualNumberShowsTheNumber(t *testing.T) {
	srv := newSeqServer(t, map[string][]string{"/b": {`{"count": ` + secretN + `}`}})
	st := httpStepWith(srv.URL, 200, nil, mcp.BodyAssert{Field: "count", Op: mcp.BodyGTOp, Value: "${saved.n}"}).
		Run("cid", map[string]string{"n": secretN})
	got := failedClaimsOf(t, st)
	if len(got) != 1 || got[0].Claim != "field count > ${saved.n}" || got[0].Observed != secretN {
		t.Fatalf("want the claim as written and observed %s, got %+v", secretN, got)
	}
}

func TestFailedClaims_HTTP_NonNumericSavedClaimStillShowsThePlaceholder(t *testing.T) {
	// Guard: a `contains ${saved.token}` claim whose observed echoes the token keeps today's redaction.
	const token = "tok-ABCDEF-123456"
	srv := newSeqServer(t, map[string][]string{"/b": {`{"echo":"` + token + `"}`}})
	st := httpStepWith(srv.URL, 200, nil, mcp.BodyAssert{Field: "echo", Op: mcp.BodyContainsOp, Value: "${saved.token}x"}).
		Run("cid", map[string]string{"token": token})
	got := failedClaimsOf(t, st)
	if len(got) != 1 {
		t.Fatalf("want one failed claim, got %+v", got)
	}
	b, _ := json.Marshal(st)
	if strings.Contains(string(b), token) || !strings.Contains(got[0].Observed, "${saved.token}") {
		t.Errorf("want the token as its placeholder, got %s", b)
	}
}

func TestFailedClaims_HTTP_NumericSavedClaim_OtherLongSavedValueStillRedacted(t *testing.T) {
	// Guard: only the claim's OWN saved variable is exempt; an unrelated long saved value echoed in a
	// numeric claim's observed text is still redacted.
	const token = "tok-ABCDEF-123456"
	srv := newSeqServer(t, map[string][]string{"/b": {`{"count":5,"echo":"` + token + `"}`}})
	st := httpStepWith(srv.URL, 200, nil, mcp.BodyAssert{Op: mcp.BodyContainsOp, Value: "zzz"},
		mcp.BodyAssert{Field: "count", Op: mcp.BodyGTOp, Value: "${saved.acks}"}).
		Run("cid", map[string]string{"acks": "5", "tok": token})
	got := failedClaimsOf(t, st)
	if len(got) != 2 {
		t.Fatalf("want two failed claims, got %+v", got)
	}
	b, _ := json.Marshal(st)
	if strings.Contains(string(b), token) {
		t.Errorf("an unrelated saved value is printed: %s", b)
	}
}
