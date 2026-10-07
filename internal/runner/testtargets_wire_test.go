package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/federation"
)

// testtargets_wire_test.go — (UI-6): the executor reports its `test_targets` on register
// and on EVERY poll, with the nil / "[]" / "[{…}]" rule of money_writes_allow. Mirrors moneywrites_wire_test.go.

const ttCfgBase = "project:\n  name: p\ntargets:\n  http:\n    base_url: http://sut.invalid\n"

func ttCfg(t *testing.T, extra string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "argus-config.yaml")
	if err := os.WriteFile(p, []byte(ttCfgBase+extra), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestTestTargetsFor_DeclaredBlockReported(t *testing.T) {
	p := ttCfg(t, "test_targets:\n  - {name: live, namespace: msgbus, match: {scenario_prefixes: [NHB-]}}\n  - {name: lab, match: {tags: [lab]}}\n")
	got := testTargetsFor(p)
	if got == nil {
		t.Fatal("testTargetsFor(declared block) = nil, want the encoded declaration")
	}
	var decoded []map[string]any
	if err := json.Unmarshal(got, &decoded); err != nil || len(decoded) != 2 || decoded[0]["name"] != "live" {
		t.Fatalf("not the declared list: %s (%v)", got, err)
	}
}

// the executor reports `load_test: never` with the rest of the declaration, and only for the
// target that declares it.
func TestTestTargetsFor_ReportsLoadTestNever(t *testing.T) {
	p := ttCfg(t, "test_targets:\n  - {name: live, load_test: never, match: {scenario_prefixes: [NHB-]}}\n  - {name: lab, match: {tags: [lab]}}\n")
	got := testTargetsFor(p)
	var decoded []map[string]any
	if err := json.Unmarshal(got, &decoded); err != nil || len(decoded) != 2 {
		t.Fatalf("not the declared list: %s (%v)", got, err)
	}
	if decoded[0]["load_test"] != "never" {
		t.Errorf("live did not report load_test: never: %s", got)
	}
	if _, has := decoded[1]["load_test"]; has {
		t.Errorf("lab reported a load_test it never declared: %s", got)
	}
}

func TestTestTargetsFor_NoBlockIsExplicitEmptyArray(t *testing.T) {
	got := testTargetsFor(ttCfg(t, ""))
	if string(got) != "[]" {
		t.Fatalf("testTargetsFor(no block) = %q, want \"[]\" — a loadable config with none declared is a REPORT, not silence", got)
	}
}

func TestTestTargetsFor_ConfigFailsToLoadIsNil(t *testing.T) {
	p := ttCfg(t, "test_targets:\n  - {name: a}\n") // empty match: refused at load
	if got := testTargetsFor(p); got != nil {
		t.Fatalf("testTargetsFor(config that fails to load) = %s, want nil", got)
	}
}

func TestTestTargetsFor_EmptyPathIsNil(t *testing.T) {
	if got := testTargetsFor(""); got != nil {
		t.Fatalf("testTargetsFor(\"\") = %s, want nil", got)
	}
}

func TestRegisterRequestFor_CarriesTestTargets(t *testing.T) {
	tt := json.RawMessage(`[{"name":"live","match":{"tags":["x"]}}]`)
	req := registerRequestFor(FedConfig{InstanceID: "i1"}, "pub", "x25519", nil, nil, nil, withRegisterTestTargets(tt))
	if string(req.TestTargets) != string(tt) {
		t.Fatalf("RegisterRequest.TestTargets = %s, want %s", req.TestTargets, tt)
	}
}

func TestPollRequestFor_CarriesTestTargets(t *testing.T) {
	req := pollRequestFor("0.3.50", nil, nil, nil, nil, nil, federation.SUTObservation{}, nil, nil, withPollTestTargets(json.RawMessage(`[]`)))
	if string(req.TestTargets) != "[]" {
		t.Fatalf("PollRequest.TestTargets = %s, want []", req.TestTargets)
	}
}

// An unchanged call site (no option) reports nothing — omitted on the wire, so an executor build that
// predates this field and a config that failed to load look identical to the control plane: "not reported".
func TestRequests_WithoutTheOptionOmitTestTargets(t *testing.T) {
	reg, _ := json.Marshal(registerRequestFor(FedConfig{InstanceID: "i1"}, "pub", "", nil, nil, nil))
	poll, _ := json.Marshal(pollRequestFor("0.3.50", nil, nil, nil, nil, nil, federation.SUTObservation{}, nil, nil))
	for name, raw := range map[string][]byte{"register": reg, "poll": poll} {
		if strings.Contains(string(raw), "test_targets") {
			t.Errorf("%s payload carries test_targets with no declaration: %s", name, raw)
		}
	}
}

// Wiring: the three real call sites (register-on-start, the poll loop) must pass the declaration. A helper
// that nothing calls reports nothing, and every instance would stay NULL silently (the money_handling lesson).
func TestTestTargetsIsWiredIntoRegisterAndTheLoop(t *testing.T) {
	for file, needle := range map[string]string{
		"runner.go":   "withRegisterTestTargets(testTargetsFor(s.fed.Exec.ConfigPath))",
		"executor.go": "withPollTestTargets(testTargetsFor(e.ConfigPath))",
	} {
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), needle) {
			t.Errorf("%s does not contain %q — test_targets would never leave the executor", file, needle)
		}
	}
}

// Old control plane: a payload that carries test_targets must decode cleanly into a body that has no such
// field (no DisallowUnknownFields), so the new executor keeps working against an old CP.
func TestPollPayloadWithTestTargetsDecodesIntoAnOlderShape(t *testing.T) {
	raw, err := json.Marshal(pollRequestFor("0.3.50", nil, nil, nil, nil, nil, federation.SUTObservation{}, nil, nil,
		withPollTestTargets(json.RawMessage(`[{"name":"live","match":{"tags":["x"]}}]`))))
	if err != nil {
		t.Fatal(err)
	}
	var older struct {
		RunnerVersion string `json:"runner_version"`
		SuiteVersion  string `json:"suite_version"`
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	if err := dec.Decode(&older); err != nil || older.RunnerVersion != "0.3.50" {
		t.Fatalf("an older control plane's decode failed on the new field: %v %+v", err, older)
	}
}

// Old executor: a payload WITHOUT the field decodes to nil = not reported.
func TestOlderExecutorPayloadDecodesToNotReported(t *testing.T) {
	var req federation.PollRequest
	if err := json.Unmarshal([]byte(`{"runner_version":"0.3.39","protocol_version":"`+federation.ProtocolVersion+`","money_handling":true}`), &req); err != nil {
		t.Fatal(err)
	}
	if req.TestTargets != nil {
		t.Fatalf("an old executor's poll decoded TestTargets = %s, want nil (not reported)", req.TestTargets)
	}
}
