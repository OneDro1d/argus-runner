package toolcore

import (
	"strings"
	"testing"
)

// V31-004 fix 2/h (VR13-BF) · TS-BF-3 — THE SCAFFOLD TEACHES THE FORM THE PRODUCT EXECUTES.
//
// The scaffolder is the first thing an author sees, so whatever it writes is what the estate fills
// with. Its status-layer text offered `body has <field> containing <x>` and nothing else — correct
// grammar, but it offers only the FIELD-SCOPED form, and the thing most authors actually want
// ("does this string appear anywhere in the answer?") is `body contains <value>`, which the scaffold
// never mentioned. That omission is half of why the estate learned `body has text containing …`:
// with no whole-answer form on offer, `text` was the field name authors invented for "the answer".
//
// So the skeleton must name BOTH forms and say which is which.
func TestScaffold_TeachesBothBodyFormsAndNeverBodyHasText(t *testing.T) {
	scaffold := func(t *testing.T, layer string) string {
		t.Helper()
		p, err := ProposeScenario("a scenario for the body-form guard", "order-api", layer)
		if err != nil {
			t.Fatalf("ProposeScenario(%q): %v", layer, err)
		}
		drafts, _ := p.(map[string]any)["drafts"].([]map[string]any)
		if len(drafts) == 0 {
			t.Fatalf("ProposeScenario(%q) returned no draft: %+v", layer, p)
		}
		md, _ := drafts[0]["scenario"].(string)
		notes, _ := drafts[0]["assumptions"].([]string)
		// The author reads BOTH: the markdown they will edit and the notes beside it.
		return md + "\n<<NOTES>>\n" + strings.Join(notes, "\n")
	}

	// The status layers are the ones whose scaffold discusses body assertions at all.
	for _, layer := range []string{"HTTP Ingestion", "Error Path", "Rate Limiting", "Permissions"} {
		t.Run(layer, func(t *testing.T) {
			all := scaffold(t, layer)

			if !strings.Contains(all, "body contains <x>") {
				t.Errorf("the %s scaffold never offers the WHOLE-ANSWER form `body contains <x>`:\n%s", layer, all)
			}
			if !strings.Contains(all, "body has <field>") {
				t.Errorf("the %s scaffold no longer offers the FIELD-SCOPED form `body has <field> …`:\n%s", layer, all)
			}
			// ⛔ the form this row exists to remove, in any of its shapes
			for _, bad := range []string{"body has text containing", "body has text matching", "body has text "} {
				if strings.Contains(all, bad) {
					t.Errorf("the %s scaffold still teaches %q — since V29-017 that asks for a JSON field NAMED `text`:\n%s", layer, bad, all)
				}
			}
		})
	}

	t.Run("and the content layers are untouched", func(t *testing.T) {
		// They have their own grammar (row_count / NAME == VALUE) and never discussed body asserts.
		for _, layer := range []string{"Database State", "Message Flow", "External Delivery"} {
			if all := scaffold(t, layer); strings.Contains(all, "body has text") {
				t.Errorf("the %s scaffold teaches `body has text`:\n%s", layer, all)
			}
		}
	})
}
