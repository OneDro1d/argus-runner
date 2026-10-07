package toolcore

import (
	"strings"
	"testing"
	"time"
)

// VR10-S3-11 / acceptance 12 (V28-012): validate-config lists every named entry in
// targets_configured as kind:name, probes it, and reports an unreachable one by name — so a wrong
// `graph` address is discovered by the check that exists for it, not by a red scenario. `mcp` joins
// the list (absent since the field was added).

const namedTargetsCfg = cfgHead + `  http:
    base_url: http://gw:8090
  mcp:
    base_url: http://gw:8090/mcp
  http_targets:
    graph:
      base_url: http://graph:8096
  database_targets:
    neo4j:
      jdbc_url: jdbc:neo4j://neo:7687
      username: neo4j
      password: pw
`

func TestValidateConfig_TargetsConfiguredListsNamedEntriesAndMCP(t *testing.T) {
	t.Setenv(probeDisableEnv, "1")
	cfg := writeCfg(t, namedTargetsCfg)
	out, _, err := ValidateConfig(Env{ConfigPath: cfg, ScenariosDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	m := out.(map[string]any)
	targets, _ := m["targets_configured"].([]string)
	joined := strings.Join(targets, ",")
	for _, want := range []string{"http", "mcp", "http:graph", "database:neo4j"} {
		found := false
		for _, x := range targets {
			if x == want {
				found = true
			}
		}
		if !found {
			t.Errorf("targets_configured must list %q, got [%s]", want, joined)
		}
	}
	reach, _ := m["reachability"].(Reachability)
	for _, want := range []string{"http_targets.graph", "database_targets.neo4j", "mcp"} {
		if findTarget(reach, want) == nil {
			t.Errorf("reachability must report the named entry %q (by name), got %+v", want, reach.Targets)
		}
	}
}

// A named entry on a closed port is UNREACHABLE by name — the operator learns `graph` is wrong
// BEFORE a scenario goes red for it. The plain slot's verdict is untouched.
func TestProbeTargets_NamedEntryUnreachableByName(t *testing.T) {
	open, done := listenOnce(t)
	defer done()
	closed := closedPort(t)
	c, err := loadConfigForProbe(writeCfg(t, cfgHead+"  http:\n    base_url: http://"+open+"\n  http_targets:\n    graph:\n      base_url: http://"+closed+"\n"))
	if err != nil {
		t.Fatal(err)
	}
	r := ProbeTargets(c, 750*time.Millisecond)
	if p := findTarget(r, "http"); p == nil || p.Status != ReachOK {
		t.Errorf("plain http slot on an open socket must be reachable: %+v", p)
	}
	g := findTarget(r, "http_targets.graph")
	if g == nil {
		t.Fatalf("the named entry must be probed and reported by name, got %+v", r.Targets)
	}
	if g.Status != ReachDown || !strings.Contains(g.Detail, "dial tcp") {
		t.Errorf("http_targets.graph on a closed port must be UNREACHABLE with the dial error, got %+v", g)
	}
	if r.AllReachable {
		t.Error("AllReachable must be false while a named entry is down")
	}
	warns := strings.Join(reachabilityWarnings(r), " | ")
	if !strings.Contains(warns, "http_targets.graph") || !strings.Contains(warns, "UNREACHABLE") {
		t.Errorf("the warning must name the entry: %q", warns)
	}
}
