// Package role models the dark-factory hat (product | test) and the access policy the
// suite enforces. Since M2.5 (D2) the hat is determined by AUTHORIZATION — the presented
// bearer token IS the hat (ARGUS_RUNNER_TOKEN → product, ARGUS_EXECUTOR_SECRET (formerly
// ARGUS_AUTHOR_TOKEN) → test; see internal/auth) — NOT a self-asserted --role (that is no
// longer an auth mechanism).
// The policy here drives both the CLI command gate (which commands a hat may run) and the
// failure-context redaction (what a hat's bundle may contain). It is the safety-critical
// core: a wrong answer here leaks the dark-factory holdout to the product hat.
package role

import "fmt"

// Role is the dark-factory hat (resolved from the bearer token since M2.5/D2).
type Role string

const (
	// Product is the product-agent hat (fixes SUT code). Runner-side commands
	// only; NO scenario access; its failure-context bundle excludes the
	// scenario and the test's expected values (works from observed reality).
	Product Role = "product"
	// Test is the test-agent hat (authors + runs scenarios). Full access.
	Test Role = "test"
	// Executor is the executor-agent hat (AC-16): a runner calling its own federation verbs —
	// register/poll/materialize/run-begin/results/rotate/heartbeat/deregister — as executor__*
	// hub tool calls, with its own credential (SY-11). Never reachable via Parse: no CLI command
	// self-asserts it, and no human picks it with --role. It carries neither CanAccessScenarios
	// (the author holdout) nor any runner-scope command grant — the executor plane is a THIRD,
	// disjoint scope, not a superset or subset of product/test.
	Executor Role = "executor"
)

// Parse validates a --role flag value. Defaults are decided by the caller; this
// rejects anything that is not exactly "product" or "test".
func Parse(s string) (Role, error) {
	switch Role(s) {
	case Product:
		return Product, nil
	case Test:
		return Test, nil
	default:
		return "", fmt.Errorf("invalid role %q (want %q or %q)", s, Product, Test)
	}
}

// CanAccessScenarios reports whether this hat may run the scenario-side commands
// (list-scenarios, read-scenario, write-scenario; validate-scenario is tokenless since it reads only a local file). The product
// hat may NOT — that is the hard dark-factory boundary (VR-B1, UC-8).
func (r Role) CanAccessScenarios() bool { return r == Test }

// MustRedactExpected reports whether the failure.expected field (the test's
// assertion) must be withheld from this hat's failure-context bundle (VR-C2).
func (r Role) MustRedactExpected() bool { return r == Product }

// MustOmitScenario reports whether the scenario section (markdown + parsed +
// references) must be omitted entirely from this hat's bundle (VR-C3).
func (r Role) MustOmitScenario() bool { return r == Product }
