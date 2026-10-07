package mcpserver

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestReqToken_S7 — S7 (CP-M3-III-5): an author PAT (odts_) presented via ?token= is REJECTED (authors must
// use the Authorization header, which leaks nothing to gateway/proxy access logs), while the static-gateway
// class (non-odts_) is STILL accepted via the query form (UC026, owner's Q1 ruling). The Authorization header
// always wins over the query param.
func TestReqToken_S7(t *testing.T) {
	mk := func(hdr, q string) *http.Request {
		r := httptest.NewRequest("POST", "/mcp?token="+q, nil)
		if hdr != "" {
			r.Header.Set("Authorization", hdr)
		}
		return r
	}
	if got := reqToken(mk("Bearer odts_abc", "static_x")); got != "odts_abc" {
		t.Fatalf("Authorization header should win: got %q", got)
	}
	if got := reqToken(mk("", "odts_secret")); got != "" {
		t.Fatalf("an odts_ PAT via ?token= must be rejected (header-only), got %q", got)
	}
	if got := reqToken(mk("", "smcp_static")); got != "smcp_static" {
		t.Fatalf("a static (non-odts_) token via ?token= must still pass (UC026), got %q", got)
	}
}
