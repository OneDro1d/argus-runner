package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// moneywrites_test.go — "money writes" (T5.4 follow-up, 2026-09-26), item 1: the money_writes block
// is valid ONLY beside money_handling: true, and every entry is checked in full at load time.

func writeMoneyWritesConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "argus-config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestMoneyWrites_RefusedWithoutMoneyHandling(t *testing.T) {
	p := writeMoneyWritesConfig(t, "project:\n  name: p\n"+
		"money_writes:\n  allow:\n    - method: POST\n      path: /api/v1/trading/quote\n      spends: false\n"+
		"targets:\n  http:\n    base_url: http://sut.invalid\n")
	_, err := Load(p)
	if err == nil {
		t.Fatal("money_writes without money_handling: true was accepted")
	}
	if !strings.Contains(err.Error(), "money_handling") {
		t.Errorf("refusal does not say why: %v", err)
	}
}

func TestMoneyWrites_EmptyAllowRefused(t *testing.T) {
	p := writeMoneyWritesConfig(t, "project:\n  name: p\nmoney_handling: true\n"+
		"money_writes:\n  allow: []\n"+
		"targets:\n  http:\n    base_url: http://sut.invalid\n")
	_, err := Load(p)
	if err == nil {
		t.Fatal("an empty money_writes.allow was accepted")
	}
}

func TestMoneyWrites_GETMethodRefused(t *testing.T) {
	p := writeMoneyWritesConfig(t, "project:\n  name: p\nmoney_handling: true\n"+
		"money_writes:\n  allow:\n    - method: GET\n      path: /api/v1/trading/quote\n      spends: false\n"+
		"targets:\n  http:\n    base_url: http://sut.invalid\n")
	if _, err := Load(p); err == nil {
		t.Fatal("method: GET was accepted in money_writes.allow")
	}
}

func TestMoneyWrites_SpendsTrueMissingAmountFieldRefused(t *testing.T) {
	p := writeMoneyWritesConfig(t, "project:\n  name: p\nmoney_handling: true\n"+
		"money_writes:\n  allow:\n    - method: POST\n      path: /api/v1/trading/order\n      spends: true\n"+
		"      max_amount: 25\n      max_per_run: 4\n"+
		"targets:\n  http:\n    base_url: http://sut.invalid\n")
	if _, err := Load(p); err == nil {
		t.Fatal("spends: true with no amount_field was accepted")
	}
}

func TestMoneyWrites_SpendsTrueMissingMaxAmountRefused(t *testing.T) {
	p := writeMoneyWritesConfig(t, "project:\n  name: p\nmoney_handling: true\n"+
		"money_writes:\n  allow:\n    - method: POST\n      path: /api/v1/trading/order\n      spends: true\n"+
		"      amount_field: source_amount\n      max_per_run: 4\n"+
		"targets:\n  http:\n    base_url: http://sut.invalid\n")
	if _, err := Load(p); err == nil {
		t.Fatal("spends: true with no max_amount was accepted")
	}
}

func TestMoneyWrites_SpendsTrueMissingMaxPerRunRefused(t *testing.T) {
	p := writeMoneyWritesConfig(t, "project:\n  name: p\nmoney_handling: true\n"+
		"money_writes:\n  allow:\n    - method: POST\n      path: /api/v1/trading/order\n      spends: true\n"+
		"      amount_field: source_amount\n      max_amount: 25\n"+
		"targets:\n  http:\n    base_url: http://sut.invalid\n")
	if _, err := Load(p); err == nil {
		t.Fatal("spends: true with no max_per_run was accepted")
	}
}

func TestMoneyWrites_SpendsFalseWithLimitFieldsRefused(t *testing.T) {
	p := writeMoneyWritesConfig(t, "project:\n  name: p\nmoney_handling: true\n"+
		"money_writes:\n  allow:\n    - method: POST\n      path: /api/v1/trading/quote\n      spends: false\n"+
		"      max_amount: 25\n"+
		"targets:\n  http:\n    base_url: http://sut.invalid\n")
	if _, err := Load(p); err == nil {
		t.Fatal("spends: false with max_amount set was accepted")
	}
}

func TestMoneyWrites_PathWithQueryStringRefused(t *testing.T) {
	p := writeMoneyWritesConfig(t, "project:\n  name: p\nmoney_handling: true\n"+
		"money_writes:\n  allow:\n    - method: POST\n      path: \"/api/v1/trading/order?x=1\"\n      spends: false\n"+
		"targets:\n  http:\n    base_url: http://sut.invalid\n")
	if _, err := Load(p); err == nil {
		t.Fatal("a path carrying a query string was accepted")
	}
}

func TestMoneyWrites_PathWithWildcardRefused(t *testing.T) {
	p := writeMoneyWritesConfig(t, "project:\n  name: p\nmoney_handling: true\n"+
		"money_writes:\n  allow:\n    - method: POST\n      path: \"/api/v1/trading/*\"\n      spends: false\n"+
		"targets:\n  http:\n    base_url: http://sut.invalid\n")
	if _, err := Load(p); err == nil {
		t.Fatal("a wildcard path was accepted")
	}
}

