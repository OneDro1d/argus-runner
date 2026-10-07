package scenario

import (
	"strings"
	"testing"
)

// bodyassert_numeric_test.go — item 25(b): `body has <field> >|>=|<|<= <number>`, parsed by the
// SAME funnel every other body-assertion form goes through (ParseBodyAsserts), never a second
// parser.

func TestParseBodyAsserts_NumericFormsParse(t *testing.T) {
	cases := []struct {
		bullet string
		op     string
		val    string
	}{
		{"- body has latency_ms > 100", BodyGT, "100"},
		{"- body has latency_ms >= 100", BodyGTE, "100"},
		{"- body has latency_ms < 100", BodyLT, "100"},
		{"- body has latency_ms <= 100", BodyLTE, "100"},
		{"- body has price.usd > 3.14", BodyGT, "3.14"},
		{"- Body Has count >= -1", BodyGTE, "-1"},
	}
	for _, c := range cases {
		if !ClaimsToBeBodyAssert(c.bullet) {
			t.Errorf("ClaimsToBeBodyAssert(%q) = false, want true", c.bullet)
		}
		got, errs := ParseBodyAsserts([]string{c.bullet})
		if len(errs) > 0 {
			t.Fatalf("ParseBodyAsserts(%q) errored: %v", c.bullet, errs[0])
		}
		if len(got) != 1 {
			t.Fatalf("ParseBodyAsserts(%q) produced %d assertions, want 1", c.bullet, len(got))
		}
		if got[0].Op != c.op || got[0].Value != c.val || got[0].Field == "" {
			t.Errorf("ParseBodyAsserts(%q) = %+v, want op=%s value=%s (field-scoped)", c.bullet, got[0], c.op, c.val)
		}
	}
}

// R3, item 25: a malformed numeric form is refused BY NAME — a bad operator and a non-numeric
// threshold each get a SPECIFIC reason, not the generic "matches no known form" fallback.
func TestParseBodyAsserts_NumericFormsRefusedByName(t *testing.T) {
	_, errs := ParseBodyAsserts([]string{"- body has latency_ms => 100"})
	if len(errs) != 1 {
		t.Fatalf("a bad operator must be refused: %v", errs)
	}
	if !containsAll(errs[0].Error(), "operator", `"=>"`) {
		t.Errorf("error must name the bad operator: %v", errs[0])
	}

	_, errs = ParseBodyAsserts([]string{"- body has latency_ms > fast"})
	if len(errs) != 1 {
		t.Fatalf("a non-numeric threshold must be refused: %v", errs)
	}
	if !containsAll(errs[0].Error(), "not a number") {
		t.Errorf("error must say the threshold is not a number: %v", errs[0])
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}
