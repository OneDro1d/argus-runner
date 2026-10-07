package config

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// test_targets_test.go — (UI-6 backend): the `test_targets` block of argus-config.
// TOP-LEVEL and lenient for an OLDER executor (it ignores the key); strict INSIDE the block for this one.

const ttBase = "project:\n  name: p\ntargets:\n  http:\n    base_url: http://sut.invalid\n"

func writeTTConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "argus-config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestTestTargets_ParsesTheDesignExample(t *testing.T) {
	p := writeTTConfig(t, ttBase+`test_targets:
  - name: live
    label: msgbus live
    namespace: msgbus
    description: Permissions, delivery loop, broker health and memory headroom.
    match: { scenario_prefixes: [NHB-], tags: [heartbeat, slack-pulse] }
  - name: lab
    label: msgbus lab
    namespace: msgbus-lab
    match: { scenario_prefixes: [NLB-], tags: [lab, sink] }
`)
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.TestTargets) != 2 || c.TestTargets[0].Name != "live" || c.TestTargets[1].Namespace != "msgbus-lab" {
		t.Fatalf("TestTargets = %#v", c.TestTargets)
	}
	if got := c.TestTargets[1].Match.Tags; len(got) != 2 || got[0] != "lab" {
		t.Fatalf("lab tags = %v", got)
	}
}

func TestTestTargets_AbsentIsTodaysBehaviour(t *testing.T) {
	c, err := Load(writeTTConfig(t, ttBase))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.TestTargets != nil {
		t.Fatalf("an absent test_targets decoded to %#v, want nil (not declared)", c.TestTargets)
	}
}

func TestTestTargets_UnknownKeyWithinTheBlockIsRefusedByName(t *testing.T) {
	p := writeTTConfig(t, ttBase+`test_targets:
  - name: live
    namespce: msgbus
    match: { tags: [x] }
`)
	_, err := Load(p)
	if err == nil {
		t.Fatal("an unknown key inside test_targets was accepted")
	}
	if !strings.Contains(err.Error(), "namespce") || !strings.Contains(err.Error(), "test_targets") {
		t.Fatalf("refusal does not name the key and the place: %v", err)
	}
}

func TestTestTargets_UnknownKeyInsideMatchIsRefused(t *testing.T) {
	p := writeTTConfig(t, ttBase+`test_targets:
  - name: live
    match: { scenario_prefix: [NHB-] }
`)
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "scenario_prefix") {
		t.Fatalf("a typo inside match was not refused by name: %v", err)
	}
}

func TestTestTargets_InvalidDeclarationsFailAtLoadTime(t *testing.T) {
	for name, body := range map[string]string{
		"duplicate":   "test_targets:\n  - {name: a, match: {tags: [x]}}\n  - {name: a, match: {tags: [y]}}\n",
		"empty match": "test_targets:\n  - {name: a}\n",
		"bad name":    "test_targets:\n  - {name: A_b, match: {tags: [x]}}\n",
	} {
		if _, err := Load(writeTTConfig(t, ttBase+body)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// An OLDER executor must keep loading a config that carries the new key. This simulates its decode: the
// strict pass is over `targets` (+ rate_limit/package/money_writes/message_schemas) with every other
// top-level key absorbed into an inline map, and the whole-file decode is lenient.
func TestTestTargets_KeyIsTopLevelSoAnOlderExecutorIgnoresIt(t *testing.T) {
	b := []byte(ttBase + "test_targets:\n  - {name: live, match: {tags: [x]}}\n")
	var older struct {
		Targets Targets        `yaml:"targets"`
		Rest    map[string]any `yaml:",inline"`
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&older); err != nil {
		t.Fatalf("an older strict decode (targets + inline rest) refused a config with test_targets: %v", err)
	}
	if _, ok := older.Rest["test_targets"]; !ok {
		t.Fatalf("test_targets was not absorbed by the older executor's inline map: %v", older.Rest)
	}
}

// `outside_cluster: true` loads; with a `namespace` on the same target the load is refused, and
// the refusal names both keys (so validate-config prints it).
func TestTestTargets_OutsideClusterLoads(t *testing.T) {
	c, err := Load(writeTTConfig(t, ttBase+"test_targets:\n  - {name: site, outside_cluster: true, match: {tags: [x]}}\n  - {name: lab, namespace: ns-lab, match: {tags: [y]}}\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.TestTargets[0].OutsideCluster || c.TestTargets[1].OutsideCluster {
		t.Fatalf("TestTargets = %#v", c.TestTargets)
	}
}

func TestTestTargets_OutsideClusterWithNamespaceRefusedNamingBothKeys(t *testing.T) {
	_, err := Load(writeTTConfig(t, ttBase+"test_targets:\n  - {name: site, namespace: ns-x, outside_cluster: true, match: {tags: [x]}}\n"))
	if err == nil {
		t.Fatal("outside_cluster together with namespace was accepted")
	}
	for _, w := range []string{"outside_cluster", "namespace", "site"} {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("refusal %q lacks %q", err, w)
		}
	}
}

// ⛔ The key must NOT live under `targets:` — that block is strict, so an older executor would refuse the
// WHOLE config on a rollback.
func TestTestTargets_UnderTargetsIsRefusedHere(t *testing.T) {
	p := writeTTConfig(t, "project:\n  name: p\ntargets:\n  http:\n    base_url: http://sut.invalid\n  test_targets: []\n")
	if _, err := Load(p); err == nil {
		t.Fatal("test_targets under targets: was accepted — an older executor would refuse that config")
	}
}
