package config

import (
	"strings"
	"testing"
)

// dashlink_test.go -- (UI-2, A'1): observability.dashboard_link is decoded leniently (an
// older executor ignores it) and validated in full at load time, like every other block.

func dlBlock(tmpl, label string) string {
	s := "\nobservability:\n  dashboard_link:\n"
	if tmpl != "" {
		s += "    template: \"" + tmpl + "\"\n"
	}
	if label != "" {
		s += "    label: \"" + label + "\"\n"
	}
	return s
}

func TestDashboardLink_ValidParses(t *testing.T) {
	c, err := smLoad(t, dlBlock("https://grafana.lab.example/d/msgbus?var-run={run_id}&from={from}&to={to}", "Open in Grafana"))
	if err != nil {
		t.Fatalf("a valid block was refused: %v", err)
	}
	tmpl, label := c.DashboardLinkDecl()
	if tmpl != "https://grafana.lab.example/d/msgbus?var-run={run_id}&from={from}&to={to}" || label != "Open in Grafana" {
		t.Fatalf("decoded wrong: %q %q", tmpl, label)
	}
}

func TestDashboardLink_AbsentIsEmpty(t *testing.T) {
	c, err := smLoad(t, "")
	if err != nil {
		t.Fatal(err)
	}
	if tmpl, label := c.DashboardLinkDecl(); tmpl != "" || label != "" {
		t.Fatalf("no block must decode to empty: %q %q", tmpl, label)
	}
}

func TestDashboardLink_CoexistsWithGrafanaPublicURL(t *testing.T) {
	_, err := smLoad(t, "\nobservability:\n  grafana:\n    public_url: {compose: http://localhost:3000}\n  dashboard_link:\n    template: \"https://g.example/d/x?r={run_id}\"\n")
	if err != nil {
		t.Fatalf("the template and the per-tier Grafana base must be declarable together: %v", err)
	}
}

func TestDashboardLink_RefusalTexts(t *testing.T) {
	cases := []struct {
		name, tmpl, label string
		want              []string
	}{
		{"userinfo", "https://admin:hunter2@grafana.lab.example/d/x?run={run_id}", "",
			[]string{"observability.dashboard_link.template", "credential (userinfo)"}},
		{"unknown placeholder", "https://g.example/d/x?run={runid}", "",
			[]string{"observability.dashboard_link.template", "unknown placeholder {runid}"}},
		{"env var", "https://g.example/d/x?t=${TOKEN}", "", []string{"observability.dashboard_link.template", "${VAR}"}},
		{"not http", "ftp://g.example/x", "", []string{"absolute http(s) URL"}},
		{"label without template", "", "Open it", []string{"observability.dashboard_link.template is required"}},
		{"bad label", "https://g.example/x", strings.Repeat("x", 61), []string{"observability.dashboard_link.label"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := smLoad(t, dlBlock(c.tmpl, c.label))
			if err == nil {
				t.Fatal("a refusal was expected")
			}
			for _, w := range c.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("refusal %q lacks %q", err.Error(), w)
				}
			}
			if strings.Contains(err.Error(), "hunter2") {
				t.Errorf("the refusal echoes the password: %v", err)
			}
			t.Logf("REFUSAL (%s): %v", c.name, err)
		})
	}
}
