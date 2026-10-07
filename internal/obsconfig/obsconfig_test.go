package obsconfig

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
	"text/template"

	"github.com/OneDro1d/argus-runner/internal/config"
	"gopkg.in/yaml.v3"
)

// RO-05/07/08: the obs stack must be rendered PER SUT from argus-config.yaml, never
// hardcoded to order-service. These guard the rendered promtail + prometheus configs.

func cfg(t *testing.T, body string) *config.Config {
	t.Helper()
	var c config.Config
	if err := yaml.Unmarshal([]byte(body), &c); err != nil {
		t.Fatal(err)
	}
	return &c
}

func mustParseYAML(t *testing.T, s string) {
	t.Helper()
	var out any
	if err := yaml.Unmarshal([]byte(s), &out); err != nil {
		t.Fatalf("rendered config is not valid YAML: %v\n---\n%s", err, s)
	}
}

func TestRenderPromtail_DeriveProjectAndLevel(t *testing.T) {
	c := cfg(t, `
project: {name: my-service}
deploy: {compose_project: my-sut, network: my-sut_default}
observability:
  loki: {url: "http://loki:3100", level_field: lvl, derive_project_from_container: true}
`)
	got := RenderPromtail(c)
	mustParseYAML(t, got)

	if !strings.Contains(got, "argus|my-sut") {
		t.Errorf("keep regex must scope the SUT project (argus|my-sut); got:\n%s", got)
	}
	if strings.Contains(got, "order-service") {
		t.Errorf("rendered promtail must NOT hardcode order-service:\n%s", got)
	}
	// Patch #4: `project` is labelled with the config Project.Name (my-service) — the canonical
	// SUT identity matching the metric project + the dashboard var (NOT the compose-project, which
	// broke per-SUT log scoping). derive_project_from_container is superseded.
	if !strings.Contains(got, "replacement: my-service") {
		t.Errorf("project label must be the config Project.Name (my-service); got:\n%s", got)
	}
	if !strings.Contains(got, "target_label: project") {
		t.Error("must relabel a project stream label")
	}
	// RO-08: a json pipeline stage extracting the SUT's structured level into `level`.
	if !strings.Contains(got, "pipeline_stages") || !strings.Contains(got, "level: lvl") {
		t.Errorf("must extract level from the SUT's level field (lvl):\n%s", got)
	}
}

// Round 2: promtail must extract the SUT's propagated correlation id into a Loki LABEL
// `correlation_id` (from the SUT's declared field — correlation_id by default, request_id for
// Social) so the dashboard's correlation-id combo dropdown can list values.
func TestRenderPromtail_ExtractsCorrelationId(t *testing.T) {
	// default field
	got := RenderPromtail(cfg(t, "project: {name: my-service}\n"))
	mustParseYAML(t, got)
	if !strings.Contains(got, "correlation_id: correlation_id") {
		t.Errorf("must extract the default correlation_id field into the label; got:\n%s", got)
	}
	if i := strings.Index(got, "- labels:"); i < 0 || !strings.Contains(got[i:], "correlation_id:") {
		t.Errorf("correlation_id must be promoted to a Loki label (under labels:); got:\n%s", got)
	}
	// SUT-declared field: Social logs request_id -> extracted INTO the correlation_id label.
	got2 := RenderPromtail(cfg(t, "project: {name: social}\nobservability:\n  loki: {correlation_field: request_id}\n"))
	mustParseYAML(t, got2)
	if !strings.Contains(got2, "correlation_id: request_id") {
		t.Errorf("must extract the SUT's declared correlation field (request_id) into the correlation_id label; got:\n%s", got2)
	}
}

