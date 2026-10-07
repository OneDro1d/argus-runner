package config

import (
	"strings"
	"testing"
)

// VR-TH1 — the tool-honesty check. These tests exist because the DEFECT was invisible to every
// check that already ran: `message_broker.type: kafka` and `auth.type: api-key|mtls` were listed
// in schemas/argus-config.schema.yaml with no implementation, the schema is embedded and read by
// nothing at run time, and the Type keys are never dispatched on — so a config declaring any of
// them loaded clean and meant nothing.

// TestTargetType_RefusesATypeWithNoImplementation is the one that would have caught it.
func TestTargetType_RefusesATypeWithNoImplementation(t *testing.T) {
	for _, tc := range []struct {
		name, where, declared string
		supported             []string
	}{
		{"kafka broker", "targets.message_broker", "kafka", mqTypesSupported},
		{"api-key auth", "targets.auth", "api-key", authTypesSupported},
		{"mtls auth", "targets.auth", "mtls", authTypesSupported},
		{"oracle db — driver deliberately not shipped", "targets.database", "oracle", dbTypesSupported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := targetTypeError(tc.where, tc.declared, tc.supported)
			if got == nil {
				t.Fatalf("targetTypeError(%q, %q) = nil, want a refusal — an unimplemented type must "+
					"be refused at validate time, not discovered at run time", tc.where, tc.declared)
			}
			// The refusal must name what IS accepted: a bare "invalid" sends the operator to the
			// source to find out what to write instead.
			for _, want := range tc.supported {
				if !strings.Contains(got.Message, want) {
					t.Errorf("refusal %q does not name the accepted value %q", got.Message, want)
				}
			}
			if !strings.Contains(got.Message, tc.where) {
				t.Errorf("refusal %q does not say WHICH target is wrong", got.Message)
			}
		})
	}
}

// TestTargetType_AcceptsWhatTheProductActuallyServes guards the other direction: narrowing this
// set silently disables a SUT that works today.
func TestTargetType_AcceptsWhatTheProductActuallyServes(t *testing.T) {
	for _, tc := range []struct {
		where, declared string
		supported       []string
	}{
		{"targets.database", "postgres", dbTypesSupported},
		{"targets.database", "mysql", dbTypesSupported},
		// These three ship a JDBC driver (install-jdbc-drivers.sh) and were denied by the old
		// [postgres, mysql] enum — the schema was narrower than the product.
		{"targets.database", "mariadb", dbTypesSupported},
		{"targets.database", "sqlserver", dbTypesSupported},
		{"targets.database", "sqlite", dbTypesSupported},
		{"targets.message_broker", "amqp", mqTypesSupported},
		{"targets.auth", "bearer", authTypesSupported},
	} {
		if got := targetTypeError(tc.where, tc.declared, tc.supported); got != nil {
			t.Errorf("targetTypeError(%q, %q) = %q, want nil — this type is served today",
				tc.where, tc.declared, got.Message)
		}
	}
}

// TestTargetType_OmittedAndNoneAlwaysPass — the key is optional and decorative by design; most
// shipped configs omit it. Refusing an absent value would refuse nearly every real config.
func TestTargetType_OmittedAndNoneAlwaysPass(t *testing.T) {
	for _, declared := range []string{"", "none", "  ", "NONE"} {
		if got := targetTypeError("targets.database", declared, dbTypesSupported); got != nil {
			t.Errorf("targetTypeError(%q) = %q, want nil", declared, got.Message)
		}
	}
}

// TestTargetType_CaseAndSpaceInsensitive — a config written by hand should not fail on "Postgres".
func TestTargetType_CaseAndSpaceInsensitive(t *testing.T) {
	for _, declared := range []string{"Postgres", "POSTGRES", " postgres "} {
		if got := targetTypeError("targets.database", declared, dbTypesSupported); got != nil {
			t.Errorf("targetTypeError(%q) = %q, want nil", declared, got.Message)
		}
	}
}

// TestTargetTypeErrors_ChecksNamedEntriesByName — a wrong type inside a named map must say WHICH
// entry, or the operator has to bisect their own config to find it.
func TestTargetTypeErrors_ChecksNamedEntriesByName(t *testing.T) {
	c := &Config{}
	c.Targets.MessageBrokerTargets = map[string]*MQTarget{
		"audit": {Type: "kafka", URL: "kafka://k:9092"},
		"good":  {Type: "amqp", URL: "amqp://a:5672/"},
	}
	errs := c.targetTypeErrors()
	if len(errs) != 1 {
		t.Fatalf("got %d errors, want exactly 1 (only `audit` is wrong): %+v", len(errs), errs)
	}
	if !strings.Contains(errs[0].Message, "message_broker_targets.audit") {
		t.Errorf("error %q does not name the offending entry `audit`", errs[0].Message)
	}
}

// TestTargetTypeErrors_QuietOnAnEmptyConfig — a config declaring no targets at all must produce
// no type errors, so this check cannot turn every minimal config red.
func TestTargetTypeErrors_QuietOnAnEmptyConfig(t *testing.T) {
	c := &Config{}
	if errs := c.targetTypeErrors(); len(errs) != 0 {
		t.Errorf("got %+v, want none", errs)
	}
}

// TestValidate_WiresTheTargetTypeCheck is the SEAM test, and it exists because the six tests
// above do NOT cover the seam: every one of them calls targetTypeError/targetTypeErrors
// directly. Mutation-tested 2026-09-22 — replacing `errs := c.targetTypeErrors()` in
// Config.Validate with `var errs []ConfigError` (i.e. deleting the only call site) turned
// NOTHING red, in internal/config, internal/toolcore, internal/scenario or internal/auth.
//
// ⛔ That is the exact defect this whole change is about: enforcement that nothing invokes —
// the same shape as schemas/argus-config.schema.yaml, which is embedded and read by no one.
// The unit tests prove the check is CORRECT. Only this one proves it RUNS.
func TestValidate_WiresTheTargetTypeCheck(t *testing.T) {
	// No scenarios on purpose: the target-type check must not depend on the scenario walk,
	// because an unsupported target is wrong whether or not a scenario references that layer.
	scnDir := t.TempDir()
	c := loadYAML(t, "project:\n  name: seam\ntargets:\n  message_broker:\n    type: kafka\n    url: amqp://sut-rabbit:5672/\n")

	errs, _, err := c.Validate(scnDir)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	for _, e := range errs {
		if contains(e.Message, "targets.message_broker") && contains(e.Message, "kafka") {
			return
		}
	}
	t.Fatalf("Config.Validate did not surface the unsupported broker type — the check exists but "+
		"nothing calls it, which is the failure mode this package is meant to have ended. errs = %+v", errs)
}
