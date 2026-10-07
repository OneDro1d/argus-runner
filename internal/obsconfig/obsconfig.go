// Package obsconfig renders the bundled obs-stack configs (promtail, prometheus)
// PER SUT from argus-config.yaml's argus-owned block, so the colleague's separate
// compose project/network is captured by logs (RO-05) + metrics (RO-07) and the error
// view is SUT-agnostic (RO-08) — never the hardcoded order-service of the dogfood.
// The onboarder writes these files before bringing the obs stack up.
package obsconfig

import (
	"fmt"
	"strings"

	"github.com/OneDro1d/argus-runner/internal/config"
)

// keepRegex keeps the argus obs project + the SUT project (parameterized, RO-05).
func keepRegex(c *config.Config) string {
	keep := "argus"
	if p := c.SUTProject(); p != "" {
		keep += "|" + p
	}
	return keep
}

// promtailKeepRegex keeps THIS instance's own compose project + the SUT project.
//
// ⛔ THE BARE "argus" LITERAL ONLY EVER MATCHED THE SINGLE-PROJECT DOGFOOD STACK
// (docker-compose.yaml's `name: argus`). Under the M3 per-instance layout onboard.sh runs each
// instance as its OWN compose project, "argus-inst-<instance-id>" (COMPOSE_PROJECT, onboard.sh) —
// and Prometheus's relabel `action: keep` is ANCHORED (`^(?:re)$`), so "argus" never matches
// "argus-inst-<id>". A keep rule that matches none of the product's own compose projects keeps
// none of the product's own logs. A standalone/local onboard (no registered instance id) has no
// per-instance project at all — it IS the dogfood project literally named "argus" — so that case
// keeps the literal unchanged.
func promtailKeepRegex(c *config.Config, inst string) string {
	return strings.Join(promtailProjects(c, inst), "|")
}

// promtailProjects is the compose projects THIS instance's promtail may read: its own, then the SUT's.
// The keep regex above and the discovery filters below are both derived from it, so they cannot drift.
func promtailProjects(c *config.Config, inst string) []string {
	proj := "argus"
	if inst != "" && inst != "local" {
		proj = "argus-inst-" + inst
	}
	out := []string{proj}
	if p := c.SUTProject(); p != "" && p != proj {
		out = append(out, p)
	}
	return out
}