// promtail must NORMALIZE each SUT's saga marker to the CANONICAL event_type="saga" label so the SHARED
// dashboard filters event_type=~"saga" for EVERY SUT: extract the SUT's saga_event_field into saga_marker,
// then a template maps its saga_event_value -> "saga" (other values pass through). This is what makes the
// obs stack generic — a SUT can use any field/value; only the shared normalized label reaches the dashboard.
func TestRenderPromtail_NormalizesSagaMarker(t *testing.T) {
	// default (OrderService-style): saga_event_field=event_type, saga_event_value=saga
	got := RenderPromtail(cfg(t, "project: {name: my-service}\n"))
	mustParseYAML(t, got)
	if !strings.Contains(got, "saga_marker: event_type") {
		t.Errorf("must extract the default saga field (event_type) into saga_marker; got:\n%s", got)
	}
	if !strings.Contains(got, `{{ if eq .saga_marker "saga" }}saga{{ else }}{{ .saga_marker | default "" }}{{ end }}`) {
		t.Errorf("must normalize the saga marker to canonical event_type=saga, defaulting a MISSING field to empty (not the <no value> literal); got:\n%s", got)
	}
	// U9: the else branch must not emit the raw {{ .saga_marker }}, which renders "<no value>" on
	// lines with no saga field and shows up as a spurious event_type bucket on the dashboard.
	if strings.Contains(got, `{{ else }}{{ .saga_marker }}{{ end }}`) {
		t.Errorf("the bare {{ .saga_marker }} else branch renders <no value> for non-saga lines; must be | default \"\"")
	}
	if i := strings.Index(got, "- labels:"); i < 0 || !strings.Contains(got[i:], "event_type:") {
		t.Errorf("event_type must be promoted to a Loki label (under labels:); got:\n%s", got)
	}
	// NON-STANDARD SUT (Social): saga_event_field=event, saga_event_value=tool_dispatch -> normalized to "saga".
	got2 := RenderPromtail(cfg(t, "project: {name: social}\nobservability:\n  loki: {saga_event_field: event, saga_event_value: tool_dispatch}\n"))
	mustParseYAML(t, got2)
	if !strings.Contains(got2, "saga_marker: event") {
		t.Errorf("must extract the SUT's saga field (event) into saga_marker; got:\n%s", got2)
	}
	if !strings.Contains(got2, `{{ if eq .saga_marker "tool_dispatch" }}saga`) {
		t.Errorf("must normalize the SUT's non-standard saga value (tool_dispatch) to canonical saga; got:\n%s", got2)
	}
}

// The Saga panel filters the CANONICAL event_type=~"saga" for EVERY SUT — promtail normalizes each SUT's
// saga marker to "saga" at ingestion, so RenderDashboard must NOT substitute a per-SUT saga value into the
// dashboard (the old behavior; one shared dashboard could hold only the last-onboarded SUT's value).
func TestRenderDashboard_NoSagaSubstitution(t *testing.T) {
	c := cfg(t, "observability:\n  loki: {saga_event_value: tool_dispatch}\n")
	in := []byte(`{"expr":"{event_type=~\"saga\"}"}`)
	out := RenderDashboard(in, c)
	if string(out) != string(in) {
		t.Errorf("RenderDashboard must NOT rewrite the saga matcher (normalization is at ingestion, promtail); got:\n%s", out)
	}
	if strings.Contains(string(out), `event_type=~\"tool_dispatch\"`) {
		t.Error("the dashboard saga matcher must stay canonical event_type=~\"saga\", never a per-SUT value")
	}
}

func TestRenderPromtail_StaticProjectLabel(t *testing.T) {
	c := cfg(t, `
project: {name: my-service}
deploy: {compose_project: my-sut}
observability:
  loki: {project_label: pinned-proj}
`)
	got := RenderPromtail(c)
	mustParseYAML(t, got)
	if !strings.Contains(got, "replacement: pinned-proj") {
		t.Errorf("static project_label must render as a replacement; got:\n%s", got)
	}
	// default level field when unset
	if !strings.Contains(got, "level: level") {
		t.Errorf("default level field should be `level`; got:\n%s", got)
	}
}

func TestRenderPrometheus_ExplicitTargets(t *testing.T) {
	c := cfg(t, `
project: {name: my-service}
observability:
  prometheus: {targets: ["my-api:9090", "my-worker:9090"]}
`)
	got := RenderPrometheus(c)
	mustParseYAML(t, got)
	if !strings.Contains(got, "my-api:9090") || !strings.Contains(got, "my-worker:9090") {
		t.Errorf("must scrape the SUT targets; got:\n%s", got)
	}
	if strings.Contains(got, "order-api:9090") {
		t.Errorf("must NOT hardcode order-api; got:\n%s", got)
	}
	if !strings.Contains(got, "project: my-service") {
		t.Errorf("external project label must come from the config; got:\n%s", got)
	}
	if !strings.Contains(got, "pushgateway:9091") {
		t.Error("must keep scraping the runner pushgateway (argus_* metrics)")
	}
}

