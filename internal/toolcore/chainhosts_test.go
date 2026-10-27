package toolcore

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/config"
)

// A tester's catalogue: argus-config declares ONE
// target, http://pilot:8080, and 216 of 364 scenarios call two more engines by full URL in a chain step
// (199 pilot-old…:8080, 17 pilot-go…:8080). The executor's SUT probe dials only the config's targets
// (probeItems), so `sut_state: ready` said nothing about either — and nothing else looked.
func TestUnprobedChainHosts_ThreeEngines(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "argus-config.yaml")
	if err := os.WriteFile(cfgPath, []byte("project:\n  name: pilot\ntargets:\n  http:\n    base_url: http://pilot:8080\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sc := filepath.Join(dir, "scenarios")
	if err := os.Mkdir(sc, 0o700); err != nil {
		t.Fatal(err)
	}
	step := func(name, url string) string {
		return `{"type":"http","name":"` + name + `","method":"POST","url":"` + url + `","body":{}}`
	}
	const expect = "### Runnable\n- step a: status=200\n- step b: status=200\n"
	twin := func(id, second string) {
		writeChainScenario(t, sc, id, "HTTP Ingestion", `{"steps":[`+step("a", "${INGESTION_URL}/v1/run")+`,`+step("b", second)+`]}`, expect)
	}
	twin("DM-VDIFF-001", "http://pilot-old.pilot.svc.cluster.local:8080/v1/run")
	twin("DM-VDIFF-002", "http://pilot-old.pilot.svc.cluster.local:8080/v1/run")
	twin("DM-DIFF-001", "http://pilot-go.pilot.svc.cluster.local:8080/v1/run")
	twin("DM-SAME-001", "http://pilot:8080/v1/run")             // the configured target, by its own name: probed
	twin("DM-SAVED-001", "http://${saved.host}:8080/v1/run")    // a host known only at run time: not judged
	twin("DM-DEFAULT-001", "https://ext.example.org/v1/health") // no port: https's own 443

	c, err := config.ParseUnresolved(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	got := UnprobedChainHosts(c, sc)
	want := []UnprobedHost{
		{Addr: "ext.example.org:443", Scenarios: []string{"DM-DEFAULT-001"}},
		{Addr: "pilot-go.pilot.svc.cluster.local:8080", Scenarios: []string{"DM-DIFF-001"}},
		{Addr: "pilot-old.pilot.svc.cluster.local:8080", Scenarios: []string{"DM-VDIFF-001", "DM-VDIFF-002"}},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d hosts, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].Addr != want[i].Addr || len(got[i].Scenarios) != len(want[i].Scenarios) {
			t.Errorf("host %d = %+v, want %+v", i, got[i], want[i])
			continue
		}
		for j := range want[i].Scenarios {
			if got[i].Scenarios[j] != want[i].Scenarios[j] {
				t.Errorf("host %d scenarios = %v, want %v", i, got[i].Scenarios, want[i].Scenarios)
			}
		}
	}

	// Declaring the host as a named target is the documented way to have the executor probe it.
	if err := os.WriteFile(cfgPath, []byte("project:\n  name: pilot\ntargets:\n  http:\n    base_url: http://pilot:8080\n"+
		"  http_targets:\n    old:\n      base_url: http://pilot-old.pilot.svc.cluster.local:8080\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err = config.ParseUnresolved(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range UnprobedChainHosts(c, sc) {
		if h.Addr == "pilot-old.pilot.svc.cluster.local:8080" {
			t.Errorf("a host declared under targets.http_targets is still reported unprobed: %+v", h)
		}
	}
}

func TestUnprobedChainHosts_NothingToSay(t *testing.T) {
	c := &config.Config{}
	if got := UnprobedChainHosts(c, ""); len(got) != 0 {
		t.Errorf("no scenarios dir: got %+v", got)
	}
	if got := UnprobedChainHosts(c, filepath.Join(t.TempDir(), "absent")); len(got) != 0 {
		t.Errorf("absent dir: got %+v", got)
	}
}
