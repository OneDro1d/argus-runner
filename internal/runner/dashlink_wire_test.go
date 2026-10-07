package runner

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/federation"
)

// (UI-2): the executor reports observability.dashboard_link on register and on EVERY poll,
// three-valued: nil = not reported (no path / config failed to load), "" = loadable config with no block.

func TestDashboardLinkFor_Declared(t *testing.T) {
	p := ttCfg(t, "observability:\n  dashboard_link:\n    template: \"https://g.example/d/x?r={run_id}\"\n    label: \"Open in Grafana\"\n")
	tmpl, label := dashboardLinkFor(p)
	if tmpl == nil || *tmpl != "https://g.example/d/x?r={run_id}" || label == nil || *label != "Open in Grafana" {
		t.Fatalf("dashboardLinkFor = %v %v", tmpl, label)
	}
}

func TestDashboardLinkFor_NoBlockIsReportedNone(t *testing.T) {
	tmpl, label := dashboardLinkFor(ttCfg(t, ""))
	if tmpl == nil || *tmpl != "" || label == nil || *label != "" {
		t.Fatalf("a loadable config without the block must report EXPLICIT empty (clears a removed template): %v %v", tmpl, label)
	}
}

func TestDashboardLinkFor_ConfigFailsToLoadIsNotReported(t *testing.T) {
	p := ttCfg(t, "observability:\n  dashboard_link:\n    template: \"https://u:p@g.example/\"\n")
	if tmpl, label := dashboardLinkFor(p); tmpl != nil || label != nil {
		t.Fatalf("a config that fails to load must report nothing: %v %v", tmpl, label)
	}
	if tmpl, label := dashboardLinkFor(""); tmpl != nil || label != nil {
		t.Fatalf("no path must report nothing: %v %v", tmpl, label)
	}
}

func TestRequestsCarryTheDashboardLink(t *testing.T) {
	a, b := "https://g.example/{run_id}", "Open"
	reg := registerRequestFor(FedConfig{InstanceID: "i1"}, "pub", "", nil, nil, nil, withRegisterDashboardLink(&a, &b))
	if reg.DashboardLinkTemplate == nil || *reg.DashboardLinkTemplate != a || *reg.DashboardLinkLabel != b {
		t.Fatalf("register lost it: %+v", reg)
	}
	poll := pollRequestFor("0.3.50", nil, nil, nil, nil, nil, federation.SUTObservation{}, nil, nil, withPollDashboardLink(&a, &b))
	if poll.DashboardLinkTemplate == nil || *poll.DashboardLinkTemplate != a || *poll.DashboardLinkLabel != b {
		t.Fatalf("poll lost it: %+v", poll)
	}
}

func TestRequests_WithoutTheOptionOmitTheDashboardLink(t *testing.T) {
	reg, _ := json.Marshal(registerRequestFor(FedConfig{InstanceID: "i1"}, "pub", "", nil, nil, nil))
	poll, _ := json.Marshal(pollRequestFor("0.3.50", nil, nil, nil, nil, nil, federation.SUTObservation{}, nil, nil))
	for name, raw := range map[string][]byte{"register": reg, "poll": poll} {
		if strings.Contains(string(raw), "dashboard_link") {
			t.Errorf("%s payload carries dashboard_link with no report: %s", name, raw)
		}
	}
}

// Wiring: a helper nothing calls reports nothing and every instance would stay NULL (money_handling lesson).
func TestDashboardLinkIsWiredIntoRegisterAndTheLoop(t *testing.T) {
	for file, needle := range map[string]string{
		"runner.go":   "withRegisterDashboardLink(dashboardLinkFor(s.fed.Exec.ConfigPath))",
		"executor.go": "withPollDashboardLink(dashboardLinkFor(e.ConfigPath))",
	} {
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), needle) {
			t.Errorf("%s does not contain %q -- the template would never leave the executor", file, needle)
		}
	}
}