// RO-08: the errors-view level matcher must be PARAMETERIZED, not a dead config key. The
// dashboard's log_view "Errors + warnings" value is substituted from c.ErrorMatch() at onboard.
func TestRenderDashboard_SubstitutesErrorMatch(t *testing.T) {
	c := cfg(t, "observability:\n  loki: {error_match: \"(?i)error|warn|fatal\"}\n")
	in := []byte(`{"name":"log_view","options":[{"text":"Errors + warnings","value":", level=~\"(?i)error|warn\""}]}`)
	out := RenderDashboard(in, c)
	if !strings.Contains(string(out), `level=~\"(?i)error|warn|fatal\"`) {
		t.Errorf("must substitute the COLLECTed matcher; got:\n%s", out)
	}
	var v any
	if err := yaml.Unmarshal(out, &v); err != nil { // JSON is valid YAML — cheap validity check
		t.Fatalf("rendered dashboard is not valid: %v", err)
	}
}

func TestRenderDashboard_DefaultIsNoOp(t *testing.T) {
	c := cfg(t, "project: {name: x}\n") // no error_match → default (?i)error|warn
	in := []byte(`{"value":", level=~\"(?i)error|warn\""}`)
	if string(RenderDashboard(in, c)) != string(in) {
		t.Error("with the default matcher, RenderDashboard must be a no-op")
	}
}

// (request_rate_query substitution retired — the request/error-rate panel is now
// runner-owned (argus_sut_requests_total), so RenderDashboard no longer swaps a SUT PromQL.
// The remaining RenderDashboard tests cover the still-live ErrorMatch substitution.)

func TestRenderPrometheus_DiscoverByProject(t *testing.T) {
	c := cfg(t, `
project: {name: my-service}
deploy: {compose_project: my-sut}
observability:
  prometheus: {discover_by_project: true}
`)
	got := RenderPrometheus(c)
	mustParseYAML(t, got)
	if !strings.Contains(got, "docker_sd_configs") {
		t.Errorf("discover_by_project must use Docker SD; got:\n%s", got)
	}
	if !strings.Contains(got, "argus|my-sut") {
		t.Errorf("Docker SD must keep on the SUT project; got:\n%s", got)
	}
	// must keep ONLY the :9090 metrics port — else SD scrapes every exposed port (amqp/db/...) → noisy 'down' targets
	if !strings.Contains(got, "__meta_docker_port_private") {
		t.Errorf("Docker SD must keep only the :9090 metrics port; got:\n%s", got)
	}
}

// TestRenderObs_ArgusInstanceLabel guards the fix for the "No data" saga/logs panels: when onboarded to a
// control plane the SUT's promtail + prometheus obs must carry argus_instance=<registered id> (the value
// the dashboard filters by), NOT the hardcoded "local". A standalone onboard (no id) stays "local".
func TestRenderObs_ArgusInstanceLabel(t *testing.T) {
	c := cfg(t, `
project: {name: my-service}
deploy: {compose_project: my-sut, network: my-sut_default}
observability:
  loki: {url: "http://loki:3100"}
`)
	// registered id → promtail + prometheus both carry it
	pt := RenderPromtail(c, "orderservice-compose")
	mustParseYAML(t, pt)
	if !strings.Contains(pt, "argus_instance") || !strings.Contains(pt, "replacement: orderservice-compose") {
		t.Errorf("promtail argus_instance not set to the registered id:\n%s", pt)
	}
	pm := RenderPrometheus(c, "orderservice-compose")
	mustParseYAML(t, pm)
	if !strings.Contains(pm, "orderservice-compose") {
		t.Errorf("prometheus argus_instance not set to the registered id:\n%s", pm)
	}
	// standalone (no id) → defaults to "local"
	def := RenderPromtail(c)
	if strings.Contains(def, "replacement: orderservice-compose") {
		t.Error("promtail without an id must default argus_instance to local")
	}
	if !strings.Contains(def, "replacement: local") {
		t.Error("promtail default argus_instance should be local")
	}
}

