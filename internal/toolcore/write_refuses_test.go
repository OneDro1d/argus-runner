package toolcore

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// VR12-E8 R3 / THE ONE RULE — WRITE MUST REFUSE EVERYTHING VALIDATE REFUSES.
//
// The trap this guards is not hypothetical, it was the shipped behaviour: author__validate_scenario
// went through scenario.Validate + argus.ExpectProblems, while author__write_scenario and the
// control plane's write called scenario.Validate ALONE. Every rule that lived in ExpectProblems was
// therefore REPORTED by validate and SILENTLY BYPASSED by both writers — an author was told the
// scenario was invalid and stored it anyway.
//
// The test asserts the property directly (same corpus, both entry points) rather than asserting that
// one function calls another, because the property is what an author actually experiences.
func TestWriteRefusesEverythingValidateRefuses(t *testing.T) {
	for _, c := range invalidCorpus() {
		t.Run(c.name, func(t *testing.T) {
			// 1. validate must call it invalid …
			res, hasErrs, err := ValidateScenario(c.body)
			if err != nil {
				t.Fatalf("ValidateScenario returned a transport error: %v", err)
			}
			if !hasErrs {
				t.Fatalf("the corpus entry is not actually invalid — ValidateScenario accepted it, so this "+
					"case proves nothing:\n%s", c.body)
			}
			m := res.(map[string]any)
			if m["valid"] != false {
				t.Fatalf(`ValidateScenario reported valid:true for an invalid scenario`)
			}

			// 2. … and write must refuse it, AND leave nothing on disk.
			dir := t.TempDir()
			wres, wErrs, err := WriteScenario(dir, []byte(c.body), "http/guard.md")
			if err != nil {
				t.Fatalf("WriteScenario returned a transport error: %v", err)
			}
			wm := wres.(map[string]any)
			if wm["written"] != false || !wErrs {
				t.Fatalf("author__write_scenario ACCEPTED a scenario author__validate_scenario refuses (%s) — "+
					"this is the exact bypass VR12-E8 R3 closes:\n%s", c.name, c.body)
			}
			if _, statErr := os.Stat(filepath.Join(dir, "http", "guard.md")); statErr == nil {
				t.Fatalf("a refused write still left the file on disk")
			}
		})
	}
}

// TestValidCorpusIsAccepted is the positive control: without it the test above would pass against a
// WriteScenario that refuses EVERYTHING, which refuses the invalid cases for the wrong reason.
func TestValidCorpusIsAccepted(t *testing.T) {
	body := guardScenario()
	if _, hasErrs, err := ValidateScenario(body); err != nil || hasErrs {
		res, _, _ := ValidateScenario(body)
		t.Fatalf("the control scenario must be VALID, else every refusal above is meaningless: %v / %+v", err, res)
	}
	dir := t.TempDir()
	res, hasErrs, err := WriteScenario(dir, []byte(body), "http/ok.md")
	if err != nil || hasErrs {
		t.Fatalf("WriteScenario refused the valid control scenario: %v / %+v", err, res)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "http", "ok.md")); statErr != nil {
		t.Fatalf("an accepted write did not land on disk: %v", statErr)
	}
}

// insideValidateAll reports whether line index i sits inside func ValidateAll.
func insideValidateAll(src string, i int) bool {
	lines := strings.Split(src, "\n")
	for j := i; j >= 0; j-- {
		if strings.HasPrefix(lines[j], "func ") {
			return strings.HasPrefix(lines[j], "func ValidateAll(")
		}
	}
	return false
}

type corpusCase struct {
	name string
	body string
}

// guardScenario is the control — every invalid case below is this text with ONE thing broken, so a
// refusal can only be caused by that one thing.
func guardScenario() string {
	return strings.Join([]string{
		"# Scenario: guard",
		"",
		"## Metadata",
		"- **ID**: GRD-001",
		"- **Layer**: HTTP Ingestion",
		"- **Tags**: http",
		"",
		"## TRIGGER",
		"POST `${INGESTION_URL}/api/v1/orders`",
		"```json",
		`{"correlation_id": "${correlation_id}"}`,
		"```",
		"",
		"## EXPECT",
		"### Runnable",
		"- status=202",
		"- body has order_id containing 01",
		"",
		"## TIMEOUT",
		"30s",
		"",
		"## CLEANUP", "N/A — a unit-test fixture; it creates nothing.", "",
	}, "\n")
}

