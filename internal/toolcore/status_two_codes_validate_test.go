package toolcore

import (
	"encoding/json"
	"strings"
	"testing"
)

// Author_validate_scenario (ValidateScenario, which author_write_scenario also calls via
// ValidateAll) REFUSES a status claim that lists two codes. It used to answer valid:true and the runner
// then judged `status=200/201` as `status=200`, dropping the second code with no word.
func httpStatusScenario(status string) string {
	return strings.Join([]string{
		"# Scenario: two codes", "",
		"## Metadata",
		"- **ID**: TWO-001",
		"- **Layer**: HTTP Ingestion",
		"- **Tags**: http, order", "",
		"## TRIGGER",
		"GET `${INGESTION_URL}/health`", "",
		"## EXPECT",
		"### Runnable",
		"- " + status, "",
		"## TIMEOUT", "30s", "",
		"## CLEANUP", "N/A — a unit-test fixture; it creates nothing.", "",
	}, "\n")
}

func TestValidateScenario_RefusesAStatusClaimListingTwoCodes(t *testing.T) {
	for _, claim := range []string{"status=200/201", "status=200,201", "status=200|201", "status=200 or 201"} {
		p, invalid, err := ValidateScenario(httpStatusScenario(claim))
		if err != nil {
			t.Fatalf("%q: %v", claim, err)
		}
		b, _ := json.Marshal(p)
		if !invalid || !strings.Contains(string(b), `"valid":false`) {
			t.Errorf("%q: author_validate_scenario must answer invalid; got invalid=%v %s", claim, invalid, b)
			continue
		}
		if !strings.Contains(string(b), "lists two codes") || !strings.Contains(string(b), "exactly one code") {
			t.Errorf("%q: the answer must name the problem; got %s", claim, b)
		}
	}
	// the ordinary form is still valid
	if p, invalid, err := ValidateScenario(httpStatusScenario("status=201 and the body has an id")); err != nil || invalid {
		t.Errorf("an ordinary claim must stay valid: invalid=%v err=%v %+v", invalid, err, p)
	}
}
