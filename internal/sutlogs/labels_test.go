package sutlogs

import (
	"os"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/k8srender"
	"gopkg.in/yaml.v3"
)

var jsonRules = Rules{
	Instance: "inst1", Project: "proj", Cluster: "k3d",
	Format: "json", LevelField: "level", CorrelationField: "correlation_id", SagaEventField: "event_type",
	SagaMarkers: []string{"saga"},
}

var logfmtRules = Rules{
	Instance: "inst1", Project: "proj", Cluster: "k3d",
	Format: "logfmt", LevelField: "severity", CorrelationField: "request_id", SagaEventField: "evt",
	SagaMarkers: []string{"tool_dispatch", "ghost_dispatch"},
}

func fixed(r Rules, container string) map[string]string {
	return map[string]string{"argus_instance": r.Instance, "project": r.Project, "cluster": r.Cluster, "service": container}
}

func with(m map[string]string, kv ...string) map[string]string {
	out := map[string]string{}
	for k, v := range m {
		out[k] = v
	}
	for i := 0; i < len(kv); i += 2 {
		out[kv[i]] = kv[i+1]
	}
	return out
}

func eq(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// The table is the rule: each row is one clause of the promtail pipeline (quoted in TestLabels_...Pipeline).
func TestLabels_Table(t *testing.T) {
	cases := []struct {
		name  string
		rules Rules
		line  string
		want  map[string]string
	}{
		{"json: level only", jsonRules, `{"level":"info","msg":"x"}`, with(fixed(jsonRules, "api"), "level", "info")},
		{"json: tr- id is labelled", jsonRules, `{"level":"warn","correlation_id":"tr-run1-sc-abcd1234"}`,
			with(fixed(jsonRules, "api"), "level", "warn", "correlation_id", "tr-run1-sc-abcd1234")},
		{"json: a SUT's own uuid is NOT a label", jsonRules, `{"level":"info","correlation_id":"3f2c-uuid"}`, with(fixed(jsonRules, "api"), "level", "info")},
		{"json: xtr- is not tr-", jsonRules, `{"correlation_id":"xtr-abc"}`, fixed(jsonRules, "api")},
		{"json: empty correlation id is omitted", jsonRules, `{"level":"info","correlation_id":""}`, with(fixed(jsonRules, "api"), "level", "info")},
		{"json: saga marker is normalised to saga", jsonRules, `{"event_type":"saga","level":"info"}`, with(fixed(jsonRules, "api"), "level", "info", "event_type", "saga")},
		{"json: another marker value passes through", jsonRules, `{"event_type":"request"}`, with(fixed(jsonRules, "api"), "event_type", "request")},
		{"json: no marker -> no event_type", jsonRules, `{"level":"info"}`, with(fixed(jsonRules, "api"), "level", "info")},
		{"json: empty level is omitted", jsonRules, `{"level":""}`, fixed(jsonRules, "api")},
		{"json: a numeric level is its digits", jsonRules, `{"level":30}`, with(fixed(jsonRules, "api"), "level", "30")},
		{"json: nested path", Rules{Instance: "i", Project: "p", Cluster: "c", Format: "json", LevelField: "log.level", CorrelationField: "c", SagaEventField: "e"},
			`{"log":{"level":"error"}}`, map[string]string{"argus_instance": "i", "project": "p", "cluster": "c", "service": "api", "level": "error"}},
		{"json: unparsable line still ships with the four fixed labels", jsonRules, `plain text, not json`, fixed(jsonRules, "api")},
		{"json: a json array is not an object", jsonRules, `[1,2]`, fixed(jsonRules, "api")},
		{"logfmt: level, tr- id, saga", logfmtRules, `severity=info request_id=tr-r-s-12345678 evt=tool_dispatch msg="hello world"`,
			with(fixed(logfmtRules, "worker"), "level", "info", "correlation_id", "tr-r-s-12345678", "event_type", "saga")},
		{"logfmt: second saga marker", logfmtRules, `evt=ghost_dispatch`, with(fixed(logfmtRules, "worker"), "event_type", "saga")},
		{"logfmt: another marker passes through", logfmtRules, `evt=other severity=warn`, with(fixed(logfmtRules, "worker"), "level", "warn", "event_type", "other")},
		{"logfmt: quoted value", logfmtRules, `severity="a b" msg=x`, with(fixed(logfmtRules, "worker"), "level", "a b")},
		{"logfmt: non-tr id omitted", logfmtRules, `request_id=abc-123`, fixed(logfmtRules, "worker")},
		{"logfmt: a syntax error extracts nothing", logfmtRules, `severity=info a=b=c`, fixed(logfmtRules, "worker")},
		{"logfmt: json line under a logfmt SUT extracts nothing", logfmtRules, `{"severity":"info"}`, fixed(logfmtRules, "worker")},
	}
	for _, c := range cases {
		container := "api"
		if c.rules.Format == "logfmt" {
			container = "worker"
		}
		if got := Labels(c.rules, container, c.line); !eq(got, c.want) {
			t.Errorf("%s:\n got  %v\n want %v", c.name, got, c.want)
		}
	}
}

// No label that would multiply streams is EVER present, whatever the line.
func TestLabels_NeverCarriesAForbiddenLabel(t *testing.T) {
	lines := []string{`{"level":"info","pod":"p","namespace":"n","container":"c","job":"j","filename":"f","stream":"stdout"}`,
		`pod=p namespace=n job=argus-runner filename=f stream=stderr container=c`, `{}`, ``, `x`}
	allowed := map[string]bool{"argus_instance": true, "project": true, "cluster": true, "service": true, "level": true, "correlation_id": true, "event_type": true}
	for _, r := range []Rules{jsonRules, logfmtRules} {
		for _, l := range lines {
			for k := range Labels(r, "c1", l) {
				if !allowed[k] {
					t.Errorf("forbidden label %q for %q", k, l)
				}
			}
		}
	}
	for _, bad := range []string{"pod", "namespace", "container", "job", "filename", "stream"} {
		if _, has := Labels(jsonRules, "c1", `{"level":"info"}`)[bad]; has {
			t.Errorf("label %q present", bad)
		}
	}
}

// The service label is the CONTAINER name; two pods of one container share a stream key.
func TestLabels_ServiceIsTheContainerName(t *testing.T) {
	if got := Labels(jsonRules, "sidecar", `{}`)["service"]; got != "sidecar" {
		t.Errorf("service = %q", got)
	}
}

// ── held equal to the promtail pipeline ───────────────────────────────────────────────────────────

// TestLabels_AreHeldEqualToThePromtailPipeline renders the promtail ConfigMap `--sut-logs-via node`
// produces for a config and checks, clause by clause, that Labels implements what that pipeline does. The
// clauses it quotes (from k8srender.renderBundledPromtailBlock / obsconfig.CorrelationLabelStage):
//
//	labels:    argus_instance: <id>, project: <ProjectLabel()>, cluster: <cluster>
//	regex:     expression: '/[^/]+/(?P<service>[^/]+)/[0-9]+\.log$'      (service = the container dir)
//	json:      expressions: {level: <LevelField>, correlation_id: <CorrelationField>, saga_marker: <SagaEventField>}
//	template:  '{{ if hasPrefix "tr-" .Value }}{{ .Value }}{{ end }}'    (source: correlation_id)
//	template:  '{{ if <cond> }}saga{{ else }}{{ .saga_marker | default "" }}{{ end }}'   (source: event_type)
//	labels:    level, correlation_id, event_type
func TestLabels_AreHeldEqualToThePromtailPipeline(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		rules      Rules
	}{
		{"json default", "project: {name: memstore}\nobservability:\n  loki:\n    url: http://loki:3100\n",
			Rules{Instance: "memstore-k3d", Project: "memstore", Cluster: "k3d", Format: "json", LevelField: "level", CorrelationField: "correlation_id", SagaEventField: "event_type", SagaMarkers: []string{"saga"}}},
		{"logfmt, two markers", "project: {name: social}\nobservability:\n  loki:\n    url: http://loki:3100\n    log_format: logfmt\n    level_field: severity\n    correlation_field: request_id\n    saga_event_field: evt\n    saga_event_values: [tool_dispatch, ghost_dispatch]\n",
			Rules{Instance: "memstore-k3d", Project: "social", Cluster: "k3d", Format: "logfmt", LevelField: "severity", CorrelationField: "request_id", SagaEventField: "evt", SagaMarkers: []string{"tool_dispatch", "ghost_dispatch"}}},
	} {
		c := loadCfg(t, tc.body)
		// the Rules the collector builds from the same config
		got := Rules{Instance: "memstore-k3d", Project: c.ProjectLabel(), Cluster: "k3d", Format: c.LogFormat(), LevelField: c.LevelField(),
			CorrelationField: c.CorrelationField(), SagaEventField: c.SagaEventField(), SagaMarkers: c.SagaEventValues()}
		if strings.Join(got.SagaMarkers, ",") != strings.Join(tc.rules.SagaMarkers, ",") || got.Format != tc.rules.Format || got.Project != tc.rules.Project {
			t.Fatalf("%s: rules from config = %+v, want %+v", tc.name, got, tc.rules)
		}
		in := k8srender.Instance{ID: "memstore-k3d", SUTNamespace: "sut", Image: "img", CPURL: "http://cp", ArgusConfig: "project: x\n", Tier: "k3d", Cluster: "k3d", CollectSUTLogs: true}
		obs, err := k8srender.RenderObs(in, c)
		if err != nil {
			t.Fatal(err)
		}
		cfgYML := promtailConfig(t, obs)
		var pc struct {
			Scrape []struct {
				Static []struct {
					Labels map[string]string `yaml:"labels"`
				} `yaml:"static_configs"`
				Stages []map[string]any `yaml:"pipeline_stages"`
			} `yaml:"scrape_configs"`
		}
		if err := yaml.Unmarshal([]byte(cfgYML), &pc); err != nil {
			t.Fatal(err)
		}
		sc := pc.Scrape[0]
		static := sc.Static[0].Labels
		for k, v := range map[string]string{"argus_instance": got.Instance, "project": got.Project, "cluster": got.Cluster} {
			if static[k] != v {
				t.Errorf("%s: promtail static label %s = %q, collector uses %q", tc.name, k, static[k], v)
			}
		}
		var sawParse, sawCorr, sawEvent bool
		for _, st := range sc.Stages {
			if p, ok := st[got.Format].(map[string]any); ok {
				sawParse = true
				m, _ := p["expressions"].(map[string]any)
				if got.Format == "logfmt" {
					m, _ = p["mapping"].(map[string]any)
				}
				for k, want := range map[string]string{"level": got.LevelField, "correlation_id": got.CorrelationField, "saga_marker": got.SagaEventField} {
					if m[k] != want {
						t.Errorf("%s: promtail extracts %s from %v, collector reads %q", tc.name, k, m[k], want)
					}
				}
			}
			if tpl, ok := st["template"].(map[string]any); ok {
				switch tpl["source"] {
				case "correlation_id":
					sawCorr = true
					if tpl["template"] != `{{ if hasPrefix "tr-" .Value }}{{ .Value }}{{ end }}` {
						t.Errorf("%s: the correlation template changed: %v", tc.name, tpl["template"])
					}
				case "event_type":
					sawEvent = true
					cond := ""
					for i, m := range got.SagaMarkers {
						_ = i
						cond += `(eq .saga_marker "` + m + `")`
					}
					tmpl, _ := tpl["template"].(string)
					for _, m := range got.SagaMarkers {
						if !strings.Contains(tmpl, `eq .saga_marker "`+m+`"`) {
							t.Errorf("%s: promtail's saga condition does not name marker %q: %s", tc.name, m, tmpl)
						}
					}
					if !strings.HasSuffix(tmpl, `}}saga{{ else }}{{ .saga_marker | default "" }}{{ end }}`) {
						t.Errorf("%s: the event_type template changed: %s", tc.name, tmpl)
					}
					_ = cond
				}
			}
		}
		if !sawParse || !sawCorr || !sawEvent {
			t.Errorf("%s: pipeline stages missing (parse %v correlation %v event %v)", tc.name, sawParse, sawCorr, sawEvent)
		}
		// and the service regex: the container directory name
		if !strings.Contains(cfgYML, `expression: '/[^/]+/(?P<service>[^/]+)/[0-9]+\.log$'`) {
			t.Errorf("%s: the service regex changed", tc.name)
		}
	}
}

func loadCfg(t *testing.T, body string) *config.Config {
	t.Helper()
	p := t.TempDir() + "/c.yaml"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := config.ParseUnresolved(p)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// promtailConfig extracts the config.yml of the promtail-config ConfigMap from a rendered obs.yaml.
func promtailConfig(t *testing.T, obs string) string {
	t.Helper()
	dec := yaml.NewDecoder(strings.NewReader(obs))
	for {
		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			break
		}
		if m["kind"] == "ConfigMap" {
			md, _ := m["metadata"].(map[string]any)
			if md["name"] == "promtail-config" {
				return m["data"].(map[string]any)["config.yml"].(string)
			}
		}
	}
	t.Fatal("no promtail-config ConfigMap in the rendered obs.yaml")
	return ""
}