// promtailDockerSD renders one docker_sd_configs entry PER PROJECT, each restricted by a Docker API
// label filter, so promtail never DISCOVERS a container outside those projects (AC-D29, issue #170).
//
// ⛔ THE KEEP RULE ALONE DOES NOT STOP PROMTAIL READING A CONTAINER. promtail's docker target
// (clients/pkg/promtail/targets/docker, v3.6.8 = 138c391) creates a tailing target for EVERY discovered
// container without evaluating relabel_configs (target_group.go addTarget), and handleOutput then runs
// `processed, _ := relabel.Process(...)` — the keep/drop verdict is DISCARDED, so a container that fails
// `keep` still ships every line, with its labels stripped to {}. Measured on a compose host: 98.5% of a
// promtail's output was other containers' lines with label set {}. An `action: drop` rule is ignored the
// same way. Upstream fixed handleOutput on 2026-05-12 (#21773); filtering at discovery does not depend on
// which promtail runs.
//
// One entry per project because Docker ANDs repeated `label` filters: a single entry listing both
// projects would match no container at all. Entries on the same host share one promtail target group
// (targetmanager.go keys it by job/host:port), so the result is the union.
func promtailDockerSD(projects []string) string {
	var b strings.Builder
	for _, p := range projects {
		fmt.Fprintf(&b, `      - host: unix:///var/run/docker.sock
        refresh_interval: 5s
        filters:
          - name: label
            values: ['com.docker.compose.project=%s']
`, p)
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// CorrelationLabelStage is the promtail stage that keeps the `correlation_id` stream label for
// ARGUS ids only (tr-…) and blanks every other value, so an empty label is never set (AC-D32, #173).
//
// ⛔ EVERY DISTINCT LABEL VALUE IS A NEW LOKI STREAM. The SUT's declared correlation field is copied
// into this label, and a SUT that stamps its OWN per-request ids there (Hub's governance used a
// fresh AMQP UUID per message) makes one stream per request: measured on hub-dev, 9,044 distinct
// values in the first-start backlog filled Loki's 5,000-stream cap in ~90 s, and Loki then refused
// every new stream — the SUT's and the runner's own request events alike.
//
// Argus ids are per SCENARIO (tr-<run_id>-<scenario_id>-<8hex>), so the label's cardinality scales
// with scenarios per run, not with SUT traffic. The dashboard's log panels only ever select
// correlation_id=~"tr-…", and get_sagas/tail_logs find a line by a LINE filter on the id
// (obsquery.queryLines), so the SUT's own ids stay searchable in the line body. Measured on the
// real promtail 3.6.8 binary: a tr- id is labelled; a UUID, an empty value and "xtr-…" carry no label.
//
// Indented for a `pipeline_stages:` list at six spaces; k8srender re-indents it for its ConfigMap.
const CorrelationLabelStage = `      # ONLY an Argus id (tr-…) becomes the correlation_id LABEL: every distinct value is a Loki stream,
      # and a SUT's own per-request ids filled the 5,000-stream cap in ~90 s (AC-D32). Other ids stay in
      # the line body, where get_sagas/tail_logs find them.
      - template:
          source: correlation_id
          template: '{{ if hasPrefix "tr-" .Value }}{{ .Value }}{{ end }}'`

// RenderPromtail produces the promtail config that ships THIS SUT's stdout to Loki:
// keep-filter scoped to the SUT project, the `project` stream label derived from the
// container's compose-project label (RO-05), and pipeline stages extracting the SUT's
// structured level into the `level` stream label (RO-08) AND its propagated correlation id
// (the SUT's declared field — correlation_id / request_id) into the `correlation_id` stream
// label, so the dashboard's correlation-id combo dropdown can list values + the saga/logs
// panels can be sliced by run/scenario via the embedded id. Only ARGUS ids become that label
// (CorrelationLabelStage): every distinct value is a Loki stream.
func RenderPromtail(c *config.Config, obsInstance ...string) string {
	// argus_instance LABEL for the SUT's shipped logs — the REGISTERED instance id when onboarded to a
	// control plane (so the saga/logs panels, filtered by the dashboard's argus_instance var, match), else
	// "local" for a standalone onboard. Must agree with the executor's ObsInstance + the pushed metrics.
	inst := "local"
	if len(obsInstance) > 0 && obsInstance[0] != "" {
		inst = obsInstance[0]
	}
	push := strings.TrimRight(c.LokiURL(), "/") + "/loki/api/v1/push"

	// Patch #4: label `project` with the config Project.Name (ProjectLabel) — the canonical SUT
	// identity that MATCHES the metric `project` label (push.go) + the dashboard's `project` var,
	// so the dashboard's Loki panels scope per-SUT and a prior SUT's logs cannot bleed through.
	// (derive_project_from_container is superseded: the docker compose-project name does NOT match
	// the metric project, which broke per-SUT log scoping.)
	projectRelabel := "      - target_label: project\n" +
		"        replacement: " + c.ProjectLabel()

	// GAP-2 (2026-07-22): the pipeline's parse stage must match the SUT's ENCODING. This was
	// hardcoded to `json`, so a logfmt SUT (Memstore logs via Go's stdlib logger) silently extracted
	// NOTHING — its level/correlation_id labels came out empty and event_type rendered as the Go
	// template's literal `<no value>`. promtail's logfmt stage takes bare key names, not JSON
	// expressions, so the two stages are rendered differently.
	parseStage := fmt.Sprintf(`      - json:
          expressions:
            level: %s
            correlation_id: %s
            saga_marker: %s`, c.LevelField(), c.CorrelationField(), c.SagaEventField())
	if c.LogFormat() == "logfmt" {
		parseStage = fmt.Sprintf(`      - logfmt:
          mapping:
            level: %s
            correlation_id: %s
            saga_marker: %s`, c.LevelField(), c.CorrelationField(), c.SagaEventField())
	}

	return fmt.Sprintf(`# RENDERED by the argus onboarder from argus-config.yaml (RO-05/08). Do not edit by hand.
server:
  http_listen_port: 9080
  grpc_listen_port: 0
positions:
  # AC-D35: under the named promtail-positions volume (mounted at /var/lib/promtail in both
  # compose files), which survives a promtail recreate — the container's own writable layer
  # (where /tmp lived) does not.
  filename: /var/lib/promtail/positions.yaml
clients:
  - url: %s
scrape_configs:
  - job_name: argus-docker
    # Discovery is filtered PER PROJECT at the Docker API: the keep rule below does not stop promtail
    # reading a container (AC-D29) — see promtailDockerSD in internal/obsconfig.
    docker_sd_configs:
%s
    relabel_configs:
      # Second line of defence only: honoured by promtail releases after 2026-05-12, ignored by 3.6.8.
      - source_labels: ['__meta_docker_container_label_com_docker_compose_project']
        regex: '%s'
        action: keep
      - source_labels: ['__meta_docker_container_name']
        regex: '/?(.*)'
        target_label: container
      - source_labels: ['__meta_docker_container_label_com_docker_compose_service']
        target_label: service
      - target_label: argus_instance
        replacement: %s
%s
      - target_label: cluster
        replacement: local
    pipeline_stages:
%s
%s
      # NORMALIZE the SUT's saga marker to the CANONICAL event_type="saga" for EVERY SUT, so the SHARED
      # dashboard filters event_type=~"saga" for all of them. A SUT declaring a non-standard saga
      # field/value (e.g. Social's saga_event_field=event, saga_event_value=tool_dispatch) is mapped to
      # "saga"; any other value of that field passes through unchanged (so it never masquerades as a saga).
      # This replaces the old per-SUT RenderDashboard substitution, which could hold only ONE SUT's value.
      # PROB-2: the condition covers EVERY declared marker value — a SUT that spreads control actions
      # over several values (Social's tool_dispatch / ayrshare_dispatch / ghost_dispatch) would
      # otherwise have only its first value normalized, leaving the dashboard's saga panel partial in
      # exactly the way the Go-side reader was.
      - template:
          source: event_type
          template: '{{ if %s }}saga{{ else }}{{ .saga_marker | default "" }}{{ end }}'
      - labels:
          level:
          correlation_id:
          event_type:
`, push, promtailDockerSD(promtailProjects(c, inst)), promtailKeepRegex(c, inst), inst, projectRelabel, parseStage, CorrelationLabelStage, sagaMarkerCond(c))
}

// sagaMarkerCond builds the promtail template condition matching ANY declared saga marker value
// (PROB-2): `eq .saga_marker "a"` for one, `or (eq …"a") (eq …"b")` for several. Go's `or` is
// variadic, so this stays one expression however many values a SUT declares.
func sagaMarkerCond(c *config.Config) string {
	vals := c.SagaEventValues()
	eqs := make([]string, 0, len(vals))
	for _, v := range vals {
		eqs = append(eqs, `(eq .saga_marker "`+v+`")`)
	}
	if len(eqs) == 1 {
		return strings.TrimSuffix(strings.TrimPrefix(eqs[0], "("), ")")
	}
	return "or " + strings.Join(eqs, " ")
}

// RenderDashboard substitutes the COLLECTed errors-view level matcher (RO-08) into the
// dashboard's log_view "Errors + warnings" value, so a SUT with a non-standard level
// vocabulary parameterizes the errors view instead of the config key being a dead no-op.
// (The default (?i)error|warn makes this a no-op for the standard the operator slog convention.)
func RenderDashboard(dashboardJSON []byte, c *config.Config) []byte {
	const def = `level=~\"(?i)error|warn\"`
	want := `level=~\"` + c.ErrorMatch() + `\"`
	out := strings.ReplaceAll(string(dashboardJSON), def, want)
	// The Saga panel filters the CANONICAL event_type="saga" for EVERY SUT — promtail NORMALIZES each
	// SUT's declared saga marker (saga_event_field/value) to "saga" at ingestion (see RenderPromtail). So
	// the shared dashboard needs NO per-SUT saga substitution. (The old substitution swapped the dashboard
	// literal to the SUT's saga_event_value; with one shared dashboard it could hold only the LAST-onboarded
	// SUT's value, blanking every other SUT's saga panel — the multi-SUT bug this fixes.)
	return []byte(out)
}

// RenderPrometheus produces the prometheus config that scrapes THIS SUT's /metrics
// (RO-07): explicit targets from the config, or Docker SD keyed on the SUT project.
// The runner pushgateway (argus_* metrics) is always scraped. Prometheus must also be
// JOINED to the SUT network (compose) for either path to reach the SUT — that is the
// onboarder's job; this only renders the scrape config.
func RenderPrometheus(c *config.Config, obsInstance ...string) string {
	inst := "local"
	if len(obsInstance) > 0 && obsInstance[0] != "" {
		inst = obsInstance[0]
	}
	head := fmt.Sprintf(`# RENDERED by the argus onboarder from argus-config.yaml (RO-07). Do not edit by hand.
global:
  scrape_interval: 15s
  external_labels:
    project: %s
    cluster: compose
scrape_configs:
`, c.ProjectLabel())

	var sut string
	if c.PromDiscoverByProject() {
		sut = fmt.Sprintf(`  - job_name: sut-docker
    docker_sd_configs:
      - host: unix:///var/run/docker.sock
        refresh_interval: 5s
        port: 9090
    relabel_configs:
      - source_labels: ['__meta_docker_container_label_com_docker_compose_project']
        regex: '%s'
        action: keep
      - source_labels: ['__meta_docker_port_private']
        regex: '9090'
        action: keep
      - source_labels: ['__meta_docker_container_label_com_docker_compose_service']
        target_label: service
      - target_label: argus_instance
        replacement: %s
`, keepRegex(c), inst)
	} else {
		quoted := make([]string, 0, len(c.PromTargets()))
		for _, t := range c.PromTargets() {
			quoted = append(quoted, `"`+t+`"`)
		}
		sut = fmt.Sprintf(`  - job_name: sut
    static_configs: [{targets: [%s], labels: {argus_instance: %s}}]
`, strings.Join(quoted, ", "), inst)
	}

	push := `  - job_name: pushgateway
    honor_labels: true
    static_configs: [{targets: ["pushgateway:9091"]}]
`
	return head + sut + push
}
