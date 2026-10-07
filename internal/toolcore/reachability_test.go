package toolcore

// INT-019 (wave 1.4): validate_config confirmed TWO of the three things UC045 asks it to confirm.
//
// UC045 is the bootup ritual: "config parses, scenarios coverable, SUT REACHABLE FROM THE RUNNER'S
// VANTAGE -> green". Live against orderservice-compose the tool returned valid:true with
// layers_configured, targets_configured, scenario_layers and scenarios_found — and NO reachability field
// of any kind. A grep for reachability in internal/toolcore found it only in mcpcall.go (a per-call MCP
// flag) and the scenario-source resolver. The third check simply was not there.
//
// It matters more than a missing field. valid:true is what an operator reads as "green, the environment
// is wired"; they then issue the first run, which is the ONLY thing that discovers the SUT is
// unreachable — one layer later and with a far noisier failure. Same shape as INT-014 and the F5 browser
// defect: a health surface reading green for something it never checked.
//
// These tests pin the honesty properties, not just the happy path. The rule that governs them: a probe
// that COULD NOT RUN is never a pass. auth declares a type and a bearer token and NO ADDRESS, so it is
// permanently unprobeable — and must say so out loud rather than being quietly dropped from the list.

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// listenOnce opens a real listener and returns its host:port. Reachability is a property of the network,
// so the reachable case is proven against an actual open socket rather than a stub.
func listenOnce(t *testing.T) (addr string, closeFn func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	return ln.Addr().String(), func() { _ = ln.Close() }
}

// closedPort binds then immediately releases, so the port is real, local, and nothing is listening.
func closedPort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

func writeCfg(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "argus-config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return p
}

func findTarget(r Reachability, name string) *TargetReach {
	for i := range r.Targets {
		if r.Targets[i].Target == name {
			return &r.Targets[i]
		}
	}
	return nil
}

const cfgHead = `project:
  name: probe-sut
targets:
`

func TestProbe_AnOpenSocketIsReachable(t *testing.T) {
	addr, done := listenOnce(t)
	defer done()
	cfg := writeCfg(t, cfgHead+"  http:\n    base_url: http://"+addr+"\n")

	r := probeConfig(t, cfg)
	got := findTarget(r, "http")
	if got == nil {
		t.Fatal("no http entry in the reachability report — a configured target must always be listed")
	}
	if got.Status != ReachOK {
		t.Errorf("http status = %q (%s), want %q against a live listener", got.Status, got.Detail, ReachOK)
	}
	if !strings.Contains(got.Address, "127.0.0.1") {
		t.Errorf("address = %q, want the host:port actually dialled — the operator has to know WHAT was probed", got.Address)
	}
}

func TestProbe_AClosedPortIsUNREACHABLE_notSilence(t *testing.T) {
	// THE defect's consequence: this is the condition that used to surface only on the first run.
	cfg := writeCfg(t, cfgHead+"  http:\n    base_url: http://"+closedPort(t)+"\n")

	r := probeConfig(t, cfg)
	got := findTarget(r, "http")
	if got == nil || got.Status != ReachDown {
		t.Fatalf("got %+v, want status %q — an unreachable SUT must be stated, not discovered by a run (INT-019)", got, ReachDown)
	}
	if got.Detail == "" {
		t.Error("unreachable with no detail — the operator cannot act on a bare false")
	}
	if r.AllReachable {
		t.Error("AllReachable is true while a target is DOWN")
	}
}

func TestProbe_AuthIsNOT_APPLICABLE_becauseItCanNeverHaveAnAddress(t *testing.T) {
	// auth declares {type, bearer_token} — there is nothing to dial, ever. The honest answer is
	// "here is why it was not dialled", never omission (which reads as "fine") and never a pass.
	//
	// ⚠ THE STATUS CHANGED IN VR7-J1 (V24-001), AND THE CHANGE IS THE POINT. This used to be
	// `not_checked`, which put it in the same bucket as a target whose address is merely missing — and
	// one such target forces the whole verdict to nil. Measured 2026-08-19: both OrderService instances
	// reported NOTHING for reachability on two different tiers, while Social (["mcp"]) and Memstore
	// (["http","mcp"]) reported fine on three between them. The distinction dialAddrFor already drew in
	// prose — "permanently unprobeable BY SHAPE" — now exists in the data.
	//
	// What did NOT change: it is still in the report, still carries a reason, and still never counts as
	// a pass.
	cfg := writeCfg(t, cfgHead+"  auth:\n    type: bearer\n    bearer_token: tok\n")

	r := probeConfig(t, cfg)
	got := findTarget(r, "auth")
	if got == nil {
		t.Fatal("auth was configured and is missing from the report entirely — absence reads as health")
	}
	if got.Status != ReachNotApplicable {
		t.Errorf("auth status = %q, want %q", got.Status, ReachNotApplicable)
	}
	if got.Detail == "" {
		t.Error("not_applicable with no reason is indistinguishable from a bug")
	}
	// It is excluded from the VERDICT but must never be excluded from the REPORT.
	if !strings.Contains(r.Summary, "not applicable") {
		t.Errorf("summary = %q — a target that was not dialled must still be named, or omission reads as fine", r.Summary)
	}
	// And a config whose ONLY target cannot be dialled is still not a pass: nothing was ever reached.
	if r.AllReachable {
		t.Error("AllReachable is true for a SUT with no dialable target at all — an empty probe is not a pass")
	}
}