// TestDashboardSource_SagaPanelCanonicalDefault guards the RenderDashboard contract: any event_type=~
// matcher left in the SOURCE dashboard must be the canonical "saga", because RenderDashboard SUBSTITUTES
// that literal per-SUT (saga_event_value). A hardcoded value (e.g. the "tool_dispatch" regression that
// shipped baked in the image and broke every SUT's saga panel) makes the substitution a no-op. MCP SUTs
// whose sagas are tool_dispatch must declare saga_event_value in THEIR argus-config, not edit the source.
//
// EN-7 (PR #113) renamed the "Saga timeline" panel to "Request timeline (by correlation_id)" and dropped its
// event_type filter altogether: a SUT that never emits event_type=saga (Memstore) was permanently blank there.
// So the timeline panel must now carry NO event_type matcher at all.
func TestDashboardSource_SagaPanelCanonicalDefault(t *testing.T) {
	b, err := os.ReadFile("../../deploy/compose/grafana/dashboards/argus-overview.json")
	if err != nil {
		t.Skipf("dashboard source not found: %v", err)
	}
	s := string(b)
	for _, m := range regexp.MustCompile(`event_type=~\\"([^"\\]*)\\"`).FindAllStringSubmatch(s, -1) {
		if m[1] != "saga" {
			t.Errorf(`source dashboard hardcodes event_type=~%q — only the canonical "saga" may appear (RenderDashboard substitutes it per-SUT); declare saga_event_value in the SUT's argus-config instead`, m[1])
		}
	}
	var dash struct {
		Panels []struct {
			Title   string `json:"title"`
			Targets []struct {
				Expr string `json:"expr"`
			} `json:"targets"`
		} `json:"panels"`
	}
	if err := json.Unmarshal(b, &dash); err != nil {
		t.Fatalf("parse dashboard: %v", err)
	}
	found := false
	for _, p := range dash.Panels {
		if p.Title != "Request timeline (by correlation_id)" {
			continue
		}
		found = true
		if len(p.Targets) == 0 {
			t.Error("the request timeline panel has no queries")
		}
		for _, tg := range p.Targets {
			if strings.Contains(tg.Expr, "event_type") {
				t.Errorf("the request timeline panel must not filter on event_type (EN-7: any event_type, not only saga): %s", tg.Expr)
			}
			if !strings.Contains(tg.Expr, "correlation_id=~") {
				t.Errorf("the request timeline panel query must select by correlation_id: %s", tg.Expr)
			}
		}
	}
	if !found {
		t.Error(`source dashboard has no "Request timeline (by correlation_id)" panel`)
	}
}

// GAP-2 (2026-07-22): the promtail parse stage must match the SUT's ENCODING. It was hardcoded to
// `json`, so Memstore (Go stdlib logger → logfmt) extracted nothing: empty level/correlation_id
// labels and a literal `<no value>` event_type from the normalization template downstream.
func TestRenderPromtail_ParseStageFollowsDeclaredLogFormat(t *testing.T) {
	var c config.Config
	c.Observability.Loki.CorrelationField = "correlation_id"
	c.Observability.Loki.SagaEventField = "event_type"

	json := RenderPromtail(&c, "memstore-compose")
	if !strings.Contains(json, "- json:") || !strings.Contains(json, "expressions:") {
		t.Errorf("the DEFAULT must stay a json stage (the the operator convention):\n%s", json)
	}
	if strings.Contains(json, "- logfmt:") {
		t.Error("an undeclared log_format must not emit a logfmt stage")
	}

	c.Observability.Loki.LogFormat = "logfmt"
	lf := RenderPromtail(&c, "memstore-compose")
	if !strings.Contains(lf, "- logfmt:") || !strings.Contains(lf, "mapping:") {
		t.Errorf("log_format: logfmt must emit a logfmt stage with a mapping:\n%s", lf)
	}
	if strings.Contains(lf, "- json:") {
		t.Error("logfmt must REPLACE the json stage, not sit alongside it")
	}
	// the saga normalization template must still ride behind whichever parse stage was chosen
	if !strings.Contains(lf, "source: event_type") || !strings.Contains(lf, "saga_marker") {
		t.Errorf("the saga normalization stage must survive the logfmt branch:\n%s", lf)
	}
}

