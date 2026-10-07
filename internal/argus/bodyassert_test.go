package argus

import (
	"strings"
	"testing"
)

// VR-C8 regression (blind-gate finding 2026-06-22): the body-assertion failure `observed`
// (product-hat-visible) must be REALITY-ONLY — it must NOT echo the test's expected value.
// The earlier message ("…does not contain '<expected>'") leaked the EXPECT substring to the
// product hat via observed. The reality-only constant must carry no echoed value + no
// "contains '"/"matches /" template that would interpolate one.
func TestBodyAssertObserved_IsRealityOnly(t *testing.T) {
	if strings.TrimSpace(bodyAssertObserved) == "" {
		t.Fatal("bodyAssertObserved must be a non-empty reality-only message")
	}
	for _, leaky := range []string{"contains '", "match /", "containing", "= '", "expected value is"} {
		if strings.Contains(bodyAssertObserved, leaky) {
			t.Fatalf("bodyAssertObserved must not echo the expected value (found %q): %q", leaky, bodyAssertObserved)
		}
	}
	if !strings.Contains(strings.ToLower(bodyAssertObserved), "body") {
		t.Fatalf("bodyAssertObserved should still describe the reality (the body did not satisfy the assertion): %q", bodyAssertObserved)
	}
}

// VR12-E8 (V29-017) — THE GRAMMAR. This replaces TestExtractBodyAsserts, which asserted the
// three defects rather than the contract:
//
//	{"containing-quoted", []string{"status=400", `body has error containing "items"`}, "items", ...}
//
// It expected the FIELD NAME to be thrown away ("items" with no `error`), and it had no case at
// all for two body bullets or for the `body contains` spelling — the two shapes that were silently
// doing nothing in the shipped catalogue.
func TestParseBodyAsserts(t *testing.T) {
	cases := []struct {
		name    string
		bullets []string
		want    []BodyAssert
		wantErr bool
	}{
		{
			name:    "field-scoped containing keeps the FIELD, which is the whole point",
			bullets: []string{"status=400", `body has error containing "items"`},
			want:    []BodyAssert{{Field: "error", Op: BodyContains, Value: "items"}},
		},
		{
			name: "⭐ EVERY bullet is wired — the first-wins defect (SYN-MCP-002)",
			bullets: []string{
				"body has serverInfo containing acme-atlassian-mcp-server",
				"body has protocolVersion containing 2024-11-05",
			},
			want: []BodyAssert{
				{Field: "serverInfo", Op: BodyContains, Value: "acme-atlassian-mcp-server"},
				{Field: "protocolVersion", Op: BodyContains, Value: "2024-11-05"},
			},
		},
		{
			name:    "⭐ the field-less form parses at all — it used to set NOTHING (SYN-MCP-001)",
			bullets: []string{"status=401", "body contains `\"code\":\"missing_token\"`"},
			want:    []BodyAssert{{Op: BodyContains, Value: `"code":"missing_token"`}},
		},
		{
			name:    "matching keeps the field and the regex",
			bullets: []string{`body has error matching qty.*maximum`},
			want:    []BodyAssert{{Field: "error", Op: BodyMatches, Value: "qty.*maximum"}},
		},
		{
			name:    "bare `body has <field>` means the field EXISTS",
			bullets: []string{"body has order_id"},
			want:    []BodyAssert{{Field: "order_id", Op: BodyExists}},
		},
		{
			name:    "a dotted path needs no grammar change (R4)",
			bullets: []string{"body has data.token containing eyJ"},
			want:    []BodyAssert{{Field: "data.token", Op: BodyContains, Value: "eyJ"}},
		},
		{
			name:    "a bullet that is not about the body is left alone",
			bullets: []string{"status=202", "row_count == 1"},
			want:    nil,
		},
		{
			name:    "⛔ R3: a bullet CLAIMING to be a body assertion and parsing as none is an ERROR",
			bullets: []string{"body should probably mention the order id"},
			wantErr: true,
		},
		{
			name:    "⛔ a `matching` regex that does not compile is an ERROR, never a silent skip",
			bullets: []string{"body has error matching qty(.*maximum"},
			wantErr: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, errs := ParseBodyAsserts(c.bullets)
			if c.wantErr {
				if len(errs) == 0 {
					t.Fatalf("want an error, got asserts %+v", got)
				}
				return
			}
			if len(errs) != 0 {
				t.Fatalf("unexpected errors: %v", errs)
			}
			if len(got) != len(c.want) {
				t.Fatalf("got %d asserts, want %d:\n  got:  %+v\n  want: %+v", len(got), len(c.want), got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Errorf("assert %d:\n  got:  %+v\n  want: %+v", i, got[i], c.want[i])
				}
			}
			if hasBodyAssert(c.bullets) != (len(c.want) > 0) {
				t.Errorf("hasBodyAssert disagrees with the parser — the BODY-ASSERT-FAIL verdict overwrite would fire for an assertion nobody enforced")
			}
		})
	}
}

// TestUnquoteKeepsInnerQuotes moved to internal/scenario with the grammar itself (VR12-E8 R5).
