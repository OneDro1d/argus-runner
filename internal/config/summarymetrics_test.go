package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const smBase = `project: { name: msgbus }
targets:
  http: { base_url: "http://msgbus.lab:8080" }
`

func smLoad(t *testing.T, extra string) (*Config, error) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "argus-config.yaml")
	if err := os.WriteFile(p, []byte(smBase+extra), 0o600); err != nil {
		t.Fatal(err)
	}
	return Load(p)
}

const smValid = `
summary_metrics:
  every: 2m
  source: { type: prometheus, url: "http://prometheus.msgbus-lab:9090" }
  readings:
    - { name: agents_connected, target: live, unit: agents, query: 'sum(msgbus_agents_online)', comfortable_limit: 250 }
    - { name: messages_per_second, unit: msg/s, query: 'sum(rate(msgbus_delivered_total[5m]))' }
`

func TestSummaryMetrics_ValidParses(t *testing.T) {
	c, err := smLoad(t, smValid)
	if err != nil {
		t.Fatalf("a valid block was refused: %v", err)
	}
	sm := c.SummaryMetricsDecl()
	if sm == nil {
		t.Fatal("SummaryMetricsDecl() = nil for a declared block")
	}
	if sm.Source.Type != "prometheus" || sm.Source.URL != "http://prometheus.msgbus-lab:9090" || len(sm.Readings) != 2 {
		t.Fatalf("decoded wrong: %+v", sm)
	}
	if got := sm.EveryDuration(); got != 2*time.Minute {
		t.Errorf("every = %v, want 2m", got)
	}
	r := sm.Readings[0]
	if r.Name != "agents_connected" || r.Target != "live" || r.Unit != "agents" || r.ComfortableLimit == nil || *r.ComfortableLimit != 250 {
		t.Errorf("reading decoded wrong: %+v", r)
	}
}

func TestSummaryMetrics_DefaultEveryIsFiveMinutes(t *testing.T) {
	c, err := smLoad(t, `
summary_metrics:
  source: { type: metrics_endpoint, url: "http://x:9100/metrics" }
  readings:
    - { name: up_total, unit: n, query: 'process_up{job="a"}' }
`)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.SummaryMetricsDecl().EveryDuration(); got != 5*time.Minute {
		t.Errorf("default every = %v, want 5m", got)
	}
}

func TestSummaryMetrics_AbsentIsToday(t *testing.T) {
	c, err := smLoad(t, "")
	if err != nil {
		t.Fatal(err)
	}
	if c.SummaryMetricsDecl() != nil {
		t.Error("an absent block must decode to nil (today's behaviour exactly)")
	}
}