// PROB-2: the promtail normalization must cover EVERY declared marker value, or the dashboard's
// saga panel stays partial in exactly the way the Go reader was.
func TestRenderPromtail_NormalizesEveryDeclaredSagaMarker(t *testing.T) {
	var c config.Config
	c.Observability.Loki.SagaEventField = "event"
	c.Observability.Loki.SagaEventValues = []string{"tool_dispatch", "ayrshare_dispatch", "ghost_dispatch"}
	got := RenderPromtail(&c, "social-compose")
	for _, v := range []string{"tool_dispatch", "ayrshare_dispatch", "ghost_dispatch"} {
		if !strings.Contains(got, `(eq .saga_marker "`+v+`")`) {
			t.Errorf("marker %q missing from the normalization condition:\n%s", v, got)
		}
	}
	if !strings.Contains(got, "{{ if or (eq .saga_marker") {
		t.Errorf("several markers must combine with `or`:\n%s", got)
	}

	// ONE value must keep the original single-eq shape (no gratuitous `or`)
	var single config.Config
	single.Observability.Loki.SagaEventField = "event_type"
	single.Observability.Loki.SagaEventValue = "saga"
	s := RenderPromtail(&single, "orderservice-compose")
	if !strings.Contains(s, `{{ if eq .saga_marker "saga" }}saga{{ else }}`) {
		t.Errorf("the single-value template regressed:\n%s", s)
	}
}

// Under the M3 per-instance compose layout onboard.sh runs each instance as its OWN compose
// project, "argus-inst-<instance-id>" (COMPOSE_PROJECT, onboard.sh) — never the bare "argus" the
// keep regex hardcoded, which was only ever right for the single-project dogfood stack. An anchored
// keep regex that never matches the instance's own project keeps none of the product's own logs.
// Two instances' containers are "discovered" by Docker SD (nothing in this repo filters that
// beyond the rendered regex), and the rendered keep regex, applied the way Prometheus applies it
// (anchored, full-string), must keep the instance's own project and exclude the other one.
func TestRenderPromtail_KeepsOnlyThisInstancesOwnComposeProject(t *testing.T) {
	c := cfg(t, `
project: {name: my-service}
deploy: {compose_project: my-sut}
observability:
  loki: {url: "http://loki:3100"}
`)
	got := RenderPromtail(c, "orderservice-compose")
	mustParseYAML(t, got)

	re := regexp.MustCompile(`(?m)^\s*regex: '([^']*)'\n\s*action: keep`)
	m := re.FindStringSubmatch(got)
	if m == nil {
		t.Fatalf("no keep regex found in the rendered config:\n%s", got)
	}
	keep := regexp.MustCompile(`^(?:` + m[1] + `)$`)

	if !keep.MatchString("argus-inst-orderservice-compose") {
		t.Errorf("keep regex %q does not match this instance's OWN compose project "+
			"(argus-inst-orderservice-compose) — none of the product's own logs would ship", m[1])
	}
	if keep.MatchString("argus-inst-social-mcp-compose") {
		t.Errorf("keep regex %q matches a DIFFERENT instance's compose project — its logs would "+
			"leak into this instance's Loki", m[1])
	}
	if keep.MatchString("some-unrelated-project") {
		t.Errorf("keep regex %q matches an unrelated project on the machine", m[1])
	}
	if !keep.MatchString("my-sut") {
		t.Errorf("keep regex %q must still keep the SUT's own declared project", m[1])
	}
}

// A standalone/local onboard (no registered instance id) has no per-instance compose project — it
// is the single dogfood project literally named "argus" — so the keep regex must still be "argus"
// there, unchanged from before this fix.
func TestRenderPromtail_LocalOnboardKeepsTheDogfoodProject(t *testing.T) {
	c := cfg(t, `
project: {name: my-service}
deploy: {compose_project: my-sut}
observability:
  loki: {url: "http://loki:3100"}
`)
	got := RenderPromtail(c)
	if !strings.Contains(got, "argus|my-sut") {
		t.Errorf("a standalone onboard must still keep the dogfood project 'argus'; got:\n%s", got)
	}
}

