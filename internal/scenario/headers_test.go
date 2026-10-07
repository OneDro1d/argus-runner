package scenario

import (
	"strings"
	"testing"
)

// VR12-T3.a / T3.b (V29-018) — the two header names an author may NOT set are REFUSED BY NAME.
//
// Silently ignoring an author's header is the defect this row is about. Doing it deliberately for
// two names, without saying so, would be the same defect wearing a justification.
func headerMD(headers ...string) string {
	lines := []string{
		"# Scenario: h", "",
		"## Metadata",
		"- **ID**: HDR-001",
		"- **Layer**: HTTP Ingestion",
		"- **Tags**: http, order", "",
		"## TRIGGER",
		"POST `${INGESTION_URL}/api/v1/orders`",
	}
	lines = append(lines, headers...)
	lines = append(lines,
		"", "```json", `{"x":1}`, "```", "",
		"## EXPECT", "### Runnable", "- status=202", "",
		"## TIMEOUT", "30s", "",
		"## CLEANUP", "N/A — a unit-test fixture; it creates nothing.", "")
	return strings.Join(lines, "\n")
}

func TestVR12T3_ReservedHeadersAreRefusedByName(t *testing.T) {
	for _, h := range []string{"X-Correlation-Id: tr-mine", "Mcp-Session-Id: s1"} {
		name := strings.SplitN(h, ":", 2)[0]
		_, errs := Validate(headerMD(h))
		if !find(errs, name) {
			t.Errorf("%q must be refused BY NAME, not silently ignored; got %v", name, errs)
		}
	}
	// …and an ordinary header is fine.
	if _, errs := Validate(headerMD("Accept: application/json")); find(errs, "may not set") {
		t.Errorf("an ordinary header must be accepted; got %v", errs)
	}
}

// AuthorHeaders is STABLE and excludes the two mechanisms that have their own path.
func TestAuthorHeaders_StableAndExcludesAuthorization(t *testing.T) {
	s := Parse(headerMD("X-Tenant: acme", "Accept: application/json", "Authorization: Bearer x"))
	got := AuthorHeaders(s)
	if len(got) != 2 || got[0][0] != "Accept" || got[1][0] != "X-Tenant" {
		t.Fatalf("want [Accept X-Tenant] sorted, Authorization excluded; got %v", got)
	}
	// HeaderNames is what the author tools report — names only, Authorization included, sorted.
	names := HeaderNames(s)
	if strings.Join(names, ",") != "Accept,Authorization,X-Tenant" {
		t.Errorf("HeaderNames = %v", names)
	}
	for _, n := range names {
		if strings.Contains(n, "Bearer") || strings.Contains(n, "acme") {
			t.Fatalf("HeaderNames leaked a VALUE: %q — a header value is the likeliest place in a "+
				"scenario to hold a credential", n)
		}
	}
}