func TestProbe_NOT_CHECKED_isNotAPass(t *testing.T) {
	// The absence-is-not-health rule, as an invariant: every checkable target is up, and AllReachable
	// must STILL be false because one target could not be checked at all.
	//
	// ⚠ THE EXAMPLE CHANGED IN VR7-J1 (V24-001); THE RULE DID NOT. This test used to reach that state
	// with an `auth` target. It no longer can: `auth` is {type, bearer_token} and can never carry an
	// address, so it now reports `not_applicable` and is excluded from the verdict — that exclusion is
	// the whole point of VR7-J1, because counting it silenced both OrderService instances while Social
	// and Memstore reported fine.
	//
	// A `message_broker` whose queues are declared and whose URL is absent is the honest example of the
	// rule this test protects: the broker is real, the runner must reach it, and nobody said where it
	// is. That is a GAP, and a gap must still make AllReachable false.
	addr, done := listenOnce(t)
	defer done()
	cfg := writeCfg(t, cfgHead+"  http:\n    base_url: http://"+addr+"\n  message_broker:\n    type: amqp\n    queues: {incoming: q}\n")

	r := probeConfig(t, cfg)
	if findTarget(r, "http").Status != ReachOK {
		t.Fatal("precondition: the http target should be reachable here")
	}
	if r.AllReachable {
		t.Error("AllReachable is TRUE while a target was never checked. 'Nothing failed' is not 'everything " +
			"passed' — that conflation is the whole INT-019 family")
	}
	if !strings.Contains(r.Summary, "not checked") {
		t.Errorf("summary = %q, must SAY that something went unchecked", r.Summary)
	}
}

func TestProbe_NeverEchoesACredential(t *testing.T) {
	// A broker URL carries userinfo. The report is read and pasted by humans; it must carry the address
	// and nothing else.
	const secret = "hunter2supersecret"
	cfg := writeCfg(t, cfgHead+"  message_broker:\n    url: amqp://rabbit:"+secret+"@"+closedPort(t)+"/\n")

	r := probeConfig(t, cfg)
	blob := fmt.Sprintf("%+v", r)
	if strings.Contains(blob, secret) {
		t.Errorf("the probe report contains the broker PASSWORD:\n%s", blob)
	}
	if got := findTarget(r, "message_broker"); got == nil || got.Status != ReachDown {
		t.Errorf("got %+v, want the broker probed and reported DOWN", got)
	}
}

func TestProbe_DatabaseJDBCURLIsProbed(t *testing.T) {
	addr, done := listenOnce(t)
	defer done()
	cfg := writeCfg(t, cfgHead+"  database:\n    jdbc_url: jdbc:postgresql://"+addr+"/orders\n    username: ro\n    password: pw\n")

	r := probeConfig(t, cfg)
	got := findTarget(r, "database")
	if got == nil || got.Status != ReachOK {
		t.Fatalf("got %+v, want the jdbc host:port dialled and reachable", got)
	}
	if strings.Contains(fmt.Sprintf("%+v", r), "pw") && !strings.Contains(addr, "pw") {
		t.Error("the database PASSWORD leaked into the report")
	}
}

func TestProbe_CanBeDisabled_andThenNothingClaimsToBeReachable(t *testing.T) {
	// An operator may need to skip the dial (an air-gapped check, a SUT that is deliberately down).
	// Disabling the probe must turn every verdict into not_checked — never into a pass.
	addr, done := listenOnce(t)
	defer done()
	cfg := writeCfg(t, cfgHead+"  http:\n    base_url: http://"+addr+"\n")
	t.Setenv(probeDisableEnv, "1")

	r := probeConfig(t, cfg)
	got := findTarget(r, "http")
	if got == nil || got.Status != ReachUnknown {
		t.Fatalf("got %+v, want %q when probing is disabled", got, ReachUnknown)
	}
	if r.AllReachable {
		t.Error("probing is OFF and AllReachable is true — the surface would read green having checked nothing")
	}
	if !strings.Contains(got.Detail, probeDisableEnv) {
		t.Errorf("detail = %q, must name the switch that disabled the probe so the state is explicable", got.Detail)
	}
}

// ── the tool surface ─────────────────────────────────────────────────────────────────────────────
func TestValidateConfig_ReportsReachability_andWarnsWithoutFailingTheConfig(t *testing.T) {
	down := closedPort(t)
	cfg := writeCfg(t, cfgHead+"  http:\n    base_url: http://"+down+"\n")

	out, isErr, err := ValidateConfig(Env{ConfigPath: cfg, ScenariosDir: t.TempDir()})
	if err != nil {
		t.Fatalf("ValidateConfig: %v", err)
	}
	m, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("payload type %T", out)
	}

	if _, present := m["reachability"]; !present {
		t.Fatal("no `reachability` key. UC045 asks validate_config to confirm the SUT is reachable from " +
			"the runner's vantage; it reported only that the config parses (INT-019)")
	}
	// An unreachable SUT is not a malformed config. Conflating them would make a temporarily-down SUT
	// look like a broken file and send the operator to edit YAML.
	if m["valid"] != true {
		t.Errorf("valid = %v, want true — the CONFIG is fine; it is the SUT that is down", m["valid"])
	}
	if isErr {
		t.Error("isErr is set for an unreachable SUT; that is a warning condition, not a tool error")
	}
	warns, _ := m["reachability_warnings"].([]string)
	joined := strings.Join(warns, " | ")
	if !strings.Contains(strings.ToLower(joined), "unreachable") {
		t.Errorf("warnings = %q. An operator who reads valid:true and skips the nested block must still "+
			"be told the SUT did not answer", joined)
	}
}

func probeConfig(t *testing.T, path string) Reachability {
	t.Helper()
	c, err := loadConfigForProbe(path)
	if err != nil {
		t.Fatalf("load config %s: %v", path, err)
	}
	return ProbeTargets(c, 750*time.Millisecond)
}