func TestSummaryMetrics_InvalidIsRefusedByName(t *testing.T) {
	many := "summary_metrics:\n  source: { type: prometheus, url: \"http://p:9090\" }\n  readings:\n"
	for i := 0; i < 13; i++ {
		many += "    - { name: r" + string(rune('a'+i)) + ", query: up }\n"
	}
	one := func(extra string) string {
		return "summary_metrics:\n  source: { type: prometheus, url: \"http://p:9090\" }\n  readings:\n    - { name: ok, query: up" + extra + " }\n"
	}
	cases := []struct{ name, yaml, want string }{
		{"every too small", "summary_metrics:\n  every: 30s\n  source: { type: prometheus, url: \"http://p:9090\" }\n  readings:\n    - { name: ok, query: up }\n", "every"},
		{"every too large", "summary_metrics:\n  every: 2h\n  source: { type: prometheus, url: \"http://p:9090\" }\n  readings:\n    - { name: ok, query: up }\n", "every"},
		{"every not a duration", "summary_metrics:\n  every: often\n  source: { type: prometheus, url: \"http://p:9090\" }\n  readings:\n    - { name: ok, query: up }\n", "every"},
		{"thirteen readings", many, "12"},
		{"no readings", "summary_metrics:\n  source: { type: prometheus, url: \"http://p:9090\" }\n  readings: []\n", "readings"},
		{"bad name", "summary_metrics:\n  source: { type: prometheus, url: \"http://p:9090\" }\n  readings:\n    - { name: Agents-Online, query: up }\n", "name"},
		{"duplicate name", "summary_metrics:\n  source: { type: prometheus, url: \"http://p:9090\" }\n  readings:\n    - { name: a, query: up }\n    - { name: a, query: up }\n", "duplicate"},
		{"unknown source type", "summary_metrics:\n  source: { type: statsd, url: \"http://p:9090\" }\n  readings:\n    - { name: a, query: up }\n", "type"},
		{"missing url", "summary_metrics:\n  source: { type: prometheus }\n  readings:\n    - { name: a, query: up }\n", "url"},
		{"non-http url", "summary_metrics:\n  source: { type: prometheus, url: \"file:///etc/passwd\" }\n  readings:\n    - { name: a, query: up }\n", "url"},
		{"userinfo in url", "summary_metrics:\n  source: { type: prometheus, url: \"http://admin:pw@p:9090\" }\n  readings:\n    - { name: a, query: up }\n", "credential"},
		{"token in url query", "summary_metrics:\n  source: { type: prometheus, url: \"http://p:9090/?token=abc\" }\n  readings:\n    - { name: a, query: up }\n", "credential"},
		{"literal credential", "summary_metrics:\n  source: { type: prometheus, url: \"http://p:9090\", credential: \"admin:hunter2\" }\n  readings:\n    - { name: a, query: up }\n", "${VAR}"},
		{"empty query", "summary_metrics:\n  source: { type: prometheus, url: \"http://p:9090\" }\n  readings:\n    - { name: a, query: \"\" }\n", "query"},
		{"typo in key", one(", querry: x"), "querry"},
		{"series with function on /metrics", "summary_metrics:\n  source: { type: metrics_endpoint, url: \"http://x:9100/metrics\" }\n  readings:\n    - { name: a, query: 'sum(foo)' }\n", "series"},
		{"comfortable_limit not a number", one(", comfortable_limit: lots"), "lots"},
	}
	for _, c := range cases {
		_, err := smLoad(t, c.yaml)
		if err == nil {
			t.Errorf("%s: was ACCEPTED, want a refusal naming %q", c.name, c.want)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: refusal %q does not name %q", c.name, err, c.want)
		}
	}
}

// The credential is a ${VAR} reference resolved like observability.betterstack.credential: an unset var
// fails loud, and a resolved value never appears in an error.
func TestSummaryMetrics_CredentialIsAVarReference(t *testing.T) {
	y := `
summary_metrics:
  source: { type: prometheus, url: "http://p:9090", credential: "${PROM_CRED}" }
  readings:
    - { name: a, query: up }
`
	t.Setenv("PROM_CRED", "")
	if _, err := smLoad(t, y); err == nil || !strings.Contains(err.Error(), "PROM_CRED") {
		t.Fatalf("an unset ${PROM_CRED} must fail loud naming the variable, got %v", err)
	}
	t.Setenv("PROM_CRED", "svc:s3cret")
	c, err := smLoad(t, y)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.SummaryMetricsDecl().Source.Credential; got != "svc:s3cret" {
		t.Errorf("credential not resolved: %q", got)
	}
}

// ⛔ The new key is TOP-LEVEL. An old executor decodes `targets` strictly and everything else leniently,
// so a config carrying summary_metrics must still pass the strict-targets pass an old binary runs.
func TestSummaryMetrics_IsNotUnderTargetsAndSurvivesStrictTargets(t *testing.T) {
	p := filepath.Join(t.TempDir(), "argus-config.yaml")
	if err := os.WriteFile(p, []byte(smBase+smValid), 0o600); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if err := strictTargets(b); err != nil {
		t.Fatalf("the strict targets pass (what an OLD executor runs) refuses a config with summary_metrics: %v", err)
	}
	under := smBase + "  summary_metrics: { every: 5m }\n"
	if err := strictTargets([]byte(under)); err == nil {
		t.Fatal("control failed: a key under targets was accepted, so this test cannot show why the new key is top-level")
	}
}
