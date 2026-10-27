package config

import (
	"bytes"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// moneyallow_cleanup_ui_fault_test.go — load-time handling of the `cleanup`, `ui` and `fault` entry kinds
//. Every refusal is named, with the entry index.

func TestMoneyAllowCleanupUIFault_EachKindLoadsAndConverts(t *testing.T) {
	c, err := loadAllow(t, ""+
		"    - engine: cleanup\n      form: sql\n"+
		"    - engine: cleanup\n      form: bash\n"+
		"    - engine: ui\n      spec: tests/live/argus-declared.spec.ts\n"+
		"    - engine: ui\n      spec: tests/live/other.spec.ts\n"+
		"    - engine: fault\n")
	if err != nil {
		t.Fatalf("a valid block with the new kinds was refused: %v", err)
	}
	al := c.MoneyWriteAllowlist()
	if len(al) != 5 {
		t.Fatalf("converted %d entries, want 5", len(al))
	}
	if al[0].Engine != "cleanup" || al[0].Form != "sql" || al[0].Method != "" || al[0].Path != "" {
		t.Errorf("cleanup sql: %+v", al[0])
	}
	if al[1].Engine != "cleanup" || al[1].Form != "bash" {
		t.Errorf("cleanup bash: %+v", al[1])
	}
	if al[2].Engine != "ui" || al[2].Spec != "tests/live/argus-declared.spec.ts" {
		t.Errorf("ui: %+v", al[2])
	}
	if al[4].Engine != "fault" || al[4].Form != "" || al[4].Spec != "" || al[4].Target != "" {
		t.Errorf("fault: %+v", al[4])
	}
	// and the converted list is what the guard consults
	if al.MatchCleanup("sql") == nil || al.MatchCleanup("bash") == nil || al.MatchUI("tests/live/other.spec.ts") == nil || al.MatchFault() == nil {
		t.Error("the converted list does not match its own entries")
	}
}

func TestMoneyAllowCleanupUIFault_LoadTimeRefusalsNamedWithTheEntryIndex(t *testing.T) {
	ok0 := "    - method: POST\n      path: /api/v1/trading/quote\n      spends: false\n"
	for _, tc := range []struct{ name, entry, want string }{
		{"cleanup with no form", "    - engine: cleanup\n",
			`money_writes.allow[1]: a cleanup entry requires form (one of sql, bash)`},
		{"cleanup with an unknown form", "    - engine: cleanup\n      form: python\n",
			`money_writes.allow[1]: form "python" is not a runnable cleanup form (one of sql, bash)`},
		{"cleanup form N/A", "    - engine: cleanup\n      form: na\n",
			`money_writes.allow[1]: form "na" is not a runnable cleanup form`},
		{"cleanup form is case sensitive", "    - engine: cleanup\n      form: SQL\n",
			`money_writes.allow[1]: form "SQL" is not a runnable cleanup form`},
		{"cleanup with a spec", "    - engine: cleanup\n      form: sql\n      spec: x.spec.ts\n",
			`money_writes.allow[1]: spec is not a field of an engine: cleanup entry`},
		{"cleanup with a target", "    - engine: cleanup\n      form: sql\n      target: ledger\n",
			`money_writes.allow[1]: target is not a field of an engine: cleanup entry`},
		{"cleanup with spends", "    - engine: cleanup\n      form: sql\n      spends: false\n",
			`money_writes.allow[1]: spends, amount_field, max_amount and max_per_run apply to HTTP entries only`},
		{"cleanup with max_amount", "    - engine: cleanup\n      form: sql\n      max_amount: 5\n",
			`money_writes.allow[1]: spends, amount_field, max_amount and max_per_run apply to HTTP entries only`},
		{"cleanup with max_per_run", "    - engine: cleanup\n      form: bash\n      max_per_run: 1\n",
			`apply to HTTP entries only`},
		{"cleanup with a method", "    - engine: cleanup\n      form: sql\n      method: POST\n",
			`money_writes.allow[1]: method is a field of an HTTP entry, and this entry is engine: cleanup`},
		{"cleanup form built at run time", "    - engine: cleanup\n      form: \"${F}\"\n",
			`a name built at run time (${…}) cannot be listed`},
		{"ui with no spec", "    - engine: ui\n",
			`money_writes.allow[1]: a ui entry requires spec`},
		{"ui with a blank spec", "    - engine: ui\n      spec: \"  \"\n",
			`money_writes.allow[1]: a ui entry requires spec`},
		{"ui spec built at run time", "    - engine: ui\n      spec: \"tests/${X}.spec.ts\"\n",
			`money_writes.allow[1]: "tests/${X}.spec.ts": a name built at run time (${…}) cannot be listed`},
		{"ui with a form", "    - engine: ui\n      spec: x.spec.ts\n      form: sql\n",
			`money_writes.allow[1]: form is not a field of an engine: ui entry`},
		{"ui with an app_url-like key is a mistyped key", "    - engine: ui\n      spec: x.spec.ts\n      app_url: http://x\n",
			`app_url`},
		{"ui with spends", "    - engine: ui\n      spec: x.spec.ts\n      spends: true\n",
			`apply to HTTP entries only`},
		{"ui with a path", "    - engine: ui\n      spec: x.spec.ts\n      path: /x\n",
			`path is a field of an HTTP entry, and this entry is engine: ui`},
		{"fault with a spec", "    - engine: fault\n      spec: x.spec.ts\n",
			`money_writes.allow[1]: spec is not a field of an engine: fault entry`},
		{"fault with a target", "    - engine: fault\n      target: trades\n",
			`money_writes.allow[1]: target is not a field of an engine: fault entry`},
		{"fault with a form", "    - engine: fault\n      form: sql\n",
			`money_writes.allow[1]: form is not a field of an engine: fault entry`},
		{"fault with spends", "    - engine: fault\n      spends: false\n",
			`apply to HTTP entries only`},
		{"form on an http entry", "    - method: POST\n      path: /x\n      spends: false\n      form: sql\n",
			`money_writes.allow[1]: form is a field of an engine entry (amqp, mcp, db, logs, cleanup, ui, fault), and this is an HTTP entry`},
		{"spec on an http entry", "    - method: GET\n      path: /x\n      spends: false\n      spec: x.spec.ts\n",
			`money_writes.allow[1]: spec is a field of an engine entry`},
		{"duplicate cleanup form", "    - engine: cleanup\n      form: sql\n    - engine: cleanup\n      form: sql\n",
			`money_writes.allow[2]: duplicates money_writes.allow[1]`},
		{"duplicate ui spec", "    - engine: ui\n      spec: a.spec.ts\n    - engine: ui\n      spec: a.spec.ts\n",
			`money_writes.allow[2]: duplicates money_writes.allow[1]`},
		{"duplicate fault", "    - engine: fault\n    - engine: fault\n",
			`money_writes.allow[2]: duplicates money_writes.allow[1]`},
		{"an unknown engine names the full list now", "    - engine: kafka\n",
			`engine "kafka" is not one of http, amqp, mcp, db, logs, cleanup, ui, fault`},
	} {
		_, err := loadAllow(t, ok0+tc.entry)
		if err == nil {
			t.Errorf("%s: the block was accepted", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s:\n  got  %v\n  want a refusal containing %q", tc.name, err, tc.want)
		}
	}
}

func TestMoneyAllowCleanupUIFault_DistinctEntriesAreNotDuplicates(t *testing.T) {
	if _, err := loadAllow(t, ""+
		"    - engine: cleanup\n      form: sql\n"+
		"    - engine: cleanup\n      form: bash\n"+
		"    - engine: ui\n      spec: a.spec.ts\n"+
		"    - engine: ui\n      spec: b.spec.ts\n"+
		"    - engine: fault\n"+
		"    - engine: logs\n"+
		"    - engine: db\n"); err != nil {
		t.Fatalf("distinct entries were refused as duplicates: %v", err)
	}
}

func TestMoneyAllowCleanupUIFault_StrictNodeRefusesAMistypedKey(t *testing.T) {
	for _, key := range []string{"frm", "specs", "spc", "forms"} {
		_, err := loadAllow(t, "    - engine: fault\n      "+key+": x\n")
		if err == nil || !strings.Contains(err.Error(), key) {
			t.Errorf("a mistyped key %q was not refused by name: %v", key, err)
		}
	}
}

func TestMoneyAllowCleanupUIFault_ABlockWithoutMoneyHandlingStillRefusesTheKinds(t *testing.T) {
	for _, entry := range []string{"    - engine: cleanup\n      form: sql\n", "    - engine: ui\n      spec: x.spec.ts\n", "    - engine: fault\n"} {
		p := writeMoneyWritesConfig(t, "project:\n  name: p\n"+"money_writes:\n  allow:\n"+entry+allowTargets)
		if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "money_handling") {
			t.Errorf("entry %q without money_handling: true was accepted: %v", entry, err)
		}
	}
}

// ⛔ OLDER EXECUTORS REFUSE THE NEW KEYS, THEY DO NOT IGNORE THEM. The base (dev 45b1d139) decodes
// `money_writes` inside the strict `targets` document with KnownFields(true) (config.go strictTargetsDoc), so
// a `form` or `spec` key it does not know is an error, and its engineOf refuses an `engine` value it does
// not list. This test decodes an entry with the BASE's field set the way the base's strict node does.
func TestMoneyAllowCleanupUIFault_ABaseStrictNodeRefusesTheNewKeys(t *testing.T) {
	type baseEntry struct {
		Method      string  `yaml:"method"`
		Path        string  `yaml:"path"`
		Spends      *bool   `yaml:"spends"`
		AmountField string  `yaml:"amount_field,omitempty"`
		MaxAmount   float64 `yaml:"max_amount,omitempty"`
		MaxPerRun   int     `yaml:"max_per_run,omitempty"`
		Engine      string  `yaml:"engine,omitempty"`
		Op          string  `yaml:"op,omitempty"`
		Exchange    *string `yaml:"exchange,omitempty"`
		RoutingKey  *string `yaml:"routing_key,omitempty"`
		Queue       *string `yaml:"queue,omitempty"`
		Target      string  `yaml:"target,omitempty"`
		Tool        string  `yaml:"tool,omitempty"`
	}
	type baseDoc struct {
		MoneyWrites *struct {
			Allow []baseEntry `yaml:"allow"`
		} `yaml:"money_writes"`
	}
	for _, tc := range []struct{ name, entry, key string }{
		{"cleanup form", "- engine: cleanup\n      form: sql\n", "form"},
		{"ui spec", "- engine: ui\n      spec: x.spec.ts\n", "spec"},
	} {
		dec := yaml.NewDecoder(bytes.NewReader([]byte("money_writes:\n  allow:\n    " + tc.entry)))
		dec.KnownFields(true)
		var d baseDoc
		err := dec.Decode(&d)
		if err == nil || !strings.Contains(err.Error(), "field "+tc.key+" not found") {
			t.Errorf("%s: the base strict decode did not refuse %q by name: %v", tc.name, tc.key, err)
		}
	}
	// `engine: fault` has no new key, so the strict decode accepts it; the base refuses it later, in
	// MoneyWritesConfig.validate, because its engineOf lists http, amqp, mcp, db, logs only (read at 45b1d139,
	// internal/config/moneywrites.go) -- the same refusal an unknown engine gets today
	// (TestMoneyAllow_LoadTimeRefusalsNamedWithTheEntryIndex, "unknown engine").
}