// promtailSDProjects parses a promtail config and returns, per docker_sd_configs entry, the compose
// project its Docker API label filter restricts it to ("" for an entry with no such filter).
func promtailSDProjects(t *testing.T, rendered string) []string {
	t.Helper()
	var doc struct {
		ScrapeConfigs []struct {
			DockerSD []struct {
				Filters []struct {
					Name   string   `yaml:"name"`
					Values []string `yaml:"values"`
				} `yaml:"filters"`
			} `yaml:"docker_sd_configs"`
		} `yaml:"scrape_configs"`
	}
	if err := yaml.Unmarshal([]byte(rendered), &doc); err != nil {
		t.Fatalf("promtail config is not valid YAML: %v\n%s", err, rendered)
	}
	var out []string
	for _, sc := range doc.ScrapeConfigs {
		for _, sd := range sc.DockerSD {
			proj := ""
			for _, f := range sd.Filters {
				if f.Name != "label" {
					continue
				}
				for _, v := range f.Values {
					if p, ok := strings.CutPrefix(v, "com.docker.compose.project="); ok {
						if proj != "" {
							t.Errorf("one docker_sd_configs entry filters on two projects (%q, %q): Docker ANDs "+
								"repeated label filters, so it would match NO container", proj, p)
						}
						proj = p
					}
				}
			}
			out = append(out, proj)
		}
	}
	return out
}

// AC-D29 (issue #170): promtail 3.6.8 tails EVERY container docker_sd discovers and discards the keep
// verdict, shipping foreign lines with labels {}. The rendered config must therefore restrict DISCOVERY:
// every docker_sd_configs entry carries a compose-project label filter, and together they name exactly
// this instance's own project and the SUT's — never an unfiltered entry, never a foreign project.
func TestRenderPromtail_DiscoveryIsFilteredToOwnAndSUTProjects(t *testing.T) {
	c := cfg(t, `
project: {name: my-service}
deploy: {compose_project: my-sut}
observability:
  loki: {url: "http://loki:3100"}
`)
	for _, tc := range []struct {
		name string
		inst []string
		want []string
	}{
		{"onboarded instance", []string{"orderservice-compose"}, []string{"argus-inst-orderservice-compose", "my-sut"}},
		{"standalone onboard", nil, []string{"argus", "my-sut"}},
	} {
		got := promtailSDProjects(t, RenderPromtail(c, tc.inst...))
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Errorf("%s: docker_sd_configs filter projects = %q, want %q — an entry with \"\" is UNFILTERED and "+
				"would tail every container on the Docker host", tc.name, got, tc.want)
		}
	}

	// No SUT project declared: only the instance's own project, still filtered.
	noSUT := cfg(t, "project: {name: my-service}\nobservability:\n  loki: {url: \"http://loki:3100\"}\n")
	if got := promtailSDProjects(t, RenderPromtail(noSUT, "x")); strings.Join(got, ",") != "argus-inst-x" {
		t.Errorf("no SUT project: filter projects = %q, want [argus-inst-x]", got)
	}
}

// promtailStage is one pipeline_stages entry, decoded just far enough to find the correlation stages.
type promtailStage struct {
	Template *struct {
		Source   string `yaml:"source"`
		Template string `yaml:"template"`
	} `yaml:"template"`
	Labels map[string]any `yaml:"labels"`
}

