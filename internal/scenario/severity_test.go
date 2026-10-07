package scenario

import (
	"strings"
	"testing"
)

// VR12-E4 / VR12-E5 — ENFORCED OR REPORTED, NEVER SILENT.

func TestVR12E4_UnexecutableRunnableBulletIsAnError(t *testing.T) {
	cases := []struct {
		name, bullet string
		refused      bool
	}{
		// Refused: assertion-shaped, and no grammar takes it.
		//
		// ⚠ NOT REFUSED, and deliberately: `- order_id ~~ 01ABC`. `~~` is not in the operator set,
		// so the bullet is not assertion-SHAPED and stays prose. That is the owner's lock (register
		// 1515-1518: ONLY assertion-shaped bullets are policed) and the boundary is worth a case of
		// its own — see the "an unknown operator stays prose" entry below.
		{"an assertion nobody implemented", "- response_time must be under 200ms", true},
		{"a body assertion with no value", "- body has order_id containing", true},
		// Accepted: a grammar takes it.
		{"a declared status", "- status=202", false},
		{"a body assertion", "- body has order_id containing 01", false},
		{"a column assertion", "- status == pending", false},
		{"a row count", "- row_count == 1", false},
		{"the negative form", "- no rows", false},
		// Accepted: PROSE is never policed — the owner's lock.
		{"plain prose", "- the order is accepted and the customer is notified", false},
		{"prose with a number", "- the queue drains within a few seconds", false},
		{"an unknown operator stays prose (the owner's lock)", "- order_id ~~ 01ABC", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			md := fullMD(func(m string) string {
				return strings.Replace(m, "- status=202", "- status=202\n"+c.bullet, 1)
			})
			_, errs := Validate(md)
			got := find(errs, "no grammar in this product can execute it")
			if got != c.refused {
				t.Fatalf("bullet %q: refused=%v, want %v (%v)", c.bullet, got, c.refused, errs)
			}
		})
	}
}

// E5 is a WARNING, and the write SUCCEEDS. The distinction is the owner's: such a bullet IS
// understood — it is merely in the wrong place, and refusing it would stop an author recording
// something true because they put it one heading too low.
func TestVR12E5_AssertionUnderNonRunnableWarnsOnly(t *testing.T) {
	md := fullMD(func(m string) string {
		return strings.Replace(m, "- status=202\n",
			"- status=202\n\n### Non-runnable\n- order_id == 01ABC\n", 1)
	})
	if _, errs := Validate(md); len(errs) > 0 {
		t.Fatalf("E5 must not make the scenario invalid: %v", errs)
	}
	var found bool
	for _, w := range Warnings(md) {
		if strings.Contains(w, "under `### Non-runnable` but looks like an assertion") {
			found = true
		}
	}
	if !found {
		t.Fatalf("an assertion under ### Non-runnable must WARN; got %v", Warnings(md))
	}
}

// ⛔ HOUSEKEEPING 2 (V29-020): the product used to TELL authors that an unexecutable EXPECT bullet
// could stay in EXPECT as prose — "… or treat it as documentation". That string is part of the fix.
func TestVR12E4_NoWarningStillSaysTreatItAsDocumentation(t *testing.T) {
	md := fullMD(func(m string) string {
		m = strings.Replace(m, "- **Layer**: HTTP Ingestion", "- **Layer**: Database State", 1)
		m = strings.Replace(m, "N/A — a status layer judges by response code.",
			"```sql\nSELECT created_at FROM orders WHERE correlation_id = '${correlation_id}'\n```", 1)
		return strings.Replace(m, "- status=202", "- created_at within ±2s of now", 1)
	})
	for _, w := range Warnings(md) {
		if strings.Contains(w, "treat it as documentation") {
			t.Errorf("the warning still tells authors to leave an unexecutable bullet in EXPECT as "+
				"prose — it must point at `### Non-runnable`: %q", w)
		}
	}
}

// HOUSEKEEPING 1 (V29-020): Warnings was WIDENED, not built. It used to inspect only Database
// State / Message Flow / Web UI, which is exactly why an http-ingestion scenario measured
// `warnings: null` — the authoring gate the contract promises was silent for the commonest layer.
func TestVR12E5_WarningsReachAnHTTPScenario(t *testing.T) {
	md := fullMD(func(m string) string {
		return strings.Replace(m, "- status=202\n",
			"- status=202\n\n### Non-runnable\n- tenant_id == t-default\n", 1)
	})
	if len(Warnings(md)) == 0 {
		t.Fatal("an http-ingestion scenario must be able to produce a warning — `warnings: null` for " +
			"the commonest layer in the estate is what V29-020 measured")
	}
}