func invalidCorpus() []corpusCase {
	v := guardScenario()
	return []corpusCase{
		{
			// V31-002 W1 — the removed step `expect` key, refused BY NAME. Both writers must refuse
			// it, or an old-format chain saves and then has its author's claims silently ignored.
			name: "step-expect-key-removed",
			body: strings.Join([]string{
				"# Scenario: c", "",
				"## Metadata", "- **ID**: CHN-W1", "- **Layer**: Permissions", "- **Tags**: chain", "",
				"## TRIGGER", "POST `chain`", "",
				"```json",
				`{"steps":[{"type":"mcp","name":"create","tool":"t","args":{},"expect":"result.isError == false"}]}`,
				"```", "",
				"## EXPECT", "### Runnable", "- step create: result.isError == false", "",
				"### Non-runnable", "- a fixture for the corpus", "",
				"## TIMEOUT", "60s", "",
				"## CLEANUP", "N/A — a unit-test fixture; it creates nothing.", "",
			}, "\n"),
		},
		{
			// V31-002 W2 — a scenario that asserts nothing. It proves nothing, so it may not be saved.
			name: "no-runnable-check",
			body: strings.Replace(v, "### Runnable", "### Non-runnable", 1),
		},
		{
			// R3 — the rule this round adds. It parses as no known body form.
			name: "body-assertion-that-parses-as-nothing",
			body: strings.Replace(v, "- body has order_id containing 01", "- body has order_id containing", 1),
		},
		{
			name: "matching-regex-that-does-not-compile",
			body: strings.Replace(v, "- body has order_id containing 01", "- body has order_id matching ([unclosed", 1),
		},
		{
			// VR12-E1: a bullet directly under ## EXPECT cannot be told from prose.
			name: "expect-not-subheaded",
			body: strings.Replace(v, "### Runnable\n", "", 1),
		},
		{
			// VR12-E2: an undefined sub-section.
			name: "unknown-expect-subsection",
			body: strings.Replace(v, "### Runnable", "### Maybe", 1),
		},
		{
			// VR12-E6 (V29-016): a scenario judged by response code that declares no status.
			// ⚠ The `- status=202` line is REMOVED, not replaced by prose: an unanchored mention is
			// not a declaration, which is the whole point of VR12-E7.
			name: "no-declared-status-on-a-code-judged-scenario",
			body: strings.Replace(v, "- status=202\n", "", 1),
		},
		{
			// The rule that lived ONLY in argus.ExpectProblems — the one the writers bypassed.
			//
			// ⚠ NOT `result.isError must be roughly false`: the plane table matches by SUBSTRING, so a
			// bullet containing both "iserror" and "false" reads as the canonical
			// `result.isError == false` no matter what sits between them. The case has to be a bullet
			// that is assertion-SHAPED and matches no plane phrase at all.
			name: "mcp-bullet-not-understood",
			body: strings.Replace(
				strings.Replace(v, "- **Tags**: http", "- **Tags**: mcp", 1),
				"- body has order_id containing 01", "- error_code must be 42", 1),
		},
	}
}

// ⭐ THE PRODUCT MUST NOT GENERATE A DRAFT ITS OWN VALIDATOR REFUSES.
//
// Found by this build: author__propose_scenario emitted a skeleton that author__validate_scenario
// rejected — the first thing an author does with the tool produced an error, from the tool itself.
// It is the cheapest possible guard and nothing had it.
//
// ⚠ It asserts VALIDITY, not usefulness: the scaffold is a skeleton by design (DF-16) and its
// placeholder values still have to be replaced with real ones. What it must never be is INVALID.
func TestEveryProposedSkeletonPassesTheProductsOwnValidator(t *testing.T) {
	for _, layer := range []string{
		"HTTP Ingestion", "Error Path", "Rate Limiting", "Permissions",
		"Database State", "Message Flow", "External Delivery",
	} {
		t.Run(layer, func(t *testing.T) {
			p, err := ProposeScenario("a scenario for the guard test", "order-api", layer)
			if err != nil {
				t.Fatalf("ProposeScenario: %v", err)
			}
			drafts, _ := p.(map[string]any)["drafts"].([]map[string]any)
			if len(drafts) == 0 {
				t.Fatalf("ProposeScenario returned no draft: %+v", p)
			}
			md, _ := drafts[0]["scenario"].(string)
			if strings.TrimSpace(md) == "" {
				t.Fatalf("the draft carries no scenario markdown: %+v", drafts[0])
			}
			if _, verrs := ValidateAll(md); len(verrs) > 0 {
				t.Errorf("the scaffold for %q is REFUSED by the product's own validator:\n%v\n\n%s", layer, verrs, md)
			}
		})
	}
}