func TestMoneyWrites_ValidBlockAccepted(t *testing.T) {
	p := writeMoneyWritesConfig(t, "project:\n  name: p\nmoney_handling: true\n"+
		"money_writes:\n  allow:\n"+
		"    - method: POST\n      path: /api/v1/trading/quote\n      spends: false\n"+
		"    - method: POST\n      path: /api/v1/trading/order\n      spends: true\n"+
		"      amount_field: source_amount\n      max_amount: 25\n      max_per_run: 4\n"+
		"targets:\n  http:\n    base_url: http://sut.invalid\n")
	c, err := Load(p)
	if err != nil {
		t.Fatalf("a valid money_writes block was refused: %v", err)
	}
	al := c.MoneyWriteAllowlist()
	if len(al) != 2 {
		t.Fatalf("MoneyWriteAllowlist() = %d entries, want 2", len(al))
	}
	if al[1].Method != "POST" || al[1].Path != "/api/v1/trading/order" || !al[1].Spends ||
		al[1].AmountField != "source_amount" || al[1].MaxAmount != 25 || al[1].MaxPerRun != 4 {
		t.Errorf("second entry did not convert correctly: %+v", al[1])
	}
}

func TestMoneyWrites_AbsentBlockIsNilAllowlist(t *testing.T) {
	p := writeMoneyWritesConfig(t, "project:\n  name: p\nmoney_handling: true\n"+
		"targets:\n  http:\n    base_url: http://sut.invalid\n")
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if al := c.MoneyWriteAllowlist(); al != nil {
		t.Fatalf("no money_writes block produced a non-nil allowlist: %+v", al)
	}
}

// item 1's "not valid without money_handling" also holds for money_handling: false explicitly.
func TestMoneyWrites_RefusedWithMoneyHandlingExplicitlyFalse(t *testing.T) {
	p := writeMoneyWritesConfig(t, "project:\n  name: p\nmoney_handling: false\n"+
		"money_writes:\n  allow:\n    - method: POST\n      path: /api/v1/trading/quote\n      spends: false\n"+
		"targets:\n  http:\n    base_url: http://sut.invalid\n")
	if _, err := Load(p); err == nil {
		t.Fatal("money_writes with money_handling: false was accepted")
	}
}

// ── config.Validate integration: the exemption + the literal early amount check (item 3) ─────────

func TestValidate_MoneyWritesExemptsAnAllowedWriteAndCatchesALiteralOverspend(t *testing.T) {
	dir := t.TempDir()
	writeScenario(t, dir, "MW-ALLOWED", "POST `http://sut.invalid/api/v1/trading/quote`")
	writeMoneyScenarioWithBody(t, dir, "MW-OVERSPEND", "POST", "http://sut.invalid/api/v1/trading/order",
		`{"source_amount": 999}`)
	writeMoneyScenarioWithBody(t, dir, "MW-UNLISTED", "POST", "http://sut.invalid/api/v1/trading/rebalance", "{}")

	c := &Config{MoneyHandling: true, MoneyWrites: &MoneyWritesConfig{Allow: []MoneyWriteAllowEntry{
		{Method: "POST", Path: "/api/v1/trading/quote", Spends: boolPtr(false)},
		{Method: "POST", Path: "/api/v1/trading/order", Spends: boolPtr(true),
			AmountField: "source_amount", MaxAmount: 25, MaxPerRun: 4},
	}}}
	c.Project.Name = "p"
	c.Targets.HTTP = &HTTPTarget{BaseURL: "http://sut.invalid"}
	errs, n, err := c.Validate(dir)
	if err != nil || n != 3 {
		t.Fatalf("Validate = %d scenarios, err %v; want 3, nil", n, err)
	}
	by := map[string]string{}
	for _, e := range errs {
		by[e.Scenario] += e.Message
	}
	if by["MW-ALLOWED"] != "" {
		t.Errorf("an allowlisted spends:false write was refused: %q", by["MW-ALLOWED"])
	}
	if !strings.Contains(by["MW-OVERSPEND"], "money_writes:") || !strings.Contains(by["MW-OVERSPEND"], "999") {
		t.Errorf("a literal overspend was not caught at validate time: %q", by["MW-OVERSPEND"])
	}
	if !strings.Contains(by["MW-UNLISTED"], "money_handling:") {
		t.Errorf("an unlisted write was not refused: %q", by["MW-UNLISTED"])
	}
}

func boolPtr(b bool) *bool { return &b }

func writeMoneyScenarioWithBody(t *testing.T, dir, id, method, url, body string) {
	t.Helper()
	d := filepath.Join(dir, "http-ingestion")
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	md := strings.Join([]string{
		"# Scenario: " + id, "",
		"## Metadata", "- **ID**: " + id, "- **Layer**: HTTP Ingestion", "- **Tags**: smoke", "",
		"## TRIGGER", method + " `" + url + "`", "```json", body, "```", "",
		"## EXPECT", "### Runnable", "- status=200", "",
		"## TIMEOUT", "30s", "",
		"## CLEANUP", "N/A — a unit-test fixture; it creates nothing.", "",
	}, "\n")
	if err := os.WriteFile(filepath.Join(d, id+".md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
}
