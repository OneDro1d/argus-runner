// Package config loads + validates an argus-config.yaml (VR-A1). validate-config
// = structural load + the layer↔target coverage check ported from argus
// preflight.py (no python on the run path). It also resolves the SUT HTTP
// host/port/protocol the runner-core passes to JMeter as -J props.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/OneDro1d/argus-runner/internal/scenario"
	"github.com/OneDro1d/argus-runner/internal/testtargets"
	"gopkg.in/yaml.v3"
)

// ── Target shapes (VR10-S3 / V28-012) ────────────────────────────────────────
// One named struct per kind, shared by the plain slot AND by its `<kind>_targets` map, so a named
// entry has EXACTLY the fields of its plain slot (owner decision D4: one mechanism for every kind,
// built in full). The plain slot stays the default; a scenario selects a named entry with one word
// (`- **Target**: <name>` in Metadata; `"target"` on a chain step) — see SelectTarget.

// HTTPTarget is one HTTP surface of the SUT: targets.http and every targets.http_targets.<name>.
type HTTPTarget struct {
	BaseURL string `yaml:"base_url"`
}

// MCPAuth is an MCP target's static auth.
type MCPAuth struct {
	Type        string `yaml:"type"`         // none | bearer
	BearerToken string `yaml:"bearer_token"` // the SUT's static MCP bearer (smcp_…); "" = none
}

// MCPTarget is one MCP endpoint of the SUT (CHANGE-1): targets.mcp — where an MCP SUT's
// endpoint/transport/token are declared, replacing the as-built MCP_URL/MCP_TOKEN env shortcut —
// and every targets.mcp_targets.<name>. A per-scenario TRIGGER/server_url value remains an explicit
// override for a SUT with ONE endpoint; with a named target selected it is refused (two answers).
type MCPTarget struct {
	BaseURL   string `yaml:"base_url"`
	Transport string `yaml:"transport"` // streamable-http (default) | http-sse — declared, never guessed
	// TimeoutSeconds (GAP-3, 2026-07-22) is the per-call MCP client deadline; default 30.
	// It was hardcoded at 30s, which made a slow-but-HEALTHY SUT unavoidably flaky: Social's
	// passing calls measured 16.1s and 19.1s, and four of its eight failures were transport
	// timeouts at exactly 30.00s. A SUT that legitimately talks to a slow upstream can now
	// declare its real ceiling instead of being scored red for our default.
	TimeoutSeconds int      `yaml:"timeout_seconds"`
	Auth           *MCPAuth `yaml:"auth"`
}

