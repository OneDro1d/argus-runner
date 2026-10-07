package config

import (
	"fmt"
	"strings"

	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// moneywrites.go — "money writes" (T5.4 follow-up, 2026-09-26 product decision): the SUT config's
// OPTIONAL `money_writes` block, valid only beside `money_handling: true`. It names the exact
// (method, path) writes the SUT's own TESTER has decided Argus may attempt against a money-handling
// system, each optionally bounded as a real, capped spend. See internal/scenario/moneywrites.go for
// the pure allowlist/spend-check core this block is parsed INTO, and moneyguard.go for the rule it
// exempts requests from.
//
//	money_writes:
//	  allow:
//	    - method: POST
//	      path: /api/v1/trading/quote
//	      spends: false
//	    - method: POST
//	      path: /api/v1/trading/order
//	      spends: true
//	      amount_field: source_amount
//	      max_amount: 25
//	      max_per_run: 4

// MoneyWriteAllowEntry is one parsed `money_writes.allow` entry, before conversion to
// scenario.MoneyWriteAllow. Spends is a *bool (not bool) so validate can tell "spends: false" (an
// explicit, valid declaration) apart from "spends" simply absent (a required field left out) — the
// same required-vs-zero-value distinction MoneyHandling's own doc discusses for money_handling.
type MoneyWriteAllowEntry struct {
	Method      string  `yaml:"method"`
	Path        string  `yaml:"path"`
	Spends      *bool   `yaml:"spends"`
	AmountField string  `yaml:"amount_field,omitempty"`
	MaxAmount   float64 `yaml:"max_amount,omitempty"`
	MaxPerRun   int     `yaml:"max_per_run,omitempty"`
}

// MoneyWritesConfig is the whole parsed `money_writes` block.
type MoneyWritesConfig struct {
	Allow []MoneyWriteAllowEntry `yaml:"allow"`
}

// validate refuses a money_writes block that is present without money_handling: true, empty, or
// carrying a malformed entry — named by index and field, exactly as RateLimit.validate/Package.validate
// refuse their own blocks at LOAD time (parseConfig), so validate-config (and onboarding, which dies
// on it) reports it, never a run.
func (m *MoneyWritesConfig) validate(moneyHandling bool) error {
	if m == nil {
		return nil
	}
	if !moneyHandling {
		return fmt.Errorf("money_writes is declared but money_handling is not true — money_writes only " +
			"relaxes the money-path guard on a SUT that has turned it on; set money_handling: true or " +
			"remove the money_writes block")
	}
	if len(m.Allow) == 0 {
		return fmt.Errorf("money_writes.allow is empty — declare at least one entry or omit the money_writes block entirely")
	}
	for i, e := range m.Allow {
		where := fmt.Sprintf("money_writes.allow[%d]", i)
		method := strings.ToUpper(strings.TrimSpace(e.Method))
		if method == "" {
			return fmt.Errorf("%s: method is required", where)
		}
		if method == "GET" {
			return fmt.Errorf("%s: method must not be GET — money_writes exempts WRITES; a GET needs no exemption", where)
		}
		if strings.TrimSpace(e.Path) == "" {
			return fmt.Errorf("%s: path is required", where)
		}
		if strings.ContainsAny(e.Path, "*?") {
			return fmt.Errorf("%s: path %q must be an exact path — no wildcards (\"*\") and no query string (\"?\")", where, e.Path)
		}
		if !strings.HasPrefix(e.Path, "/") {
			return fmt.Errorf("%s: path %q must start with \"/\"", where, e.Path)
		}
		if e.Spends == nil {
			return fmt.Errorf("%s: spends is required (true or false)", where)
		}
		if *e.Spends {
			if strings.TrimSpace(e.AmountField) == "" {
				return fmt.Errorf("%s: spends: true requires amount_field", where)
			}
			if e.MaxAmount <= 0 {
				return fmt.Errorf("%s: spends: true requires max_amount > 0", where)
			}
			if e.MaxPerRun < 1 {
				return fmt.Errorf("%s: spends: true requires max_per_run >= 1", where)
			}
		} else if e.AmountField != "" || e.MaxAmount != 0 || e.MaxPerRun != 0 {
			return fmt.Errorf("%s: spends: false must not set amount_field/max_amount/max_per_run", where)
		}
	}
	return nil
}

// allowlist converts the parsed block into the pure type moneyguard.go / argus / chain consult. nil
// (no block declared) converts to nil — MoneyGuardViolations/Match already treat that as "exempt
// nothing", today's behaviour exactly.
func (m *MoneyWritesConfig) allowlist() scenario.MoneyWriteAllowlist {
	if m == nil || len(m.Allow) == 0 {
		return nil
	}
	out := make(scenario.MoneyWriteAllowlist, len(m.Allow))
	for i, e := range m.Allow {
		out[i] = scenario.MoneyWriteAllow{
			Method:      strings.ToUpper(strings.TrimSpace(e.Method)),
			Path:        e.Path,
			Spends:      e.Spends != nil && *e.Spends,
			AmountField: e.AmountField,
			MaxAmount:   e.MaxAmount,
			MaxPerRun:   e.MaxPerRun,
		}
	}
	return out
}

// MoneyWriteAllowlist is the exported accessor every door outside this package (argus, control,
// reporoute) uses to reach c's money_writes allowlist — never c.MoneyWrites directly, so the
// conversion stays in ONE place.
func (c *Config) MoneyWriteAllowlist() scenario.MoneyWriteAllowlist {
	return c.MoneyWrites.allowlist()
}