// correlationLabelCheck finds the stage that gates the correlation_id LABEL in a promtail pipeline and
// returns a function that RUNS its template on a value — the way promtail does — so a test asserts what
// becomes a label, not what the YAML says. It fails the test if the stage is missing, doubled, or sits
// AFTER the labels stage that promotes correlation_id (where it would gate nothing).
//
// hasPrefix is sprig's (prefix first, then the string): promtail's template stage exposes sprig's
// functions. That argument order was confirmed on the real promtail 3.6.8 binary (AC-D32): a tr- id was
// labelled, a UUID, an empty value and "xtr-…" were not.
func correlationLabelCheck(t *testing.T, stages []promtailStage) func(string) string {
	t.Helper()
	gate, promote := -1, -1
	for i, s := range stages {
		if s.Template != nil && s.Template.Source == "correlation_id" {
			if gate >= 0 {
				t.Fatalf("two template stages rewrite correlation_id (stages %d and %d)", gate, i)
			}
			gate = i
		}
		if _, ok := s.Labels["correlation_id"]; ok && promote < 0 {
			promote = i
		}
	}
	if promote < 0 {
		t.Fatal("no labels stage promotes correlation_id — the dashboard's log panels select on that label")
	}
	if gate < 0 {
		t.Fatal("no template stage gates correlation_id before it becomes a label: every distinct value of the " +
			"SUT's correlation field would be a Loki stream (AC-D32 — 9,044 values filled the 5,000-stream cap in ~90 s)")
	}
	if gate > promote {
		t.Fatalf("the correlation_id gate (stage %d) runs AFTER the labels stage (stage %d) — it gates nothing", gate, promote)
	}
	tmpl, err := template.New("gate").Funcs(template.FuncMap{
		"hasPrefix": func(prefix, s string) bool { return strings.HasPrefix(s, prefix) },
	}).Parse(stages[gate].Template.Template)
	if err != nil {
		t.Fatalf("correlation_id gate template does not parse: %v", err)
	}
	return func(v string) string {
		var b strings.Builder
		if err := tmpl.Execute(&b, map[string]any{"Value": v}); err != nil {
			t.Fatalf("correlation_id gate template failed on %q: %v", v, err)
		}
		return b.String()
	}
}

// assertOnlyArgusIDsBecomeLabels runs the gate over the cases that matter: an Argus id passes through
// untouched (the dashboard selects correlation_id=~"tr-<run>-…"); a SUT's own id, an empty value and a
// "tr-" that is not a prefix all come out empty, so promtail sets no label for them.
func assertOnlyArgusIDsBecomeLabels(t *testing.T, where string, gate func(string) string) {
	t.Helper()
	for in, want := range map[string]string{
		"tr-20260923T182013000-ORDE-001-abcd1234": "tr-20260923T182013000-ORDE-001-abcd1234",
		"tr-0123456789abcdef":                     "tr-0123456789abcdef",
		"6f9619ff-8b86-d011-b42d-00c04fc964ff":    "",
		"":                                        "",
		"xtr-not-argus":                           "",
	} {
		if got := gate(in); got != want {
			t.Errorf("%s: correlation_id %q became label value %q, want %q", where, in, got, want)
		}
	}
}

func promtailStages(t *testing.T, rendered string) []promtailStage {
	t.Helper()
	var doc struct {
		ScrapeConfigs []struct {
			PipelineStages []promtailStage `yaml:"pipeline_stages"`
		} `yaml:"scrape_configs"`
	}
	if err := yaml.Unmarshal([]byte(rendered), &doc); err != nil {
		t.Fatalf("promtail config is not valid YAML: %v\n%s", err, rendered)
	}
	if len(doc.ScrapeConfigs) != 1 {
		t.Fatalf("want exactly one scrape config, got %d", len(doc.ScrapeConfigs))
	}
	return doc.ScrapeConfigs[0].PipelineStages
}

// AC-D32 (issue #173): only an ARGUS id may become the correlation_id stream label. Every distinct label
// value is a Loki stream; a SUT that stamps its own per-request ids into its declared field filled the
// instance Loki's 5,000-stream cap in ~90 s on hub-dev, after which Loki refused every new stream —
// including the runner's own request events. Both parse encodings, since the gate must follow either.
func TestRenderPromtail_OnlyArgusIDsBecomeTheCorrelationLabel(t *testing.T) {
	for _, format := range []string{"", "logfmt"} {
		var c config.Config
		c.Observability.Loki.CorrelationField = "request_id"
		c.Observability.Loki.LogFormat = format
		got := RenderPromtail(&c, "hub-dev")
		mustParseYAML(t, got)
		assertOnlyArgusIDsBecomeLabels(t, "RenderPromtail log_format="+format, correlationLabelCheck(t, promtailStages(t, got)))
	}
}