// DBTarget is one database of the SUT: targets.database and every targets.database_targets.<name>.
type DBTarget struct {
	// Type is the documented `type: postgres` key the schema, the template and the shipped examples
	// carry. Nothing in the product reads it (the JDBC driver is chosen from the URL); it is a field
	// so the strict targets node does not refuse every shipped config.
	Type     string `yaml:"type"`
	JDBCURL  string `yaml:"jdbc_url"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

// MQTarget is one message broker of the SUT: targets.message_broker and every
// targets.message_broker_targets.<name>.
type MQTarget struct {
	Type          string            `yaml:"type"` // documented `type: amqp` — see DBTarget.Type
	URL           string            `yaml:"url"`
	ManagementURL string            `yaml:"management_url"`
	Queues        map[string]string `yaml:"queues"`
	Exchanges     map[string]string `yaml:"exchanges"`
	// RoutingKeys are the binding keys of the SUT's exchanges — `incoming` is the one the Message
	// Flow tap binds with (V30-004 F-6). An exchange is only half an address: a `topic` exchange
	// delivers nothing to a queue bound with the wrong key, so a tap with no key would silently
	// receive nothing and every content assertion would fail for a reason that is ours, not the
	// SUT's. An absent entry binds with "", which is correct for a `fanout` exchange and wrong for
	// a `topic` one — validate-config says so rather than letting the run discover it.
	RoutingKeys map[string]string `yaml:"routing_keys"`
}

// AuthTarget is targets.auth — a type and a token, never an address.
type AuthTarget struct {
	Type        string `yaml:"type"`
	BearerToken string `yaml:"bearer_token"`
}

// Targets is the `targets:` node. KEYS ARE STRICT HERE — and only here in this build (SA §0.14
// S3-a): an unknown key anywhere under targets is refused at load, by name, with the accepted list.
// Before VR10-S3 an unknown key (NEO-010's `graph_health:`) parsed fine and reached nothing — the
// same silent drop as V28-006. `rate_limiting` is REMOVED (VR10-S3-13; V28-019 owns the concept):
// under strict keys a dead-but-accepted key is the worst kind, because it looks supported.
type Targets struct {
	HTTP        *HTTPTarget            `yaml:"http"`
	HTTPTargets map[string]*HTTPTarget `yaml:"http_targets"`

	MessageBroker        *MQTarget            `yaml:"message_broker"`
	MessageBrokerTargets map[string]*MQTarget `yaml:"message_broker_targets"`

	Database        *DBTarget            `yaml:"database"`
	DatabaseTargets map[string]*DBTarget `yaml:"database_targets"`

	// External deliberately stays `any` — two shapes exist in the wild (see ExternalWebhookBaseURL);
	// it has no named form and is not part of the named-target mechanism.
	External any         `yaml:"external"`
	Auth     *AuthTarget `yaml:"auth"`

	MCP        *MCPTarget            `yaml:"mcp"`
	MCPTargets map[string]*MCPTarget `yaml:"mcp_targets"`
}

type Config struct {
	Project struct {
		Name string `yaml:"name"`
	} `yaml:"project"`
	Targets Targets `yaml:"targets"`
	// Observability + Deploy are the ARGUS-OWNED block (RO-10 split). argus's
	// run.sh/preflight.py never read these (they read project/targets/scenarios only),
	// so they live as a self-contained, relocatable block that argus ignores. Before
	// this round observability.loki.url / observability.prometheus.* were dead keys.
	Observability struct {
		Loki struct {
			Enabled                    *bool  `yaml:"enabled"`
			URL                        string `yaml:"url"`
			ProjectLabel               string `yaml:"project_label"`                 // RO-05: the Loki `project` label value
			DeriveProjectFromContainer bool   `yaml:"derive_project_from_container"` // RO-05: relabel project from the compose-project container label
			LevelField                 string `yaml:"level_field"`                   // RO-08: the SUT's structured level field
			ErrorMatch                 string `yaml:"error_match"`                   // RO-08: the errors-view level matcher
			// CHANGE-2: the SUT may log its propagated correlation id under a non-canonical
			// field name (Social uses request_id). The runner's log/saga extraction reads the
			// DECLARED field; the dashboard Loki panels filter by VALUE so they stay agnostic.
			// The propagation BEHAVIOUR stays HARD — only the field NAME is declarable.
			CorrelationField string `yaml:"correlation_field"` // default correlation_id
			SagaEventField   string `yaml:"saga_event_field"`  // default event_type (the field tagging a saga line)
			SagaEventValue   string `yaml:"saga_event_value"`  // default saga (its value marking a saga)
			// SagaEventValues (PROB-2, 2026-07-22) declares SEVERAL marker values when the SUT does
			// not use one. the operator's convention puts every step under event_type=saga and varies
			// step_name, so a scalar sufficed; Social instead encodes step identity in the marker
			// field itself (tool_dispatch on the gateway, ayrshare_dispatch and ghost_dispatch on
			// the workers). With only a scalar its saga could contain gateway lines and NOTHING
			// else — every worker-side control action (retry/NACK, DLQ routing, circuit-breaker
			// trip) was invisible by construction, which is exactly what a triage round reported
			// as "no saga contained any step from social-ayrshare or social-ghost at all".
			// Empty → falls back to the single saga_event_value. It is a WHITELIST: an undeclared
			// value never matches, so ordinary request logs cannot masquerade as saga steps.
			SagaEventValues []string `yaml:"saga_event_values"`
			// SagaStepFields (GAP-1, 2026-07-22) completes the translation table. The four fields
			// above told us which lines ARE sagas; nothing told us how this SUT names the fields
			// INSIDE one, so they were read under hardcoded the operator names. A SUT that marked its saga
			// lines correctly but named the steps differently (Social: msg/time/level) yielded a
			// timeline of content-free nodes that still reported available:true — a blind panel
			// claiming health. Map a LOGICAL name to this SUT's name; omitted keys use the the operator
			// default plus a fallback chain. Logical keys:
			//   saga_id · step · step_name · step_status · timestamp · error · service
			SagaStepFields map[string]string `yaml:"saga_step_fields"`
			// LogFormat declares how this SUT's lines are encoded: json (default) | logfmt. It
			// drives the promtail pipeline stage. The Go-side reader auto-detects either way, so
			// this only matters for the LABELS promtail extracts (Memstore logs logfmt via Go's
			// stdlib logger, and the json stage silently produced empty level/correlation_id).
			LogFormat string `yaml:"log_format"`
			// PushURL (T3.1, E3 export hosted-Loki target) is the operator's HOSTED Loki PUSH
			// endpoint (e.g. https://logs-xxx.grafana.net/loki/api/v1/push) -- the write side.
			// URL is the READ/query side (already used by adopt); a hosted-Loki export target
			// needs BOTH plus Credential (see LokiPushURLConfigured, k8srender.Instance
			// validate()'s obsModeExport case). Empty on every SUT that does not use export.
			PushURL string `yaml:"push_url"`
			// Credential (T3.1) is a ${VAR} reference ONLY -- never a literal -- resolving to the
			// hosted Loki's "username:password" Basic-Auth pair, SAME shape and SAME validation
			// rule as observability.betterstack.credential below (bsCredentialVarRe, checked in
			// validateObservabilityBackend). Threaded into promtail (push, via a k8s Secret +
			// config.expand-env -- never a literal in a rendered manifest) and into obsquery.Loki
			// (query, HTTP Basic auth) -- see k8srender.go / internal/obsquery/obsquery.go.
			Credential string `yaml:"credential"`
		} `yaml:"loki"`
		// Pushgateway (T3.2, E3 adopt mode) is the operator's OWN Prometheus Pushgateway, used
		// only when --obs=adopt. OPTIONAL, unlike loki.url in adopt mode: a Pushgateway is a
		// push TARGET, not a query surface anything reads FROM, so an operator without one just
		// gets no pushed argus_* metrics rather than a run-blocking refusal — see PushgatewayURL.
		Pushgateway PushgatewayObs `yaml:"pushgateway"`
		// BetterStack (AC-D13) is an ALTERNATIVE log/saga source to the bundled Loki — a hosted
		// Telemetry account queried over its HTTP SQL API instead of LogQL. Exactly one of
		// observability.loki / observability.betterstack may be declared (see
		// validateObservabilityBackend); the query surface offered to the rest of Argus
		// (correlation-id lookup within a window, saga-marker lines, per-service grouping) is the
		// same either way — see internal/obsquery.Backend.
		BetterStack struct {
			Enabled *bool `yaml:"enabled"`
			// QueryURL is the HTTP SQL query endpoint (the account's regional Query API host,
			// e.g. https://eu-nbg-2-connect.betterstackdata.com).
			QueryURL string `yaml:"query_url"`
			// Credential is a ${VAR} reference ONLY — never a literal (D1/D2 parity: resolved by
			// expandEnv like every other credential field). It carries the query API's
			// "username:password" Basic-Auth pair.
			Credential string `yaml:"credential"`
			TeamID     string `yaml:"team_id"`
			// Sources maps a source slug (as it appears in the table name
			// remote(t<team_id>_<slug>_logs)) to the service name it carries (BetterStack: one
			// source per service, logs only, no spans — see CorrelationFields). Slugs are
			// validated against a safe identifier pattern; never interpolated raw into SQL.
			Sources map[string]string `yaml:"sources"`
			// CorrelationFields are the JSON field PATHS tried in order to read the propagated
			// correlation id out of a log line's `raw` JSON body — dot-separated, e.g.
			// message_json.correlationId. Empty means the default two-path chain (see
			// defaultBetterStackCorrelationFields). Each path segment is validated against a safe
			// pattern before it is built into a JSONExtractString(...) SQL fragment.
			CorrelationFields []string          `yaml:"correlation_fields"`
			LevelField        string            `yaml:"level_field"`
			SagaEventField    string            `yaml:"saga_event_field"`
			SagaEventValue    string            `yaml:"saga_event_value"`
			SagaEventValues   []string          `yaml:"saga_event_values"`
			SagaStepFields    map[string]string `yaml:"saga_step_fields"`
		} `yaml:"betterstack"`
		// OpenShell (spec 26, A1 — observe only) says where the SUT agent's OpenShell sandbox events
		// are read. nil = absent = the feature is off (see openshell.go). Unlike its neighbours, this
		// block is strict: an unknown key inside it is refused by name (strictTargetsDoc).
		OpenShell  *OpenShellObs `yaml:"openshell"`
		Prometheus struct {
			Enabled           *bool    `yaml:"enabled"`
			Endpoint          string   `yaml:"endpoint"`
			Targets           []string `yaml:"targets"`             // RO-07: explicit SUT scrape targets (host:port)
			DiscoverByProject bool     `yaml:"discover_by_project"` // RO-07: Docker SD + keep on the SUT project
			// (request_rate_query retired — the request/error-rate panel is now runner-owned
			// ("Test requests sent", sourced from the Loki request-event stream), so no SUT PromQL is declared.)
		} `yaml:"prometheus"`
		Grafana struct {
			// PublicURL is the HOST-facing Grafana base the dashboard deep link must use — what a
			// HUMAN opens in a browser, never the in-container `grafana:3000`.
			//
			// PER-TIER since M3-FX (VR-E1/E5): it is the only field in this file whose correct value
			// differs per environment, so it is the only one that is a TierMap. `localhost:3000` is
			// right on compose and k3d and WRONG on a k8s tier, where localhost is not the operator's
			// machine. There is no default and no environment fallback — see GrafanaPublicURL.
			PublicURL         TierMap `yaml:"public_url"`
			DashboardTemplate string  `yaml:"dashboard_template"` // bundled dashboard path (informational) — COMMON, same everywhere
		} `yaml:"grafana"`
		// DashboardLink (, UI-2, A'1) is an operator-set link template for ANY tool
		// (Grafana, Zabbix, Datadog, an internal page). The control plane renders it per run, so the
		// link works for every past run the moment it is declared. Lenient on purpose, like everything
		// under `observability:`: an older executor ignores the key. Validated in full at load time
		// (dashboardLinkValidate): absolute http(s), no userinfo, no ${VAR}, six placeholders only.
		DashboardLink struct {
			Template string `yaml:"template"`
			Label    string `yaml:"label"`
		} `yaml:"dashboard_link"`
	} `yaml:"observability"`
	Deploy struct {
		ComposeProject string `yaml:"compose_project"` // RO-05/07: the SUT's docker compose project
		Network        string `yaml:"network"`         // RO-07: the SUT's docker network (JMeter + Prometheus join)
		// DeploymentProbe (UC073) is the OPTIONAL socket-free redeploy signal: the executor GETs `url`
		// and reads the stable build id from the JSON `fingerprint_field` (e.g. version/commit/build_id).
		// When it changes, the executor auto-pushes a deployment marker. Absent → no auto-detect (the
		// manual `argus mark-deployment`, UC074, remains the fallback).
		DeploymentProbe *struct {
			URL              string `yaml:"url"`
			FingerprintField string `yaml:"fingerprint_field"`
		} `yaml:"deployment_probe"`
	} `yaml:"deploy"`
	// RateLimit (VR10-R1 / V28-009) is the SUT owner's declaration of its own rate limit and of
	// how the SUT says "you are going too fast". TOP-LEVEL, never under `targets` — that key was
	// about testing a limiter, this block is about respecting one. Absent = nothing changes: no
	// heuristic may reclassify a verdict on its own (owner D1). See ratelimit.go.
	RateLimit *RateLimit `yaml:"rate_limit"`
	// DeclaredGapsList (CHANGE-4) are the SUT owner's honest declared-gap lines for SOFT/
	// conditional capabilities the suite CANNOT derive from the config (e.g. "no inbound
	// correlation propagation", "no sagas"). The preflight echoes them; a declared gap is
	// reported, never a failure. Lives at the top level so it reads as a SUT-facing declaration.
	DeclaredGapsList []string `yaml:"declared_gaps"`
	// Package (AC-36) declares the package `argus package-check --config` certifies — the
	// deployment's own manifests, lockfiles, schemas and seed document — instead of the whole
	// repository at Argus's conventional paths. Optional; every declared path must exist at load
	// time. See package.go.
	Package *Package `yaml:"package"`
	// MoneyHandling (T5.4) declares that this SUT moves real money. It turns on the money-path
	// guard (scenario.MoneyGuardViolations) at BOTH doors: Validate refuses every scenario that is
	// not a plain HTTP GET, names an order/quote/swap/rebalance/claim path, or runs a CLEANUP; and
	// the run refuses the same scenarios again before firing (argus.runOneScenario), because a file
	// can reach a run without passing the validator. Absent = false = nothing changes. TOP-LEVEL
	// and explicit on purpose: the rule it enforces lived only in runbook prose until now, and a
	// generating agent was free not to read it.
	MoneyHandling bool `yaml:"money_handling"`
	// MoneyWrites (money_writes follow-up, 2026-09-26) declares the EXACT (method, path) writes —
	// each optionally a bounded real spend — the SUT's own tester allows Argus to attempt against a
	// money-handling SUT. Valid only beside MoneyHandling: true (moneywrites.go's validate). Absent =
	// nil = nothing is exempted from MoneyGuardViolations' GET-only/money-verb-path rule, exactly
	// as before this field existed. See internal/scenario/moneywrites.go for the allowlist type and
	// the spend check this block feeds.
	MoneyWrites *MoneyWritesConfig `yaml:"money_writes"`
	// MessageSchemas (ARGUS-DESIGN-GENERIC-SCHEMA-MESSAGES-2026-09-28, design §1) declares the wire
	// schemas an `amqp` chain step's `schema`+`record` publish or `schema` consume may reference by
	// name — an app's message fields are test data in a scenario, never Argus code. Optional; every
	// entry is parsed and checked at load time (schemas.go), the same load-time-is-validate-time rule
	// Package/RateLimit/MoneyWrites already follow.
	MessageSchemas MessageSchemas `yaml:"message_schemas"`
	// LoadAllowedTargets (S9) names the message_broker_targets entries the OPERATOR has
	// marked as allowing load. TOP-LEVEL, never under `targets:` (an old executor decodes `targets`
	// strictly and would refuse a new key there; a top-level key falls into its lenient inline map and
	// is ignored). Absent / empty / a name not listed = no target allows load: the default.
	LoadAllowedTargets map[string]LoadAllow `yaml:"load_allowed_targets"`
	// TestTargets (, UI-6) declares the named slices of ONE instance's checks — e.g. `live`
	// (namespace msgbus) and `lab` (msgbus-lab) — with the scenario prefixes / tags that map a check to
	// each. TOP-LEVEL, never under the strict `targets:` block (an older executor would refuse the whole
	// config there); a top-level key is simply ignored by an older executor. NOT the connection
	// `targets:` and NOT federation's declared_targets. Absent = nil = today's one-target view. See
	// internal/testtargets.
	TestTargets testtargets.List `yaml:"test_targets"`
	// SummaryMetrics (UI-7a) declares the environment's own metrics source and the few
	// named numbers the executor reports from it. TOP-LEVEL and lenient on purpose -- see the type.
	SummaryMetrics *SummaryMetrics `yaml:"summary_metrics"`
	// CheckEnv declares extra environment variable NAMES the checks
	// use inside their own bodies, headers or chain steps (${NAME}), which no credential field names.
	// Names only, never values. TOP-LEVEL for the same reason as load_allowed_targets: an older
	// executor ignores it silently. See checkenv.go.
	CheckEnv []string `yaml:"check_env"`
}

// ── Argus-owned resolvers (RO-05/07/08/10, D5) ────────────────────────────
// Explicit config values win; absent ones fall back to safe defaults so a minimal
// config still works and the bundled obs-stack URLs are the default.

const (
	defaultLokiURL          = "http://loki:3100"
	defaultLevelField       = "level"
	defaultErrorMatch       = "(?i)error|warn"
	defaultGrafanaURL       = "http://localhost:3000"  // FX-1: host-facing default for the dashboard deep link
	defaultCorrelationField = "correlation_id"         // CHANGE-2: canonical join key field (VR-G5)
	defaultSagaEventField   = "event_type"             // CHANGE-2: the field tagging a saga line
	defaultSagaEventValue   = "saga"                   // CHANGE-2: its value marking a saga
	defaultPrometheusURL    = "http://prometheus:9090" // AC-11: bundled query-API base, mirrors defaultLokiURL
)

// defaultBetterStackCorrelationFields (AC-D13) are the JSON paths tried IN ORDER to read the
// correlation id out of a BetterStack log line: BetterStack nests an ingested JSON message body
// under `message_json`, so a SUT's own `correlationId` key lands at `message_json.correlationId`;
// `correlation_id` covers a SUT that also (or instead) writes it as a top-level field.
var defaultBetterStackCorrelationFields = []string{"message_json.correlationId", "correlation_id"}

// PrometheusURL is the Prometheus QUERY-API base the survival-plane read uses (AC-11) — distinct
// from Observability.Prometheus.Targets (the SUT's own /metrics SCRAPE targets, RO-07). Explicit
// `endpoint` wins; absent falls back to the bundled obs-stack default, same pattern as LokiURL.
func (c *Config) PrometheusURL() string {
	if c.Observability.Prometheus.Endpoint != "" {
		return c.Observability.Prometheus.Endpoint
	}
	return defaultPrometheusURL
}

// GrafanaPublicURL is the HOST-facing Grafana base used to build the dashboard deep link — what a
// HUMAN opens in a browser, never the in-container grafana:3000. It is resolved FOR A TIER, and it
// REFUSES rather than guessing (VR-E8).
//
// ── WHAT THIS FUNCTION USED TO SAY, AND WHY THAT WAS REVERSED (M3-FX section E) ──────────────────
//
// Until 2026-08-08 this comment argued the opposite, and argued it well:
//
//	"the browser-facing Grafana base is a property of the ENVIRONMENT, not of the SUT. One
//	 argus-config must serve compose, k3d and managed UNCHANGED — that is the bring-your-own-SUT
//	 premise — so baking a tier's Grafana host into it would be exactly the wrong place for it."
//
// The premise was right and the implementation contradicted it: the precedence ranked an explicit
// `public_url` in the SUT's config ABOVE the environment. So the managed executor WAS handed
// https://grafana.example.com — and then discarded it, because the Social SUT's config declared
// `public_url: http://localhost:3000` (which the documentation told its author to write). Every
// managed-tier deep link pointed at the operator's own laptop. That is INT-008, and it survived a
// 188-use-case sweep because a link is only checked by FETCHING it.
//
// The owner's decision was not to invert the precedence but to make the config able to answer the
// question properly: one file, three tiers, three correct answers. So the field is a TierMap, and:
//
//   - there is NO built-in default. `localhost:3000` was a sensible default for a single-host world
//     and a wrong answer on k8s, delivered silently.
//   - there is NO environment fallback. VR-E8 is explicit ("no fall-back to the environment"), and a
//     silent fallback is exactly what let a wrong config look correct for weeks.
//   - "declared a map but omitted my tier" and "declared nothing at all" produce the SAME refusal.
//     Both mean the same thing to the operator: this file cannot answer for the tier you are
//     onboarding to.
//
// ARGUS_GRAFANA_PUBLIC_URL is consequently NO LONGER READ HERE. k8srender still injects it into
// rendered executor manifests; that injection is now redundant and is scheduled for removal with the
// executor work rather than in this change, so the rendered manifests and their tests move together.
func (c *Config) GrafanaPublicURL(tier string) (string, error) {
	m := c.Observability.Grafana.PublicURL
	if u, ok := m.Resolve(tier); ok {
		return u, nil
	}
	return "", missingTierError("observability.grafana.public_url", tier, m.Tiers())
}

// SUTProject is the SUT's docker compose project (NOT necessarily project.name —
// project.name is a label). Empty when no deploy block is provided.
func (c *Config) SUTProject() string { return c.Deploy.ComposeProject }

// SUTNetwork is the SUT's docker network that JMeter + Prometheus must join (RO-07).
func (c *Config) SUTNetwork() string { return c.Deploy.Network }

// DeploymentProbeURL is the SUT endpoint the executor GETs to fingerprint the deployment (UC073); "" =
// no auto-detect. DeploymentProbeField is the JSON field in that response carrying the stable build id.
func (c *Config) DeploymentProbeURL() string {
	if c.Deploy.DeploymentProbe != nil {
		return strings.TrimSpace(c.Deploy.DeploymentProbe.URL)
	}
	return ""
}

func (c *Config) DeploymentProbeField() string {
	if c.Deploy.DeploymentProbe != nil {
		return strings.TrimSpace(c.Deploy.DeploymentProbe.FingerprintField)
	}
	return ""
}

// LokiURL is the Loki endpoint from the runner's vantage (bundled default).
func (c *Config) LokiURL() string {
	if c.Observability.Loki.URL != "" {
		return c.Observability.Loki.URL
	}
	return defaultLokiURL
}

// LokiURLConfigured is the operator's OWN observability.loki.url, with NO bundled-default
// fallback — "" when the operator declared none. Unlike LokiURL (which exists so the bundled
// path always has a working endpoint to hand the executor), --obs=adopt (T3.2) must tell
// "declared" apart from "silently defaulted": adopting the bundled default would point the
// executor at a `loki:3100` that --obs=adopt never deploys, so this is what render-k8s reads to
// require the key explicitly rather than resolving it away.
func (c *Config) LokiURLConfigured() string {
	return strings.TrimSpace(c.Observability.Loki.URL)
}

// LokiPushURLConfigured is the operator's HOSTED Loki PUSH endpoint (observability.loki.push_url,
// T3.1 export hosted-Loki target) — "" when undeclared. NO bundled-default fallback, same
// reasoning as LokiURLConfigured: a hosted-Loki export target must be told apart from "not
// declared" rather than resolved to something Argus never deploys.
func (c *Config) LokiPushURLConfigured() string {
	return strings.TrimSpace(c.Observability.Loki.PushURL)
}

// LokiCredential is the RESOLVED "username:password" Basic-Auth credential for a hosted-Loki
// export target (observability.loki.credential, T3.1) — "" when undeclared. Resolved through the
// SAME expandEnv mechanism as every other credential field (walkCredentialFields); never a
// literal (validateObservabilityBackend refuses one at load time).
func (c *Config) LokiCredential() string {
	return c.Observability.Loki.Credential
}

// PushgatewayURL is the operator's OWN Pushgateway endpoint (observability.pushgateway.url,
// T3.2), read only in --obs=adopt. Unlike LokiURL there is deliberately NO bundled-default
// fallback: bundled mode never calls this at all (k8srender keeps the hardcoded bundled
// pushgateway arg unchanged), and in adopt mode an empty value is not an error — it is the
// correct "off" signal render-k8s passes to the executor as an explicit --pushgateway "", which
// obsquery.PushMetrics (internal/obsquery/push.go) already treats as a no-op push rather than
// failing against a Pushgateway that was never deployed.
func (c *Config) PushgatewayURL() string {
	return strings.TrimSpace(c.Observability.Pushgateway.URL)
}

// LevelField is the SUT's structured log level field (RO-08).
func (c *Config) LevelField() string {
	if c.Observability.Loki.LevelField != "" {
		return c.Observability.Loki.LevelField
	}
	return defaultLevelField
}

// ErrorMatch is the SUT-agnostic errors-view level matcher (RO-08); default ERROR|WARN.
// DEPRECATED (M25-FX3 Unit 1): the bundled dashboard's logs panel now shows ALL SUT logs
// (no level filter — generic for any SUT), so this is inert for the default dashboard. Kept
// for back-compat / a custom dashboard that still parameterizes a level-based errors view.
func (c *Config) ErrorMatch() string {
	if c.Observability.Loki.ErrorMatch != "" {
		return c.Observability.Loki.ErrorMatch
	}
	return defaultErrorMatch
}

// CorrelationField is the SUT log field carrying the propagated correlation id (CHANGE-2);
// default correlation_id. Social declares request_id.
func (c *Config) CorrelationField() string {
	if c.Observability.Loki.CorrelationField != "" {
		return c.Observability.Loki.CorrelationField
	}
	return defaultCorrelationField
}

// SagaEventField / SagaEventValue declare how a saga line is tagged (CHANGE-2); default
// event_type=saga (the canonical GoKit convention).
func (c *Config) SagaEventField() string {
	if c.Observability.Loki.SagaEventField != "" {
		return c.Observability.Loki.SagaEventField
	}
	return defaultSagaEventField
}
func (c *Config) SagaEventValue() string {
	if c.Observability.Loki.SagaEventValue != "" {
		return c.Observability.Loki.SagaEventValue
	}
	// PROB-2: when only the LIST form is declared, the scalar accessor reports its first entry —
	// callers that can hold just one value (the promtail template's primary case, messages) stay
	// correct rather than silently reverting to the "saga" default the SUT never emits.
	if v := c.Observability.Loki.SagaEventValues; len(v) > 0 && v[0] != "" {
		return v[0]
	}
	return defaultSagaEventValue
}

// SagaEventValues is the full SET of values marking a saga line for this SUT (PROB-2): the declared
// list when present, else the single scalar. Always non-empty. Duplicates are collapsed and blanks
// dropped so a sloppy declaration cannot widen the whitelist to "".
func (c *Config) SagaEventValues() []string {
	seen := map[string]bool{}
	var out []string
	add := func(v string) {
		if v = strings.TrimSpace(v); v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	add(c.Observability.Loki.SagaEventValue)
	for _, v := range c.Observability.Loki.SagaEventValues {
		add(v)
	}
	if len(out) == 0 {
		return []string{defaultSagaEventValue}
	}
	return out
}

// LogFieldMappingWarnings returns one advisory warning per log-field TRANSLATION-TABLE declaration
// the SUT owner did NOT set explicitly (so the suite silently falls back to a default). Onboarding
// policy: a SUT owner must declare all four explicitly (onboarding/HOW-TO-ARGUS-CONFIG.md) — this
// surfaces reliance-on-a-default WITHOUT failing (validate-config still passes; these are warnings).
func (c *Config) LogFieldMappingWarnings() []string {
	var w []string
	nudge := func(got, key, def string) {
		if strings.TrimSpace(got) == "" {
			w = append(w, fmt.Sprintf("you're relying on the default for observability.loki.%s (default: %q) — declare it explicitly (onboarding/HOW-TO-ARGUS-CONFIG.md → Log-field translation table)", key, def))
		}
	}
	nudge(c.Observability.Loki.CorrelationField, "correlation_field", defaultCorrelationField)
	nudge(c.Observability.Loki.LevelField, "level_field", defaultLevelField)
	nudge(c.Observability.Loki.SagaEventField, "saga_event_field", defaultSagaEventField)
	nudge(c.Observability.Loki.SagaEventValue, "saga_event_value", defaultSagaEventValue)
	return w
}

// ProjectLabel is the Loki `project` stream-label value (RO-05); defaults to project.name.
func (c *Config) ProjectLabel() string {
	if c.Observability.Loki.ProjectLabel != "" {
		return c.Observability.Loki.ProjectLabel
	}
	return c.Project.Name
}

// DeriveProjectFromContainer relabels project from the compose-project container label (RO-05).
func (c *Config) DeriveProjectFromContainer() bool {
	return c.Observability.Loki.DeriveProjectFromContainer
}

// PromTargets are the explicit SUT scrape targets (RO-07).
func (c *Config) PromTargets() []string { return c.Observability.Prometheus.Targets }

// PromDiscoverByProject selects Docker-SD scraping keyed on the SUT project (RO-07).
func (c *Config) PromDiscoverByProject() bool { return c.Observability.Prometheus.DiscoverByProject }

// PromEnabled reports whether the SUT declares a Prometheus /metrics surface (CHANGE-4).
func (c *Config) PromEnabled() bool {
	return c.Observability.Prometheus.Enabled != nil && *c.Observability.Prometheus.Enabled
}

// DeclaredGaps returns the SUT owner's honest declared-gap lines (CHANGE-4); nil when none.
func (c *Config) DeclaredGaps() []string { return c.DeclaredGapsList }

// MCPAuthType is the declared MCP auth mode for the capability report (CHANGE-4): the
// explicit targets.mcp.auth.type, else bearer when a token is present, else none.
func (c *Config) MCPAuthType() string {
	if c.Targets.MCP != nil && c.Targets.MCP.Auth != nil && strings.TrimSpace(c.Targets.MCP.Auth.Type) != "" {
		return strings.TrimSpace(c.Targets.MCP.Auth.Type)
	}
	if c.MCPToken() != "" {
		return "bearer"
	}
	return "none"
}

// ── MCP target resolvers (CHANGE-1) ──────────────────────────────────────────
// The MCP endpoint/transport/token are declared in targets.mcp; a per-scenario
// TRIGGER/server_url value still wins as an explicit override (resolved upstream).

const defaultMCPTransport = "streamable-http"

// defaultMCPTimeout is the per-call MCP deadline when the SUT declares none (GAP-3).
const defaultMCPTimeout = 30 * time.Second

// MCPBaseURL is the SUT MCP endpoint from targets.mcp (e.g. social-gateway:8080),
// resolved from the runner's SUT-joined vantage. "" when no targets.mcp is declared.
func (c *Config) MCPBaseURL() string { return c.Targets.MCP.URL() }

// MCPTransport is the SUT's declared MCP transport; default streamable-http (never guessed).
func (c *Config) MCPTransport() string { return c.Targets.MCP.TransportOrDefault() }

// The entry-level accessors below answer for the plain slot AND for a named entry alike, so a
// scenario that selected `mcp_targets.<name>` gets exactly the plain slot's semantics. All are
// nil-safe: a nil entry answers with the defaults.

// URL is the entry's trimmed base_url; "" for a nil entry.
func (t *MCPTarget) URL() string {
	if t == nil {
		return ""
	}
	return strings.TrimSpace(t.BaseURL)
}

// TransportOrDefault is the entry's declared transport, else streamable-http (never guessed).
func (t *MCPTarget) TransportOrDefault() string {
	if t != nil && strings.TrimSpace(t.Transport) != "" {
		return strings.TrimSpace(t.Transport)
	}
	return defaultMCPTransport
}

// TimeoutOrDefault is the entry's per-call deadline, else 30s (GAP-3; "0" is never "no timeout").
func (t *MCPTarget) TimeoutOrDefault() time.Duration {
	if t != nil && t.TimeoutSeconds > 0 {
		return time.Duration(t.TimeoutSeconds) * time.Second
	}
	return defaultMCPTimeout
}

// Token is the entry's static bearer from its auth block; "" = none.
func (t *MCPTarget) Token() string {
	if t != nil && t.Auth != nil {
		return strings.TrimSpace(t.Auth.BearerToken)
	}
	return ""
}

// SagaStepFields maps a LOGICAL saga step field to this SUT's name for it (GAP-1); nil when the
// SUT declares none, in which case the reader uses the the operator defaults plus its fallback chain.
func (c *Config) SagaStepFields() map[string]string {
	return c.Observability.Loki.SagaStepFields
}

// ── observability.betterstack accessors (AC-D13) ─────────────────────────────

// UseBetterStack reports whether observability.betterstack is declared (non-zero). Exactly one
// of loki/betterstack may be declared (validateObservabilityBackend enforces it at load time);
// neither declared keeps the unchanged Loki-default behaviour.
func (c *Config) UseBetterStack() bool {
	return !reflect.ValueOf(c.Observability.BetterStack).IsZero()
}

// NoObservabilityBackend reports whether this SUT declares NEITHER observability.loki NOR
// observability.betterstack — the "no obs plane declared" signal (item 8b, msgbus tester
// 2026-09-28: "on tier managed, do not require observability.grafana.public_url when
// observability is none/absent"). It is deliberately about the DECLARED backend, never about
// --obs (a render-time/deploy-time choice this package has no view of): a bundled deployment
// legitimately declares neither loki nor betterstack and still has a real Loki running, so this
// predicate must NOT be read as "obs is absent at runtime" — only the executor's own env
// (Loki/Pushgateway URLs, resolved at run time) can answer that question. It is used only where
// "this file names no obs backend" is itself the fact that matters — e.g. a chain/mcp-only kit
// with nothing to link a dashboard to.
func (c *Config) NoObservabilityBackend() bool {
	return reflect.ValueOf(c.Observability.Loki).IsZero() && !c.UseBetterStack()
}

// BetterStackQueryURL is the HTTP SQL query endpoint.
func (c *Config) BetterStackQueryURL() string {
	return strings.TrimSpace(c.Observability.BetterStack.QueryURL)
}

// BetterStackCredential is the RESOLVED "username:password" Basic-Auth credential (the raw config
// value is a ${VAR} reference; Load() resolves it via expandEnv — see walkCredentialFields).
func (c *Config) BetterStackCredential() string {
	return c.Observability.BetterStack.Credential
}

// BetterStackTeamID is the BetterStack team id (t<team_id>_<source>_logs).
func (c *Config) BetterStackTeamID() string {
	return strings.TrimSpace(c.Observability.BetterStack.TeamID)
}

// BetterStackSources maps a source slug/id to the service name it carries (one source per
// service — BetterStack has no cross-service source, unlike Loki's single stream).
func (c *Config) BetterStackSources() map[string]string {
	return c.Observability.BetterStack.Sources
}

// BetterStackCorrelationFields is the ordered list of JSON field paths tried to read the
// correlation id out of a log line; declared list wins, else the default two-path chain.
func (c *Config) BetterStackCorrelationFields() []string {
	if len(c.Observability.BetterStack.CorrelationFields) > 0 {
		return c.Observability.BetterStack.CorrelationFields
	}
	return defaultBetterStackCorrelationFields
}

// BetterStackLevelField / BetterStackSagaEventField / BetterStackSagaEventValues carry the SAME
// meaning as their observability.loki counterparts (LevelField / SagaEventField / SagaEventValues)
// — a flat field name read out of the log line's parsed JSON body, not a dotted path.
func (c *Config) BetterStackLevelField() string {
	if c.Observability.BetterStack.LevelField != "" {
		return c.Observability.BetterStack.LevelField
	}
	return defaultLevelField
}
func (c *Config) BetterStackSagaEventField() string {
	if c.Observability.BetterStack.SagaEventField != "" {
		return c.Observability.BetterStack.SagaEventField
	}
	return defaultSagaEventField
}
func (c *Config) BetterStackSagaEventValues() []string {
	seen := map[string]bool{}
	var out []string
	add := func(v string) {
		if v = strings.TrimSpace(v); v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	add(c.Observability.BetterStack.SagaEventValue)
	for _, v := range c.Observability.BetterStack.SagaEventValues {
		add(v)
	}
	if len(out) == 0 {
		return []string{defaultSagaEventValue}
	}
	return out
}

// BetterStackSagaStepFields maps a LOGICAL saga step field to this SUT's name for it — same
// meaning/fallback-chain contract as observability.loki.saga_step_fields.
func (c *Config) BetterStackSagaStepFields() map[string]string {
	return c.Observability.BetterStack.SagaStepFields
}

// bsSafeIdentRe is the safe-identifier pattern for a BetterStack source slug/id and team id: they
// are built directly into a `remote(t<team>_<source>_logs)` SQL table reference, so only bare
// alphanumerics/underscore are accepted — never interpolated raw from an unchecked value.
var bsSafeIdentRe = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

// bsFieldPathRe is the safe pattern for a BetterStack field path/name (correlation_fields,
// level_field, saga_event_field, saga_step_fields values): dot-separated identifier segments,
// built into JSONExtractString(raw, 'seg', ...) SQL fragments — never interpolated raw.
var bsFieldPathRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)*$`)

// bsCredentialVarRe requires the ENTIRE credential field to be exactly one ${VAR} reference — a
// literal secret (or a ${VAR} embedded in other text) is refused (REQUIRED 1: "never a literal").
var bsCredentialVarRe = regexp.MustCompile(`^\$\{[A-Za-z_][A-Za-z0-9_]*\}$`)

// validateObservabilityBackend enforces AC-D13's exclusivity rule: observability.loki and
// observability.betterstack may not BOTH be declared (naming both blocks, never a generic "bad
// config"). Neither declared is unchanged behaviour (Loki's bundled defaults apply) — this is not
// an error, since it is every pre-existing argus-config's shape. When betterstack IS declared, its
// own fields are validated here too (load time IS validate time — VR10-R1-1 precedent).
func (c *Config) validateObservabilityBackend() error {
	lokiSet := !reflect.ValueOf(c.Observability.Loki).IsZero()
	bsSet := c.UseBetterStack()
	if lokiSet && bsSet {
		return errors.New("observability: set exactly one of observability.loki or observability.betterstack — both are declared")
	}
	// Spec 26: the OpenShell evidence read goes to the executor's --loki
	// (toolcore.evidenceFor → lokiFor). A BetterStack instance has no Loki, so the block would report
	// `unavailable` on every row of every run. Refused here, where the operator can still change it.
	if bsSet && c.Observability.OpenShell != nil {
		return errors.New("observability.openshell reads Loki; this config selects observability.betterstack, which has no Loki to read")
	}
	// T3.1 (E3 export hosted-Loki target): observability.loki.credential, when declared, must be a
	// single ${VAR} reference — never a literal — same rule and wording style as
	// observability.betterstack.credential below. Checked unconditionally (not gated on bsSet, and
	// not gated on --obs mode: config.Load has no view of the CLI flag, so a literal is refused
	// whether or not export is the mode in use today).
	if cred := strings.TrimSpace(c.Observability.Loki.Credential); cred != "" && !bsCredentialVarRe.MatchString(cred) {
		return errors.New("observability.loki.credential must be a single ${VAR} reference (e.g. ${LOKI_CREDENTIAL}) — never a literal credential committed to the file")
	}
	if !bsSet {
		return nil
	}
	b := &c.Observability.BetterStack
	var errs []string
	if strings.TrimSpace(b.QueryURL) == "" {
		errs = append(errs, "observability.betterstack.query_url is required — the HTTP SQL query endpoint")
	}
	if strings.TrimSpace(b.TeamID) == "" {
		errs = append(errs, "observability.betterstack.team_id is required")
	} else if !bsSafeIdentRe.MatchString(strings.TrimSpace(b.TeamID)) {
		errs = append(errs, fmt.Sprintf("observability.betterstack.team_id: %q is not a valid team id — accepted pattern %s", b.TeamID, bsSafeIdentRe.String()))
	}
	cred := strings.TrimSpace(b.Credential)
	switch {
	case cred == "":
		errs = append(errs, "observability.betterstack.credential is required — a ${VAR} reference to the query credential, never a literal")
	case !bsCredentialVarRe.MatchString(cred):
		errs = append(errs, "observability.betterstack.credential must be a single ${VAR} reference (e.g. ${BETTERSTACK_CREDENTIAL}) — never a literal credential committed to the file")
	}
	if len(b.Sources) == 0 {
		errs = append(errs, "observability.betterstack.sources is required — at least one source slug/id mapped to the service name it carries")
	}
	for _, slug := range sortedKeys(b.Sources) {
		if !bsSafeIdentRe.MatchString(slug) {
			errs = append(errs, fmt.Sprintf("observability.betterstack.sources: %q is not a valid source name — accepted pattern %s", slug, bsSafeIdentRe.String()))
		}
	}
	checkPath := func(key, path string) {
		if path != "" && !bsFieldPathRe.MatchString(path) {
			errs = append(errs, fmt.Sprintf("observability.betterstack.%s: %q is not a valid field path — accepted pattern %s", key, path, bsFieldPathRe.String()))
		}
	}
	for _, p := range b.CorrelationFields {
		checkPath("correlation_fields", p)
	}
	checkPath("level_field", b.LevelField)
	checkPath("saga_event_field", b.SagaEventField)
	for _, k := range sortedKeys(b.SagaStepFields) {
		checkPath("saga_step_fields."+k, b.SagaStepFields[k])
	}
	if len(errs) == 0 {
		return nil
	}
	return errors.New(strings.Join(errs, "; "))
}

// LogFormat is the SUT's declared log encoding for the promtail pipeline: "logfmt" when declared
// as such, else "json" (the default and the the operator convention).
func (c *Config) LogFormat() string {
	if strings.EqualFold(strings.TrimSpace(c.Observability.Loki.LogFormat), "logfmt") {
		return "logfmt"
	}
	return "json"
}

// MCPTimeout is the per-call MCP client deadline (GAP-3): the SUT's declared
// targets.mcp.timeout_seconds, else 30s. A non-positive declaration falls back to the default —
// "0" must never be read as "no timeout", which would hang a run forever on an unresponsive SUT.
func (c *Config) MCPTimeout() time.Duration { return c.Targets.MCP.TimeoutOrDefault() }

// MCPToken is the SUT's static MCP bearer from targets.mcp.auth; "" = none.
func (c *Config) MCPToken() string { return c.Targets.MCP.Token() }

// layerToConfig mirrors argus preflight.py LAYER_TO_CONFIG (the runtime authority).
var layerToConfig = map[string]string{
	"HTTP Ingestion":       "http",
	"Message Flow":         "message_broker",
	"Database State":       "database",
	"External Delivery":    "external",
	"Error Path":           "", // derives from http/db
	"Rate Limiting":        "http",
	"Permissions":          "auth",
	scenario.AMQPLoadLayer: "message_broker", // a load ramp drives a broker target
	scenario.HTTPLoadLayer: "http",           // the HTTP ramp drives an http target
}

// TargetForLayer returns the argus-config target key a test-layer needs (e.g. "Message Flow" →
// "message_broker"), and whether the layer maps to a concrete target. Exported so the cloud
// validate_scenario coverability check (UC030) can compare a scenario's layers to an instance's
// DECLARED targets.
func TargetForLayer(layer string) (string, bool) {
	t, ok := layerToConfig[layer]
	return t, ok && t != ""
}

// BusGuardrailWarnings enforces the bus rule (D-EXEC.3.6 / UC163): "share the eyes, isolate the hands"
// — a SUT under test must use its OWN message broker, never a shared or production bus. It warns when
// the declared message_broker URL / management URL looks shared or production.
func (c *Config) BusGuardrailWarnings() []string {
	if c.Targets.MessageBroker == nil {
		return nil
	}
	var w []string
	for _, f := range []struct{ label, url string }{
		{"url", c.Targets.MessageBroker.URL},
		{"management_url", c.Targets.MessageBroker.ManagementURL},
	} {
		if looksSharedOrProd(f.url) {
			w = append(w, fmt.Sprintf("message_broker.%s %q looks shared/production — the bus rule is \"share the eyes, isolate the hands\": give the SUT its OWN broker (per-instance/compose), never a shared or production bus (D-EXEC.3.6)", f.label, f.url))
		}
	}
	return w
}

// looksSharedOrProd reports whether a URL carries a shared/production segment (delimited, so
// "product-service" does NOT match but "rabbitmq.prod.internal" does).
func looksSharedOrProd(u string) bool {
	if u == "" {
		return false
	}
	segs := strings.FieldsFunc(strings.ToLower(u), func(r rune) bool {
		return !('a' <= r && r <= 'z' || '0' <= r && r <= '9')
	})
	for _, s := range segs {
		switch s {
		case "prod", "production", "prd", "shared":
			return true
		}
	}
	return false
}

func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c, err := parseConfig(b, path)
	if err != nil {
		return nil, err
	}
	// D1/D2 (SECRETS-VAR-PARITY): resolve ${VAR} in the SUT wiring/credential fields from
	// the process environment; a referenced-but-unset ${VAR} fails loud (never send an empty
	// credential). The value lives in the PRODUCT folder's .env (the onboarder injects the
	// referenced names from there) or a shell export on direct host runs — never the committed
	// config (CLAUDE.md ${VAR} rule + spec-20 FR-L9).
	if missing := c.expandEnv(); len(missing) > 0 {
		return nil, fmt.Errorf("unresolved ${VAR} in %s — set %s (add the value to the product folder's .env — the onboarder loads the ${VAR} names your argus-config references from there; for direct host runs export it; never commit the value)", path, strings.Join(missing, "; "))
	}
	return c, nil
}

// ParseUnresolved reads + unmarshals an argus-config WITHOUT resolving ${VAR}s (no D1
// hard-fail) — for preflight tooling (secrets-scan) whose whole job is to inspect the
// references BEFORE the values exist. Everything else must use Load.
func ParseUnresolved(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseConfig(b, path)
}

// parseConfig is the one decode both loaders share: the lenient whole-file unmarshal (every block
// outside `targets` is unchanged in this build), then the strict pass over the targets node, then
// the entry-name rules. A config that fails here never reaches a run, a probe or the 2b preflight.
func parseConfig(b []byte, path string) (*Config, error) {
	var c Config
	// a `load_test` key that is WRITTEN must say `never`. Checked on the document itself,
	// before the typed decode: an empty value decodes to "" (the same as an absent key) and would read as
	// "no guard", and a list or a map would be refused without naming the key.
	if err := strictLoadTestValues(b); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := strictTargets(b); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := c.Targets.checkNames(); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	// S9: every load_allowed_targets entry names a real broker target, is bounded, and
	// is not a shared/production-looking broker. Load time IS validate time.
	if err := c.validateLoadAllowed(); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	// VR10-R1-1: a malformed rate_limit declaration fails LOUDLY here — load time IS validate time
	// (validate-config loads the file, and onboard.sh dies on a failed validate-config).
	if err := c.RateLimit.validate(); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	// AC-36: a declared package is checked at load time for the same reason — a path that is not
	// there is refused by key and path here, where validate-config reports it, not at check time.
	if err := c.Package.validate(filepath.Dir(path)); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	// money_writes (2026-09-26 follow-up): valid only beside money_handling: true, and every entry
	// checked in full — load time IS validate time, the same rule RateLimit/Package just followed.
	if err := c.MoneyWrites.validate(c.MoneyHandling); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	// UI-7a: summary_metrics is checked in full at load time too, and its keys are
	// strict INSIDE the block only (a mistyped key must not parse to an empty query).
	if err := strictSummaryMetrics(b); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := c.SummaryMetrics.validate(); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	// AC-D13: observability.loki / observability.betterstack are mutually exclusive, and a
	// declared betterstack block is checked in full here — same load-time-is-validate-time rule.
	if err := c.validateObservabilityBackend(); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	// Spec 26 (A1): a declared observability.openshell block is checked in full here — every problem
	// named at once, same load-time-is-validate-time rule. Absent (nil) is the feature switched off.
	if o := c.Observability.OpenShell; o != nil {
		if err := o.validate(); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
	}
	// observability.pushgateway.group_retention is checked at load, so a garbage or
	// negative value is refused by name rather than silently falling back.
	if err := c.Observability.Pushgateway.validate(); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	// message_schemas (design §1): every declared schema is parsed here, relative to the config
	// file's directory — the same load-time rule as Package/RateLimit/MoneyWrites. validate-config
	// (which just loads the file) is what makes this "reports a schema that fails to parse by name".
	if err := c.MessageSchemas.validate(filepath.Dir(path)); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	// test_targets: <= 8, unique valid names, a non-empty match each — load time IS
	// validate time, like every block above.
	if err := c.TestTargets.Validate(); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	// observability.dashboard_link (, UI-2): absolute http(s), no userinfo, no ${VAR}, the
	// six placeholders only, <= 2048 bytes -- load time IS validate time.
	if err := c.validateDashboardLink(); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	// check_env: every declared entry is an environment variable NAME — load time IS
	// validate time. The refusal never echoes the entry.
	if err := c.validateCheckEnv(); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &c, nil
}

// strictTargetsDoc decodes `targets` with KnownFields(true) and absorbs every OTHER top-level key
// into the inline map, so strictness starts and stops at the targets node (SA §0.14 S3-a: turned
// on for `targets` only; widening it to other blocks is round-11 hygiene).
type strictTargetsDoc struct {
	Targets Targets `yaml:"targets"`
	// VR10-R1: `rate_limit` joins the strict node for the same reason `targets` did — a key that
	// parses and reaches nothing looks supported. The refusal names the key AND the place
	// (rate_limit / rate_limit.signature / rate_limit.pause).
	RateLimit *RateLimit `yaml:"rate_limit"`
	// AC-36: `package` joins the strict node too — a mistyped `manifest:` that parsed and reached
	// nothing would certify a package the operator never declared.
	Package *Package `yaml:"package"`
	// money_writes (2026-09-26 follow-up) joins the strict node for the same reason: a mistyped key
	// inside an allow entry (e.g. "methd") would otherwise parse to its zero value and silently
	// refuse to exempt anything, which reads as "the guard is broken" rather than "the key was wrong".
	MoneyWrites *MoneyWritesConfig `yaml:"money_writes"`
	// message_schemas joins the strict node too — a mistyped key inside a declaration (e.g. "paht")
	// would otherwise parse to a zero value and refuse "declare exactly one of path or inline" in a
	// way that hides the real typo.
	MessageSchemas MessageSchemas `yaml:"message_schemas"`
	// load_allowed_targets joins the strict node for the same reason: a mistyped `max_sesions` would
	// silently lose the operator's ceiling. (An OLD executor never saw this field: the key falls into its
	// inline map below and is ignored -- TestLoadAllowed_OldExecutorDecodesIgnoring.)
	LoadAllowedTargets map[string]LoadAllow `yaml:"load_allowed_targets"`
	// test_targets joins the strict node for the same reason: a mistyped key inside an entry (e.g.
	// "namespce") would otherwise parse to a zero value and silently drop the field. Strict only for
	// binaries that know the key; an older executor never sees it as strict (it is in its inline Rest).
	TestTargets testtargets.List `yaml:"test_targets"`
	// Spec 26 (finding A1): observability.openshell joins the strict node. ONLY that block — every
	// other key under `observability` lands in strictObservability.Rest and stays lenient, exactly as
	// before (TestOpenShellObs_OtherObservabilityKeysStayLenient pins it).
	Observability *strictObservability `yaml:"observability"`
	Rest          map[string]any       `yaml:",inline"`
}

// strictObservability decodes `observability` for the strict pass: the openshell block with
// KnownFields, everything else absorbed and unchecked.
type strictObservability struct {
	OpenShell *OpenShellObs `yaml:"openshell"`
	// observability.pushgateway joins the strict node (url + group_retention), so a
	// mistyped `group_retenton` is refused by name instead of silently keeping the 15m default.
	Pushgateway *PushgatewayObs `yaml:"pushgateway"`
	Rest        map[string]any  `yaml:",inline"`
}

// strictTargets refuses an unknown key anywhere under `targets` — the node itself, a plain slot, a
// named entry — by name, with the line and the accepted keys for that place (VR10-S3-1).
func strictTargets(b []byte) error {
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	var d strictTargetsDoc
	err := dec.Decode(&d)
	if err == nil || errors.Is(err, io.EOF) {
		return nil
	}
	var te *yaml.TypeError
	if !errors.As(err, &te) {
		return err
	}
	msgs := make([]string, 0, len(te.Errors))
	for _, e := range te.Errors {
		msgs = append(msgs, plainUnknownKey(e))
	}
	return errors.New(strings.Join(msgs, "; "))
}

// unknownFieldRe matches yaml.v3's known-fields refusal ("line 7: field graph_health not found in
// type config.Targets") so it can be said in plain English with the accepted keys.
var unknownFieldRe = regexp.MustCompile(`^line (\d+): field (\S+) not found in type (\S+)$`)

// targetPlaces maps each strict struct to the place an operator knows it by. The accepted keys are
// read from the struct tags, so this table can never drift from the shape it describes.
var targetPlaces = []struct {
	t     reflect.Type
	place string
}{
	{reflect.TypeOf(Targets{}), "targets"},
	{reflect.TypeOf(HTTPTarget{}), "an http target (targets.http or targets.http_targets.<name>)"},
	{reflect.TypeOf(MCPTarget{}), "an mcp target (targets.mcp or targets.mcp_targets.<name>)"},
	{reflect.TypeOf(MCPAuth{}), "an mcp target's auth block"},
	{reflect.TypeOf(DBTarget{}), "a database target (targets.database or targets.database_targets.<name>)"},
	{reflect.TypeOf(MQTarget{}), "a message_broker target (targets.message_broker or targets.message_broker_targets.<name>)"},
	{reflect.TypeOf(AuthTarget{}), "targets.auth"},
	{reflect.TypeOf(RateLimit{}), "rate_limit"},
	{reflect.TypeOf(RateLimitSignature{}), "rate_limit.signature"},
	{reflect.TypeOf(RateLimitPause{}), "rate_limit.pause"},
	{reflect.TypeOf(Package{}), "package"},
	{reflect.TypeOf(LoadAllow{}), "a load_allowed_targets entry"},
	{reflect.TypeOf(testtargets.Target{}), "test_targets"},
	{reflect.TypeOf(testtargets.Match{}), "a test_targets entry's match"},
	{reflect.TypeOf(OpenShellObs{}), "observability.openshell"},
	{reflect.TypeOf(PushgatewayObs{}), "observability.pushgateway"},
}

func plainUnknownKey(e string) string {
	m := unknownFieldRe.FindStringSubmatch(e)
	if m == nil {
		return e
	}
	for _, tp := range targetPlaces {
		if tp.t.String() == m[3] {
			return fmt.Sprintf("line %s: unknown key %q under %s — accepted: %s", m[1], m[2], tp.place, strings.Join(yamlKeys(tp.t), ", "))
		}
	}
	return e
}

// yamlKeys lists a struct's yaml keys in declaration order.
func yamlKeys(t reflect.Type) []string {
	var out []string
	for i := 0; i < t.NumField(); i++ {
		if name := strings.Split(t.Field(i).Tag.Get("yaml"), ",")[0]; name != "" && name != "-" {
			out = append(out, name)
		}
	}
	return out
}

// plainSlotKeys are the keys under `targets` that already mean something; a named entry may not
// take one of them (VR10-S3-3), nor `default` — the plain slot IS the default.
var plainSlotKeys = []string{"http", "mcp", "database", "message_broker", "external", "auth"}

func isPlainSlotKey(n string) bool {
	for _, k := range plainSlotKeys {
		if n == k {
			return true
		}
	}
	return false
}

// checkNames applies the entry-name rules to every named map and refuses an entry that declares
// nothing (a nil or all-empty entry would otherwise be dereferenced at selection time).
func (t *Targets) checkNames() error {
	var errs []string
	check := func(kind string, names []string, isEmpty func(string) bool) {
		sort.Strings(names)
		for _, n := range names {
			label := "targets." + kind + "." + n
			switch {
			case strings.TrimSpace(n) == "":
				errs = append(errs, fmt.Sprintf("targets.%s has an entry with an empty name — give it a name", kind))
			case n == "default":
				errs = append(errs, fmt.Sprintf("%s: the name `default` is refused — the plain `%s:` slot IS the default; give this entry its own name", label, strings.TrimSuffix(kind, "_targets")))
			case isPlainSlotKey(n):
				errs = append(errs, fmt.Sprintf("%s: the name %q collides with a plain-slot key (%s) — give this entry its own name", label, n, strings.Join(plainSlotKeys, ", ")))
			case isEmpty(n):
				errs = append(errs, fmt.Sprintf("%s declares nothing — fill its fields or remove it", label))
			}
		}
	}
	check("http_targets", sortedKeys(t.HTTPTargets), func(n string) bool { return t.HTTPTargets[n] == nil || reflect.ValueOf(*t.HTTPTargets[n]).IsZero() })
	check("mcp_targets", sortedKeys(t.MCPTargets), func(n string) bool { return t.MCPTargets[n] == nil || reflect.ValueOf(*t.MCPTargets[n]).IsZero() })
	check("database_targets", sortedKeys(t.DatabaseTargets), func(n string) bool {
		return t.DatabaseTargets[n] == nil || reflect.ValueOf(*t.DatabaseTargets[n]).IsZero()
	})
	check("message_broker_targets", sortedKeys(t.MessageBrokerTargets), func(n string) bool {
		return t.MessageBrokerTargets[n] == nil || reflect.ValueOf(*t.MessageBrokerTargets[n]).IsZero()
	})
	if len(errs) == 0 {
		return nil
	}
	return errors.New(strings.Join(errs, "; "))
}

func sortedKeys[M ~map[string]V, V any](m M) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// envVarRe matches a ${NAME} placeholder. Braces are REQUIRED — a bare $ in a password/URL is
// left untouched. Mirrors argus.resolveVars' pattern; duplicated here because config cannot
// import argus (that would be an import cycle).
var envVarRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// runtimeVars are per-run placeholders resolved LATER in internal/argus (resolveVars; the HTTP path's config token gets fillCorrelationID alone, V31-003) — not environment
// variables. expandEnv leaves them literal and never flags them as missing.
var runtimeVars = map[string]bool{"cid": true, "correlation_id": true}

// expandEnv resolves ${VAR} references in the SUT wiring/credential fields (D2 full parity:
// HTTP auth token, MCP token + base_url, DB creds, broker URL) from the process environment.
// A referenced-but-unset OR set-but-empty ${VAR} is collected as a missing reference (D1: Load
// turns a non-empty result into a hard error). ${cid}/${correlation_id} are runtime placeholders
// and left literal. Returns the missing references as "NAME (referenced by <field>)" strings —
// it NEVER returns a resolved secret value.
func (c *Config) expandEnv() []string {
	var missing []string
	c.walkCredentialFields(func(field, label string) string {
		return envVarRe.ReplaceAllStringFunc(field, func(m string) string {
			name := envVarRe.FindStringSubmatch(m)[1]
			if runtimeVars[name] {
				return m // runtime placeholder — resolved at run time, not here
			}
			if v, ok := os.LookupEnv(name); ok && v != "" {
				return v
			}
			missing = append(missing, fmt.Sprintf("%s (referenced by %s)", name, label))
			return m
		})
	})
	// check_env: a declared name is required in the environment exactly like a credential reference.
	missing = append(missing, c.declaredEnvMissing()...)
	return missing
}

// EnvRef is one ${VAR} reference in a D2-parity credential/wiring field.
type EnvRef struct {
	Name  string `json:"name"`
	Field string `json:"field"`
}

// EnvRefs reports every ${VAR} the config references in the D2-parity fields, in field
// order, runtime placeholders excluded — WITHOUT resolving anything. Call it on a
// ParseUnresolved config (after Load the placeholders are already substituted away).
// It never reads the environment and never returns a value.
func (c *Config) EnvRefs() []EnvRef {
	var refs []EnvRef
	c.walkCredentialFields(func(field, label string) string {
		for _, m := range envVarRe.FindAllStringSubmatch(field, -1) {
			if runtimeVars[m[1]] {
				continue
			}
			refs = append(refs, EnvRef{Name: m[1], Field: label})
		}
		return field
	})
	// check_env: the declared names ride the same list, so every consumer
	// (secrets-scan, the onboarding preflight, doctor) carries them without knowing the key exists.
	// A name a credential field already references is listed once, under that field.
	refs = append(refs, c.declaredEnvRefs()...)
	return refs
}

// walkCredentialFields visits the enumerated wiring/credential fields (D2 full parity;
// http.base_url + broker management_url added by the secrets-preflight gate — both can carry
// embedded creds), assigning back whatever the visitor returns — the single authority on
// WHICH fields carry ${VAR}s, shared by expandEnv (resolve) and EnvRefs (report).
// (targets.external is an untyped `any` and is NOT walked — documented in HOW-TO.)
//
// VR10-S3-9: every NAMED entry is walked too, visiting the SAME fields per kind, with labels that
// name the entry (`targets.database_targets.neo4j.password`) so the masked preview and the
// missing-name refusal say WHICH target. This is the one walker — do not add a second beside it.
func (c *Config) walkCredentialFields(repl func(field, label string) string) {
	http := func(t *HTTPTarget, at string) {
		t.BaseURL = repl(t.BaseURL, at+".base_url")
	}
	mcp := func(t *MCPTarget, at string) {
		t.BaseURL = repl(t.BaseURL, at+".base_url")
		if t.Auth != nil {
			t.Auth.BearerToken = repl(t.Auth.BearerToken, at+".auth.bearer_token")
		}
	}
	db := func(t *DBTarget, at string) {
		t.Username = repl(t.Username, at+".username")
		t.Password = repl(t.Password, at+".password")
		t.JDBCURL = repl(t.JDBCURL, at+".jdbc_url")
	}
	mq := func(t *MQTarget, at string) {
		t.URL = repl(t.URL, at+".url")
		t.ManagementURL = repl(t.ManagementURL, at+".management_url")
	}
	if c.Targets.HTTP != nil {
		http(c.Targets.HTTP, "targets.http")
	}
	if c.Targets.Auth != nil {
		c.Targets.Auth.BearerToken = repl(c.Targets.Auth.BearerToken, "targets.auth.bearer_token")
	}
	if c.Targets.MCP != nil {
		mcp(c.Targets.MCP, "targets.mcp")
	}
	if c.Targets.Database != nil {
		db(c.Targets.Database, "targets.database")
	}
	if c.Targets.MessageBroker != nil {
		mq(c.Targets.MessageBroker, "targets.message_broker")
	}
	for _, n := range sortedKeys(c.Targets.HTTPTargets) {
		if t := c.Targets.HTTPTargets[n]; t != nil {
			http(t, "targets.http_targets."+n)
		}
	}
	for _, n := range sortedKeys(c.Targets.MCPTargets) {
		if t := c.Targets.MCPTargets[n]; t != nil {
			mcp(t, "targets.mcp_targets."+n)
		}
	}
	for _, n := range sortedKeys(c.Targets.DatabaseTargets) {
		if t := c.Targets.DatabaseTargets[n]; t != nil {
			db(t, "targets.database_targets."+n)
		}
	}
	for _, n := range sortedKeys(c.Targets.MessageBrokerTargets) {
		if t := c.Targets.MessageBrokerTargets[n]; t != nil {
			mq(t, "targets.message_broker_targets."+n)
		}
	}
	// AC-D13: observability.betterstack.credential is a ${VAR}-only field, resolved through the
	// SAME expandEnv mechanism (fails loud on a missing/empty var) as every other credential here.
	c.Observability.BetterStack.Credential = repl(c.Observability.BetterStack.Credential, "observability.betterstack.credential")
	// T3.1: observability.loki.credential is a ${VAR}-only field too (export hosted-Loki target),
	// resolved through the SAME mechanism — see LokiCredential / validateObservabilityBackend.
	c.Observability.Loki.Credential = repl(c.Observability.Loki.Credential, "observability.loki.credential")
	// UI-7a: summary_metrics.source.credential is a ${VAR}-only field, resolved the same way.
	if c.SummaryMetrics != nil {
		c.SummaryMetrics.Source.Credential = repl(c.SummaryMetrics.Source.Credential, "summary_metrics.source.credential")
	}
	// Spec 26 (finding A2): observability.openshell.sandbox is wiring, not a credential, but it is
	// ${SUT_SANDBOX_UID} in every documented config, and this walker is the single authority on which
	// fields expand — without this line the literal "${SUT_SANDBOX_UID}" would be the sandbox id, and
	// EnvRefs (the onboarding preflight's name list) would never ask for the variable.
	if o := c.Observability.OpenShell; o != nil {
		o.Sandbox = repl(o.Sandbox, "observability.openshell.sandbox")
	}
}

// ── Named-target selection (VR10-S3 / V28-012) ───────────────────────────────

// The four kinds a scenario can select a named entry of. `external` and `auth` have no named form.
const (
	KindHTTP          = "http"
	KindMCP           = "mcp"
	KindDatabase      = "database"
	KindMessageBroker = "message_broker"
)

// TargetSelection is the entry a scenario's **Target** word selected — exactly one of the four
// pointers is set, matching Kind. A scenario without the word selects nothing (nil), and the plain
// slots stay the default.
type TargetSelection struct {
	Kind string
	Name string
	HTTP *HTTPTarget
	MCP  *MCPTarget
	DB   *DBTarget
	MQ   *MQTarget
}

// NamedTargets lists the declared entry names of a kind, sorted.
func (c *Config) NamedTargets(kind string) []string {
	switch kind {
	case KindHTTP:
		return sortedKeys(c.Targets.HTTPTargets)
	case KindMCP:
		return sortedKeys(c.Targets.MCPTargets)
	case KindDatabase:
		return sortedKeys(c.Targets.DatabaseTargets)
	case KindMessageBroker:
		return sortedKeys(c.Targets.MessageBrokerTargets)
	}
	return nil
}

// DeclaresNamedTarget reports whether the environment declares a named connection target called name under ANY kind
// (http, mcp, database, message broker). A comparison member's target is looked up with it before any check fires
// (ARGUS-CMP-11); which KIND a check then needs is the per-scenario selector's job (SelectTarget).
func (c *Config) DeclaresNamedTarget(name string) bool {
	for _, kind := range []string{KindHTTP, KindMCP, KindDatabase, KindMessageBroker} {
		for _, n := range c.NamedTargets(kind) {
			if n == name {
				return true
			}
		}
	}
	return false
}

// HTTPTargetNamed resolves targets.http_targets.<name>; an unknown name is refused with the
// declared names and a nearest match. It NEVER answers with the plain slot.
func (c *Config) HTTPTargetNamed(name string) (*HTTPTarget, error) {
	if t := c.Targets.HTTPTargets[name]; t != nil {
		return t, nil
	}
	return nil, c.unknownTarget(KindHTTP, name)
}

// MCPTargetNamed resolves targets.mcp_targets.<name> (see HTTPTargetNamed).
func (c *Config) MCPTargetNamed(name string) (*MCPTarget, error) {
	if t := c.Targets.MCPTargets[name]; t != nil {
		return t, nil
	}
	return nil, c.unknownTarget(KindMCP, name)
}

// DBTargetNamed resolves targets.database_targets.<name> (see HTTPTargetNamed).
func (c *Config) DBTargetNamed(name string) (*DBTarget, error) {
	if t := c.Targets.DatabaseTargets[name]; t != nil {
		return t, nil
	}
	return nil, c.unknownTarget(KindDatabase, name)
}

// MQTargetNamed resolves targets.message_broker_targets.<name> (see HTTPTargetNamed).
func (c *Config) MQTargetNamed(name string) (*MQTarget, error) {
	if t := c.Targets.MessageBrokerTargets[name]; t != nil {
		return t, nil
	}
	return nil, c.unknownTarget(KindMessageBroker, name)
}

// unknownTarget is the one refusal shape for a name that does not exist under the kind the scenario
// selects: the declared names, a nearest match within Levenshtein distance 2 (S1-a — a wrong hint is
// worse than none), and — when the name lives under ANOTHER kind — which kind, because the word
// means one thing per layer (VR10-S3-5).
func (c *Config) unknownTarget(kind, name string) error {
	declared := c.NamedTargets(kind)
	var msg string
	if len(declared) == 0 {
		msg = fmt.Sprintf("unknown %s target %q — no targets.%s_targets are declared in argus-config.yaml (add targets.%s_targets.%s; a named target is never taken from the plain slot)", kind, name, kind, kind, name)
	} else {
		msg = fmt.Sprintf("unknown %s target %q — declared: %s", kind, name, strings.Join(declared, ", "))
		if n := nearest(name, declared); n != "" {
			msg += fmt.Sprintf(" (did you mean %s?)", n)
		}
	}
	for _, other := range []string{KindHTTP, KindMCP, KindDatabase, KindMessageBroker} {
		if other == kind {
			continue
		}
		for _, n := range c.NamedTargets(other) {
			if n == name {
				msg += fmt.Sprintf("; %q is declared as a %s target — the word means one thing per layer, and this scenario's layer selects an %s target", name, other, kind)
			}
		}
	}
	return errors.New(msg)
}

// nearest returns the declared name within Levenshtein distance 2 of name (ties → the first in
// sorted order), or "" when nothing is close enough.
func nearest(name string, declared []string) string {
	best, bestD := "", 3
	for _, d := range declared {
		if dist := levenshtein(strings.ToLower(name), strings.ToLower(d)); dist < bestD {
			best, bestD = d, dist
		}
	}
	return best
}

func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)]
}

// SelectTarget turns a scenario's **Target** word into the entry of the kind its layer means
// (scenario.TargetKind), refusing by name a missing entry, a wrong-kind name, or a layer the word
// has no meaning on. (nil, nil) when the scenario carries no word. THERE IS NO FALLBACK: a refusal
// selects nothing, and the caller must not run the scenario against the plain slot — that silent
// fallback is how NEO-010's `graph_health:` vanished and the test quietly hit the gateway.
func (c *Config) SelectTarget(s *scenario.Scenario) (*TargetSelection, error) {
	if s == nil || s.Target == "" {
		return nil, nil
	}
	kind, err := scenario.TargetKind(s)
	if err != nil {
		return nil, err
	}
	sel := &TargetSelection{Kind: kind, Name: s.Target}
	switch kind {
	case KindHTTP:
		sel.HTTP, err = c.HTTPTargetNamed(s.Target)
	case KindMCP:
		sel.MCP, err = c.MCPTargetNamed(s.Target)
	case KindDatabase:
		sel.DB, err = c.DBTargetNamed(s.Target)
	case KindMessageBroker:
		sel.MQ, err = c.MQTargetNamed(s.Target)
	default:
		err = fmt.Errorf("**Target** %q: no named form exists for kind %q", s.Target, kind)
	}
	if err != nil {
		return nil, err
	}
	return sel, nil
}

// targetPresent reports whether a targets.<key> section is present + non-empty.
func (c *Config) targetPresent(key string) bool {
	switch key {
	case "http":
		return c.Targets.HTTP != nil && c.Targets.HTTP.BaseURL != ""
	case "message_broker":
		return c.Targets.MessageBroker != nil
	case "database":
		return c.Targets.Database != nil && c.Targets.Database.JDBCURL != ""
	case "external":
		return c.Targets.External != nil
	case "auth":
		return c.Targets.Auth != nil
	case "mcp":
		return c.MCPBaseURL() != ""
	}
	return false
}

// ExternalWebhookBaseURL returns the SUT-declared webhook sink URL from the argus-config's
// `external` block (the MAP form: `external: {webhook_base_url: ...}`), or "" if absent. The
// executor's scenarios reach the sink via ${WEBHOOK_URL}; the k8s render injects this value into the
// executor env so the config is the source of truth instead of a hardcoded compose default (U9).
//
// The `external` block deliberately stays `any`: two shapes exist in the wild — this MAP form and a
// LIST form (`external: [{name, verify_url}]`, OrderService's codebase config). Typing it to one
// struct would break the other, so this accessor reads the map shape and returns "" for the list
// shape (whose URLs are still picked up by walkURLs for the ExternalName aliases).
func (c *Config) ExternalWebhookBaseURL() string {
	m, ok := c.Targets.External.(map[string]any)
	if !ok {
		return ""
	}
	if v, ok := m["webhook_base_url"].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

// isMCPScenario reports whether a scenario triggers the SUT via its MCP endpoint
// (tag `mcp` or `chain`). Such a scenario's wiring lives in targets.mcp, NOT
// targets.http — even though its layer label is "HTTP Ingestion". Tag literals are
// duplicated here (config cannot import argus — that would be an import cycle).
func isMCPScenario(tags []string) bool {
	for _, t := range tags {
		if t == "mcp" || t == "chain" {
			return true
		}
	}
	return false
}

// needsPlainMCPTarget reports whether an mcp/chain scenario reads targets.mcp.base_url at run
// time (AC-D20b). A chain's `http`, `ui` and `amqp` (AC-D18b) steps never touch the MCP endpoint,
// and an mcp step
// with its own `target` resolves targets.mcp_targets instead (chain_scenario.go), so a chain with
// no mcp step that falls back to the plain slot needs no base_url. It errs toward requiring it:
// a plain `mcp` scenario, or a chain payload that does not parse, still needs the slot.
//
// the engine is the one the RUNTIME picks, scenario.NativeEngineTag (first dispatch
// tag in the runner's order, chain before mcp), never "an `mcp` tag somewhere in the list". A check
// tagged `chain, mcp, auth` runs as a chain; deciding on the `mcp` tag refused it at onboarding for a
// slot it never calls.
func needsPlainMCPTarget(s *scenario.Scenario) bool {
	if scenario.NativeEngineTag(s) != scenario.ChainTag {
		return true
	}
	steps, err := scenario.ParseChainSteps(s.Trigger.Payload)
	if err != nil {
		return true
	}
	for _, st := range steps {
		if st.Type == "mcp" && st.Target == "" {
			return true
		}
	}
	return false
}

// TargetPresentExported reports whether a targets.<key> section is present (for the CLI).
func (c *Config) TargetPresentExported(key string) bool { return c.targetPresent(key) }

// ConfigError is one validate-config error (file:line-less; config-scoped).
type ConfigError struct {
	Scenario string `json:"scenario,omitempty"`
	Message  string `json:"message"`
}

// ── VR-TH1 (tool honesty): refuse a declared target TYPE the product cannot serve ────────────
//
// `Targets`' own doc says this one level up, about KEYS: *"under strict keys a dead-but-accepted
// key is the worst kind, because it looks supported."* A dead-but-accepted VALUE is the same
// failure one level down, and it survived longer because nothing was looking:
// `message_broker.type: kafka` and `auth.type: api-key|mtls` were listed in
// `schemas/argus-config.schema.yaml` with NO implementation anywhere in the tree.
//
// ⛔ AND THE SCHEMA COULD NEVER HAVE CAUGHT THEM. `schemas.ArgusConfig` is embedded
// (`schemas/embed.go:22`) and read by NOTHING — only `schemas.Scenario` is consumed
// (`internal/scenario/schema.go:36`). `ValidateConfig` runs `config.Load` + this function and
// never consults the schema, so every enum in that file is advisory. Deleting a value from the
// schema therefore changes documentation and nothing else; THIS is the half that refuses.
//
// ⚠ These Type fields are DECORATIVE BY DESIGN and must stay that way — `DBTarget.Type`'s own
// comment: *"Nothing in the product reads it (the JDBC driver is chosen from the URL)"*. This
// check does not start dispatching on them. It refuses only a value the product cannot serve,
// so that a config which loads is a config that means something.
//
// ⚠ Widening one of these sets is a PROMISE. Do not add a value without the implementation
// behind it — that is the exact defect this check exists to have caught.
var (
	// dbTypesSupported: the JDBC drivers the execution plane actually ships —
	// jmeter-plugins/amqp/docker/install-jdbc-drivers.sh. The driver is chosen from
	// the JDBC URL, so this set is wider than the schema's old [postgres, mysql] and that is not
	// a mistake: mariadb/sqlserver/sqlite SUTs are testable today and the schema denied it.
	// ⚠ Oracle is deliberately NOT here — that script skips it on licence grounds.
	dbTypesSupported = []string{"postgres", "postgresql", "mysql", "mariadb", "sqlserver", "sqlite"}
	// mqTypesSupported: internal/amqpengine is the only broker engine in the tree.
	mqTypesSupported = []string{"amqp"}
	// authTypesSupported: AuthTarget carries exactly one credential field, BearerToken. `api-key`
	// has no header-name field and `mtls` has no cert/key pair, so neither could be honoured even
	// if something read this value.
	authTypesSupported = []string{"bearer"}
)

// targetTypeError returns a ConfigError when declared is a non-empty, non-"none" value outside
// supported. Empty and "none" always pass: the field is optional and most shipped configs omit it.
func targetTypeError(where, declared string, supported []string) *ConfigError {
	d := strings.ToLower(strings.TrimSpace(declared))
	if d == "" || d == "none" {
		return nil
	}
	for _, s := range supported {
		if d == s {
			return nil
		}
	}
	return &ConfigError{Message: fmt.Sprintf(
		"%s: type %q is not supported by this build — accepted: %s, or omit the key",
		where, declared, strings.Join(supported, ", "))}
}

// targetTypeErrors checks every declared target type, plain slot and named entry alike. A named
// entry is checked under its own name so the operator is told WHICH one is wrong.
func (c *Config) targetTypeErrors() []ConfigError {
	var errs []ConfigError
	add := func(e *ConfigError) {
		if e != nil {
			errs = append(errs, *e)
		}
	}
	if t := c.Targets.Database; t != nil {
		add(targetTypeError("targets.database", t.Type, dbTypesSupported))
	}
	for _, n := range sortedKeys(c.Targets.DatabaseTargets) {
		if t := c.Targets.DatabaseTargets[n]; t != nil {
			add(targetTypeError("targets.database_targets."+n, t.Type, dbTypesSupported))
		}
	}
	if t := c.Targets.MessageBroker; t != nil {
		add(targetTypeError("targets.message_broker", t.Type, mqTypesSupported))
	}
	for _, n := range sortedKeys(c.Targets.MessageBrokerTargets) {
		if t := c.Targets.MessageBrokerTargets[n]; t != nil {
			add(targetTypeError("targets.message_broker_targets."+n, t.Type, mqTypesSupported))
		}
	}
	if t := c.Targets.Auth; t != nil {
		add(targetTypeError("targets.auth", t.Type, authTypesSupported))
	}
	if t := c.Targets.MCP; t != nil && t.Auth != nil {
		add(targetTypeError("targets.mcp.auth", t.Auth.Type, authTypesSupported))
	}
	for _, n := range sortedKeys(c.Targets.MCPTargets) {
		if t := c.Targets.MCPTargets[n]; t != nil && t.Auth != nil {
			add(targetTypeError("targets.mcp_targets."+n+".auth", t.Auth.Type, authTypesSupported))
		}
	}
	return errs
}

// Validate runs the layer↔target coverage check across the scenarios dir
// (preflight.py parity). Returns nil errors if the config covers every layer
// referenced by every scenario.
func (c *Config) Validate(scenariosDir string) ([]ConfigError, int, error) {
	if c.Project.Name == "" {
		return []ConfigError{{Message: "missing project.name"}}, 0, nil
	}
	// VR-TH1: checked BEFORE the scenario walk and independent of it — an unsupported target type
	// is wrong whether or not any scenario happens to reference that layer today.
	errs := c.targetTypeErrors()
	count := 0
	// Shared discovery (FX-3): skips dot-dirs + placeholder IDs, so the phantom never
	// inflates scenarios_found and the count matches list/run exactly.
	for _, d := range scenario.DiscoverFiles(scenariosDir) {
		count++
		// T5.4: a money-handling SUT may only be READ. Checked FIRST and for EVERY scenario —
		// including the mcp/chain ones the coverage walk below `continue`s past — and a refused
		// scenario is not checked further.
		if c.MoneyHandling {
			allow := c.MoneyWriteAllowlist()
			if v := scenario.MoneyGuardViolations(d.Scenario, allow); len(v) > 0 {
				for _, msg := range v {
					errs = append(errs, ConfigError{Scenario: d.Scenario.ID, Message: "money_handling: " + msg})
				}
				continue
			}
			// money_writes item 3 (literal early check): a spends:true request whose body carries no
			// "${…}" templating at all can be checked NOW rather than waiting for a run to discover a
			// bound-to-fail literal amount.
			if v := scenario.MoneyWriteAmountViolations(d.Scenario, allow); len(v) > 0 {
				for _, msg := range v {
					errs = append(errs, ConfigError{Scenario: d.Scenario.ID, Message: "money_writes: " + msg})
				}
				continue
			}
		}
		// a load check that belongs to a test target declaring `load_test: never` is an
		// error, named by file, target and key. (The run-time door is argus.runOneScenario.)
		if rel, rerr := filepath.Rel(scenariosDir, d.Path); rerr == nil {
			if msg := c.neverLoadValidateMessage(d.Scenario, filepath.ToSlash(rel)); msg != "" {
				errs = append(errs, ConfigError{Scenario: d.Scenario.ID, Message: msg})
			}
		}
		// VR10-S3-7: a **Target** word is checked HERE, at validate time, against the declared
		// entries — a missing name, a wrong-kind name, and a layer the word has no meaning on are
		// refused by name. A refused scenario is not checked further (its coverage would be noise).
		sel, serr := c.SelectTarget(d.Scenario)
		if serr != nil {
			errs = append(errs, ConfigError{Scenario: d.Scenario.ID, Message: serr.Error()})
			continue
		}
		// S9, the authoring-time half: an AMQP Load scenario whose target the operator
		// has not marked as allowing load is reported HERE, before any run. (The run-time door is
		// argus.runOneScenario, which refuses it again: validation is advice, that one is the guarantee.)
		if msg := c.loadValidateMessage(d.Scenario); msg != "" {
			errs = append(errs, ConfigError{Scenario: d.Scenario.ID, Message: msg})
		}
		// CHANGE-1: an mcp/chain scenario triggers the SUT's MCP endpoint, declared in
		// targets.mcp — require a usable base_url and SKIP the per-layer http check (its
		// "HTTP Ingestion" layer label means the MCP edge, not an HTTP REST target), so a
		// pure-MCP SUT (Social, Memstore) needs no targets.http. A selected named mcp entry
		// covers the scenario on its own.
		if isMCPScenario(d.Scenario.Tags) {
			if sel == nil && c.MCPBaseURL() == "" && needsPlainMCPTarget(d.Scenario) {
				errs = append(errs, ConfigError{
					Scenario: d.Scenario.ID,
					Message:  "is an mcp/chain scenario but config has no usable targets.mcp.base_url",
				})
			}
			continue
		}
		for _, layer := range d.Scenario.Layers {
			key, ok := layerToConfig[layer]
			if !ok || key == "" {
				continue
			}
			if sel != nil && sel.Kind == key {
				continue // the selected named entry covers this layer; no plain slot is needed
			}
			if !c.targetPresent(key) {
				errs = append(errs, ConfigError{
					Scenario: d.Scenario.ID,
					Message:  fmt.Sprintf("uses layer %q but config has no usable targets.%s", layer, key),
				})
			}
		}
	}
	return errs, count, nil
}

// HTTPHostPort parses targets.http.base_url into (protocol, host, port).
func (c *Config) HTTPHostPort() (proto, host, port string) { return c.Targets.HTTP.HostPort() }

// HostPort parses an http target's base_url into (protocol, host, port) — the plain slot and a
// named entry answer alike. A nil or empty entry keeps the historical localhost:8080 default.
func (t *HTTPTarget) HostPort() (proto, host, port string) {
	proto, host, port = "http", "localhost", "8080"
	if t == nil || t.BaseURL == "" {
		return
	}
	u, err := url.Parse(t.BaseURL)
	if err != nil {
		return
	}
	if u.Scheme != "" {
		proto = u.Scheme
	}
	if u.Hostname() != "" {
		host = u.Hostname()
	}
	if u.Port() != "" {
		port = u.Port()
	} else if proto == "https" {
		port = "443"
	}
	return
}

// Origin is scheme://host:port of the target — exactly the host and port a plain HTTP check of the
// same target would call (HostPort, so the same defaults), and nothing of base_url's path
// . It answers false when the target declares no base_url: a chain step that asks for
// it must say so, not fall back to the historical localhost default.
func (t *HTTPTarget) Origin() (string, bool) {
	if t == nil || strings.TrimSpace(t.BaseURL) == "" {
		return "", false
	}
	proto, host, port := t.HostPort()
	return proto + "://" + net.JoinHostPort(host, port), true
}

// PathFromURL extracts the request path (+query) from a scenario trigger URL.
// Scenario URLs are typically "${INGESTION_URL}/api/v1/orders" (the ${VAR} stands
// in for the base URL, which the runner takes from config instead) or a full
// "http://host:port/path". Returns just "/api/v1/orders".
func PathFromURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "${") { // strip a leading ${VAR} base-URL placeholder
		if i := strings.Index(raw, "}"); i >= 0 {
			raw = raw[i+1:]
		}
	}
	if i := strings.Index(raw, "://"); i >= 0 { // strip scheme://host[:port]
		rest := raw[i+3:]
		if j := strings.IndexByte(rest, '/'); j >= 0 {
			raw = rest[j:]
		} else {
			raw = "/"
		}
	}
	if raw == "" {
		return "/"
	}
	if !strings.HasPrefix(raw, "/") && !strings.HasPrefix(raw, "?") {
		raw = "/" + raw
	}
	return raw
}

var _ = strconv.Itoa // reserved for future numeric knobs
var _ = url.Parse
