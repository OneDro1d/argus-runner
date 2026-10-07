package runner

import (
	"encoding/json"

	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/federation"
	"github.com/OneDro1d/argus-runner/internal/scenario"
	"github.com/OneDro1d/argus-runner/internal/testtargets"
)

// declaredTargets loads the SUT's argus-config and returns the JSON array of target keys it declares
// (http/message_broker/database/external/auth/mcp) — sent at registration so the cloud
// validate_scenario coverability check (UC030) knows what the instance actually targets. Best-effort:
// a missing/unreadable config yields nil (no declared targets → no coverability warnings).
func declaredTargets(configPath string) json.RawMessage {
	if configPath == "" {
		return nil
	}
	c, err := config.Load(configPath)
	if err != nil {
		return nil
	}
	var out []string
	for _, k := range []string{"http", "message_broker", "database", "external", "auth", "mcp"} {
		if c.TargetPresentExported(k) {
			out = append(out, k)
		}
	}
	if out == nil {
		return nil
	}
	b, _ := json.Marshal(out)
	return b
}

// moneyHandlingFor loads the SUT's argus-config the same way declaredTargets does and returns its
// money_handling declaration (T5.4 follow-up) — sent on register and on every poll so
// the control plane's money-path guard can refuse order/quote/swap/rebalance/claim generation before
// it ever reaches this instance (moneyGuardRefusal, internal/control).
//
// A *bool, not a bool: nil means NOT REPORTED, and it MUST mean that only for the two cases that
// genuinely carry no information — no config path at all, or a config that failed to load (an
// unresolved ${VAR}, bad YAML, a missing file). A config that loads cleanly always has an answer,
// even when the file never mentions money_handling: the YAML's own zero value (false) IS the
// declaration then, exactly as config.Config.MoneyHandling's own doc comment says ("Absent = false =
// nothing changes"). Collapsing that case to nil would make an executor whose config loads fine look
// identical, on the wire, to one that has told the control plane nothing at all.
func moneyHandlingFor(configPath string) *bool {
	if configPath == "" {
		return nil
	}
	c, err := config.Load(configPath)
	if err != nil {
		return nil
	}
	v := c.MoneyHandling
	return &v
}

// RegisterOption / PollOption add a wire field to the register / poll payload WITHOUT widening
// registerRequestFor / pollRequestFor's positional argument lists again (every new field used to touch
// ~30 call sites; sibling changes that each add one collide). The first user is test_targets.
type RegisterOption func(*federation.RegisterRequest)
type PollOption func(*federation.PollRequest)

func withRegisterTestTargets(tt json.RawMessage) RegisterOption {
	return func(r *federation.RegisterRequest) { r.TestTargets = tt }
}

func withPollTestTargets(tt json.RawMessage) PollOption {
	return func(r *federation.PollRequest) { r.TestTargets = tt }
}

func withRegisterDashboardLink(tmpl, label *string) RegisterOption {
	return func(r *federation.RegisterRequest) { r.DashboardLinkTemplate, r.DashboardLinkLabel = tmpl, label }
}

func withPollDashboardLink(tmpl, label *string) PollOption {
	return func(r *federation.PollRequest) { r.DashboardLinkTemplate, r.DashboardLinkLabel = tmpl, label }
}

// dashboardLinkFor loads the SUT's argus-config the same way testTargetsFor does and returns its
// observability.dashboard_link template and label (, UI-2) -- sent on register and on every
// poll so the control plane can render a per-run link without the executor being involved again.
//
// THREE-VALUED, both pointers together: nil = NOT REPORTED, for the two cases that carry no information
// (no config path, or a config that failed to load -- including a template the validator refused); a
// pointer to "" = REPORTED NONE (a config that loads cleanly and declares no block, so a template removed
// from the config clears on the control plane); otherwise the declared values.
func dashboardLinkFor(configPath string) (tmpl, label *string) {
	if configPath == "" {
		return nil, nil
	}
	c, err := config.Load(configPath)
	if err != nil {
		return nil, nil
	}
	t, l := c.DashboardLinkDecl()
	return &t, &l
}

// testTargetsFor loads the SUT's argus-config the same way moneyWritesAllowFor does and returns its
// top-level `test_targets` declaration, JSON-encoded (, UI-6) — sent on register and on
// every poll so the control plane can show an instance's targets and stamp each run with the ones it
// tested. The stamp comes from THIS declaration and the run's own selection, never from the SUT.
//
// nil means NOT REPORTED, for the same two cases moneyWritesAllowFor's nil means it: no config path, or
// a config that failed to load. A config that loads cleanly ALWAYS reports something — an explicit "[]"
// when it declares no test_targets — so "declared none" is distinguishable on the wire from silence.
func testTargetsFor(configPath string) json.RawMessage {
	if configPath == "" {
		return nil
	}
	c, err := config.Load(configPath)
	if err != nil {
		return nil
	}
	b, err := testtargets.Encode(c.TestTargets)
	if err != nil {
		return nil
	}
	return b
}

// moneyWritesAllowFor loads the SUT's argus-config the same way moneyHandlingFor does and returns
// its money_writes.allow block, JSON-encoded (money_writes follow-up, 2026-09-26) — sent on register
// and on every poll so the control plane's money-path guard can exempt exactly what the SUT's own
// tester declared, never anything wider (moneyGuardRefusal, internal/control).
//
// nil means NOT REPORTED, for the SAME two cases moneyHandlingFor's nil means it: no config path at
// all, or a config that failed to load. A config that loads cleanly ALWAYS reports something, even
// when it declares no money_writes block at all: that reports as an EXPLICIT empty JSON array ("[]"),
// never nil — collapsing it to nil would make a config that loads fine indistinguishable, on the
// wire, from an executor that has told the control plane nothing.
func moneyWritesAllowFor(configPath string) json.RawMessage {
	if configPath == "" {
		return nil
	}
	c, err := config.Load(configPath)
	if err != nil {
		return nil
	}
	al := c.MoneyWriteAllowlist()
	if al == nil {
		al = scenario.MoneyWriteAllowlist{}
	}
	b, merr := json.Marshal(al)
	if merr != nil {
		return nil
	}
	return b
}
