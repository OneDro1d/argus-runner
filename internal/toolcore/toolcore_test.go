package toolcore

import (
	"encoding/json"
	"strings"
	"testing"
)

// VR-AUTH1: a proposed draft is argus-valid AND flags its guessed fields.
func TestProposeScenario_DraftIsValidAndFlagged(t *testing.T) {
	p, err := ProposeScenario("verify a duplicate order returns 409", "order-api", "")
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	b, _ := json.Marshal(p)
	var got struct {
		Drafts []struct {
			Scenario        string   `json:"scenario"`
			NeedsUserReview []string `json:"needs_user_review"`
		} `json:"drafts"`
	}
	if err := json.Unmarshal(b, &got); err != nil || len(got.Drafts) == 0 {
		t.Fatalf("no draft returned: %v (%s)", err, b)
	}
	if len(got.Drafts[0].NeedsUserReview) == 0 {
		t.Error("guessed fields must be flagged in needs_user_review[] (VR-AUTH1)")
	}
	if _, invalid, _ := ValidateScenario(got.Drafts[0].Scenario); invalid {
		t.Fatalf("proposed draft is not argus-valid:\n%s", got.Drafts[0].Scenario)
	}
}

// VR-AUTH2/AUTH3 / DF-DEC-M25-03: an uncoverable EXPECT is valid:true + a warning (not invalid).
func TestValidateScenario_UncoverableWarnsNotFails(t *testing.T) {
	md := strings.Join([]string{
		"# Scenario: DB range", "", "## Metadata", "- **ID**: DB-099", "- **Layer**: Database State", "- **Tags**: http", "",
		"## TRIGGER", "POST `${INGESTION_URL}/api/v1/orders`", "",
		"## VERIFY", "```sql", "SELECT created_at FROM orders WHERE correlation_id = '${correlation_id}'", "```", "", // RO-09: a content VERIFY must be executable
		"## EXPECT", "### Runnable", "- created_at within ±2s of now", "",
		"## TIMEOUT", "30s", "",
		"## CLEANUP", "N/A — a unit-test fixture; it creates nothing.", "",
	}, "\n")
	p, invalid, _ := ValidateScenario(md)
	if invalid {
		t.Fatalf("uncoverable EXPECT must be valid:true + warning, got invalid: %+v", p)
	}
	b, _ := json.Marshal(p)
	if strings.Contains(string(b), `"warnings":[]`) || strings.Contains(string(b), `"warnings":null`) {
		t.Fatalf("uncoverable EXPECT must produce a warning: %s", b)
	}
}

// VR-AUTH6 / DF-DEC-M25-04: deleting an absent id is a success no-op, not an error.
func TestDeleteScenario_IdempotentOnAbsent(t *testing.T) {
	e := Env{ScenariosDir: t.TempDir()}
	p, err := DeleteScenario(e, "NOPE-001")
	if err != nil {
		t.Fatalf("delete absent id must be a no-op, got err: %v", err)
	}
	if b, _ := json.Marshal(p); !strings.Contains(string(b), `"deleted":false`) {
		t.Fatalf("absent delete should report deleted:false: %s", b)
	}
}
