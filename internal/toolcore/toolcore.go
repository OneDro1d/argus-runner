// Package toolcore is the single shared operation layer behind BOTH entry points:
// the CLI (cmd/argus) and the MCP server (internal/mcpserver). Each function
// produces the structured payload the CLI emits AND the MCP server wraps in its
// envelope — so the two are byte-identical (VR-H1, no second core). Functions are
// thin orchestration over the existing internal/* packages; no business logic
// lives in the CLI command or the MCP handler.
package toolcore

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/OneDro1d/argus-runner/internal/argus"
	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/obsquery"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/role"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// Env holds the per-instance configuration both entry points supply (the CLI from
// flags, the MCP server from its serve config). instance_id is always "local" in M2.5.
type Env struct {
	Instance string
	// RunMode is the mode of the run this Env executes (the federated assignment's: build | final |
	// scheduled | rehearsal). Run records it on the report it writes, so every reader of the file —
	// product-hat GetReport, the relay's reduceReport — can tell a certification run's report from a
	// build run's. Empty (a local or direct run) keeps the report's own "ci" stamp.
	RunMode string
	// CompareTarget (executor release E2) is the declared named connection target a comparison member names: the
	// run uses it for every check that carries no **Target** of its own. Only a run of mode `compare` reads it.
	CompareTarget string
	// ObsInstance is the argus_instance LABEL for telemetry (metrics/logs) + Grafana deep-links; it
	// falls back to Instance when empty. M3 sets it to the REAL registered instance id even where the
	// tool identity stays "local" (the single-tenant convention the runner tools validate), so direct +
	// cloud runs share ONE Grafana instance drawer (R4-2). Instance still drives the report path +
	// the tool-identity check.
	ObsInstance  string
	ConfigPath   string
	ScenariosDir string
	ResultsRoot  string
	ComposeFile  string
	Grafana      string
	Loki         string
	// LokiTenant (T3.3, E3 shared ingest) is the X-Scope-OrgID every Loki request carries — the
	// query (lokiFor) AND the request-event push (Run). The instance id under --obs=shared; "" for
	// every other mode, which sends no header at all. Deploy wiring: --loki-tenant / ARGUS_LOKI_TENANT.
	LokiTenant  string
	Pushgateway string
	// Cluster is the deploy-derived environment label R6 (M3.1 ADR-10, CP-M3-116) stamps on the
	// pushed metrics + request events (compose | k3d-local | <aks-cluster>), so the shared
	// per-environment Prometheus distinguishes tiers and instances never merge across
	// environments. Empty → "compose" (the tier-1 default). Deploy wiring: ARGUS_CLUSTER.
	Cluster string
	// Tier is the DEPLOYMENT TIER this executor runs on, normalized by federation.WireTier to one of
	// the three values the control plane's CHECK constraint accepts: compose | k3d | managed.
	//
	// It is NOT the same thing as Cluster, which is a per-ENVIRONMENT telemetry label (k3d-local, an
	// AKS cluster name). Two instances on different clusters share a tier; that is the distinction
	// M3-FX needs, because what varies per tier is CONFIGURATION (VR-E1/E5) while what varies per
	// cluster is where metrics land.
	//
	// Added for VR-E8: `argus-config.yaml` gains per-tier values and onboarding must REFUSE when the
	// target tier has none. Before this field the tier reached only serve-time federation
	// registration (cmd/argus/main.go) and the rendered k8s Deployment — a config validator asked
	// "is the Grafana URL present for the tier I am onboarding to?" had no way to know the tier.
	//
	// EMPTY MEANS NOT SUPPLIED, AND A CONSUMER MUST NOT GUESS. It is tempting to default it to
	// "compose", since that is the CLI default everywhere else — but then a missing tier becomes an
	// answer nobody gave, and a per-tier lookup would silently resolve against the wrong tier and
	// report success. That is precisely the failure class this build exists to remove: report it as
	// "could not determine the tier" (gate.NotEstablished) rather than assuming one.
	Tier string
	// ObsMode is the observability mode the operator chose at onboarding (`--obs`): bundled | adopt |
	// export | shared | none. Empty means NOT SUPPLIED (a hand-run `argus validate-config`), and then
	// the validator behaves exactly as it always has. under "none" the operator wired
	// their own dashboards (or none), so the validator does not require
	// observability.grafana.public_url for any tier. Read from ARGUS_OBS_MODE, as Tier is from ARGUS_TIER.
	ObsMode string
	// JMeterLocal selects the JMeter-as-a-service runner (LocalJMeterRunner — jmeter as a
	// local subprocess, bundled in the serve image; no docker socket). Default false keeps
	// the DockerRunner (`docker compose exec jmeter`) for the host/dogfood path.
	JMeterLocal  bool
	TemplatesDir string // where the bundled <base>.jmx live (LocalJMeterRunner; default /templates)
	// RunReporter, when set, is called after a successful Run with the finished report + the run's run_id
	// + scope (single|tag|layer|full). It is the D3 "ALL RUNS REPORT UP" hook (R10/UC062): the serve
	// process wires it to push the evidence-free tallies to the cloud ledger under the run's own run_id.
	// Nil (the default, and every non-cloud path) means no push. Injected as a plain func so toolcore stays
	// decoupled from the federation client (no import cycle).
	// VR9-T1: startedAt/finishedAt are THIS RUN'S wall clock, measured here — the only place that
	// knows when the work began. A reporter that stamped its own "now" would be measuring the PUSH,
	// which is a receipt time wearing an elapsed time's clothes.
	RunReporter func(rep *report.Report, runID, scope string, startedAt, finishedAt time.Time)
	// SetFetcher, when set, fetches this instance's ACTIVE scenario set from the control-plane
	// catalog for the given selection. It is the U2 hook that lets an executor run with NO local
	// scenario directory — required on k8s, where /scenarios is a per-pod emptyDir and three
	// replicas cannot share a "kubectl cp"'d path. Injected as a plain func for the same reason
	// RunReporter is: toolcore stays decoupled from the federation client (no import cycle).
	//
	// Nil means "no catalog available", which is NOT the same as "the catalog is empty" — see
	// resolveScenarioDir: the difference decides whether a 0-scenario run is allowed or refused.
	SetFetcher func(ctx context.Context, layer, tag, scenarioID string) ([]CatalogScenario, error)
	// SetHasher, when set, asks the catalog ONLY for the current set hash — no bodies (VR-F6).
	//
	// It exists so a DIRECT local run can consult the catalog on EVERY run without paying a full
	// set transfer each time. Before VR-F6 a non-empty local directory always won, so a scenario
	// edited in the registry and then run locally executed the OLD copy on disk and reported a pass
	// for a version nobody was testing. Nil means "cannot ask cheaply" and the resolution falls back
	// to SetFetcher, which is correct but slower.
	SetHasher func(ctx context.Context, layer, tag, scenarioID string) (string, error)
	// UseLocalScenarios is PLAN B, explicitly chosen (VR-F6 + the owner's ruling 2026-08-11).
	//
	// When the control plane cannot be reached after VR-F2b's full retry policy, the default is to
	// REFUSE and let the user decide: wait for the CP, or run the local set knowing it is unverified.
	// This flag is the second choice, and it only has an effect in that one situation — it is not an
	// override that skips the catalog when the catalog is available, and it does not apply to a
	// REFUSED credential (a withdrawn authorisation is not an outage to work around).
	//
	// Set from `use_local_scenarios` on runner__run, or --use-local-scenarios on the CLI.
	UseLocalScenarios bool
	// AssignedSet says ScenariosDir holds a set the CONTROL PLANE assigned — the federated run
	// materializes the poll's scenarios there before calling Run. The set came from the catalog, so the
	// report says `catalog`. Without it the federated Env (which has no SetFetcher: the set was already
	// fetched) fell into "no catalog wired" and a cloud run was reported as the operator's own files
	// (V32 Release QA, run 20260914T152153182).
	AssignedSet bool
}

func (e Env) resultsDir() string { return filepath.Join(e.ResultsRoot, e.Instance) }

// obsInstance is the argus_instance label for telemetry + Grafana deep-links (falls back to Instance).
// cluster returns the deploy-derived environment label (R6/ADR-10), defaulting to the tier-1
// "compose" so a standalone rig needs no configuration.
func (e Env) cluster() string {
	if e.Cluster == "" {
		return "compose"
	}
	return e.Cluster
}

func (e Env) obsInstance() string {
	if e.ObsInstance != "" {
		return e.ObsInstance
	}
	return e.Instance
}
func (e Env) reportPath() string { return filepath.Join(e.resultsDir(), "report.json") }

// runReportPath is the per-run report file (RO-06): results/<instance>/runs/<run_id>.json.
// Run-attributed so get_report({run_id}) returns YOUR run even when the shared "latest"
// report.json belongs to a concurrent run (the two-agents-on-one-instance race).
func runReportPath(resultsDir, runID string) string {
	return filepath.Join(resultsDir, "runs", runID+".json")
}

// RedactExpected is redactExpected exported for AC-10's build record: the runner assembles
// argus.build_record from the SAME product-hat report shape get_report already returns (observed
// kept, expected stripped) — one rule, not a second copy of it in internal/runner.
func RedactExpected(rep *report.Report, hat role.Role) { redactExpected(rep, hat) }

// redactExpected strips failure.Expected for the product hat (the dark-factory holdout);
// applied on BOTH the per-run and the latest read paths.
func redactExpected(rep *report.Report, hat role.Role) {
	if !hat.MustRedactExpected() {
		return
	}
	for i := range rep.Layers {
		for j := range rep.Layers[i].Scenarios {
			sc := &rep.Layers[i].Scenarios[j]
			if f := sc.Failure; f != nil {
				f.Expected = nil
				// / the failing body bullet is the author's words and
				// carries the expected value — test hat only, nilled explicitly like Expected.
				f.FailedBodyCheck = nil
			}
			// VR10-S2 (S2-c): a chain step's enforced-assertion TEXT carries the scenario's expected
			// value — the product hat keeps only the count (AssertionsEnforcedCount).
			for k := range sc.Steps {
				sc.Steps[k].AssertionsEnforced = nil
				// the failed claims carry the claim text AND the SUT's observed value
				// for it — author-only, nilled explicitly (a new field is not covered by accident).
				sc.Steps[k].FailedClaims = nil
				// how many claims did not hold past the list's backstop — also
				// author-only (how many hidden claims failed is holdout material), zeroed by name.
				sc.Steps[k].FailedClaimsOmitted = 0
			}
			// The scenario-level twin (http, mcp and ui scenarios record their enforced checks on the
			// scenario, not on a step) carries the same expected values: text nilled, count kept.
			sc.AssertionsEnforced = nil
			// ⛔ VR12-E14 — TIER 3 IS HOLDOUT MATERIAL TOO. Unexecuted quotes the author's bullet
			// VERBATIM, which is the test's expected value in the author's own words: "body has
			// error containing …" tells a product agent exactly what is being checked. A brand-new
			// report field is NOT covered by this redaction by accident, so it is nilled here
			// explicitly and asserted by a test rather than assumed.
			sc.Unexecuted = nil
			// ARGUS-CMP-3: the recorded output digests of a compare run belong to the
			// author and to the control plane's digest ledger. A new report field is not covered by
			// accident, so it is nilled here BY NAME (and a compare run is verdict-only to a builder
			// anyway: WithholdScenarios drops every row).
			sc.Outputs = nil
		}
	}
}

// expectErrors — VR10-S2-8 / CR-1: every EXPECT bullet the MCP parser cannot classify, across the
// discovered mcp and chain scenarios, as validate-config errors naming file, line and bullet — the
// same rule the run applies at preflight, surfaced BEFORE any run.
func expectErrors(scenariosDir string) []config.ConfigError {
	var out []config.ConfigError
	for _, d := range scenario.DiscoverFiles(scenariosDir) {
		problems := argus.ExpectProblems(d.Scenario)
		if len(problems) == 0 {
			continue
		}
		text, _ := os.ReadFile(d.Path)
		rel := filepath.ToSlash(d.Path)
		if r, err := filepath.Rel(scenariosDir, d.Path); err == nil {
			rel = filepath.ToSlash(r)
		}
		for _, p := range problems {
			out = append(out, config.ConfigError{Message: fmt.Sprintf("%s line %d: %s", rel, lineOfBullet(string(text), p.Bullet), p.Message)})
		}
	}
	return out
}

// idErrors reports every discovered scenario whose id breaks the id rule, naming the file and the rule.
func idErrors(scenariosDir string) []config.ConfigError {
	var out []config.ConfigError
	for _, d := range scenario.DiscoverFiles(scenariosDir) {
		msg := scenario.CheckID(d.Scenario.ID)
		if msg == "" {
			continue
		}
		rel := filepath.ToSlash(d.Path)
		if r, err := filepath.Rel(scenariosDir, d.Path); err == nil {
			rel = filepath.ToSlash(r)
		}
		out = append(out, config.ConfigError{Message: fmt.Sprintf("%s: %s", rel, msg)})
	}
	return out
}

// scenarioRuleWarnings reports, per discovered scenario file, every problem ValidateAll finds:
// the rules both writers and the control plane's author_write_scenario apply. Before this
// validate-config said nothing about them, so a kit read valid:true and its first write was refused.
//
// ⛔ WARNINGS, not errors, on purpose: onboard.sh dies on a failed validate-config, and a kit that runs from local
// files has never had these rules applied to it at this step. Reported here, enforced by the writers. The EXPECT
// classifier and the id rule are already ERRORS above (expectErrors, idErrors) and are not said twice: their
// messages are skipped by equality.
func scenarioRuleWarnings(scenariosDir string) []string {
	var out []string
	for _, d := range scenario.DiscoverFiles(scenariosDir) {
		text, err := os.ReadFile(d.Path)
		if err != nil {
			continue
		}
		s, verrs := ValidateAll(string(text))
		// Already an ERROR of this same answer: the id rule and every EXPECT-classifier problem.
		already := map[string]bool{}
		if s != nil {
			if m := scenario.CheckID(s.ID); m != "" {
				already[m] = true
			}
			for _, p := range argus.ExpectProblems(s) {
				already[p.Message] = true
			}
		}
		rel := filepath.ToSlash(d.Path)
		if r, rerr := filepath.Rel(scenariosDir, d.Path); rerr == nil {
			rel = filepath.ToSlash(r)
		}
		for _, v := range verrs {
			if already[v.Message] {
				continue
			}
			out = append(out, fmt.Sprintf("%s line %d: %s. A scenario write (author__write_scenario, the control plane's author_write_scenario) will refuse this file until it is fixed", rel, v.Line, v.Message))
		}
	}
	return out
}

// lineOfBullet is the 1-based line of the first line containing the bullet text (0 when the
// bullet cannot be located — e.g. a whole-payload parse problem).
func lineOfBullet(text, bullet string) int {
	if bullet == "" {
		return 0
	}
	for i, l := range strings.Split(text, "\n") {
		if strings.Contains(l, bullet) {
			return i + 1
		}
	}
	return 0
}

func nowISO() string { return time.Now().UTC().Format(time.RFC3339) }

// ConfigWarnings is the ONE list of non-fatal config nudges validate-config prints, as a function so
// `argus doctor --config` runs the SAME checks instead of its own copy ( doctor said
// "validate-config's checks pass" for a config validate-config warned about). Pure and local: nothing
// here touches the network. estimated is the pack's request count (argus.RequestsEstimated).
func ConfigWarnings(c *config.Config, scenariosDir string, estimated int) []string {
	warnings := append(c.LogFieldMappingWarnings(), c.BusGuardrailWarnings()...)
	// AC-D21: a TRIGGER URL naming a host the run will not use (only its path is kept).
	warnings = append(warnings, c.TriggerHostWarnings(scenariosDir)...)
	// a base_url path is dropped by the runner (only host:port is used).
	warnings = append(warnings, c.BaseURLPathWarnings()...)
	if w := c.RateLimit.Warning(estimated); w != "" {
		warnings = append(warnings, w)
	}
	return warnings
}

// ValidateConfig — VR-A1. payload: {valid, scenarios_found, layers_configured, errors}.
func ValidateConfig(e Env) (any, bool, error) {
	c, err := config.Load(e.ConfigPath)
	if err != nil {
		return nil, false, fmt.Errorf("load config: %w", err)
	}
	errs, count, err := c.Validate(e.ScenariosDir)
	if err != nil {
		return nil, false, fmt.Errorf("validate: %w", err)
	}
	// item 8a (msgbus tester 2026-09-28): `count` above is a PURE LOCAL DIRECTORY WALK
	// (scenario.DiscoverFiles(e.ScenariosDir)). On a catalog-driven executor — k8s, where
	// /scenarios is a per-pod emptyDir (U2, the same trap resolveScenarioDir already refuses a RUN
	// over) — that walk finds nothing, and a relayed runner__validate_config reported
	// scenarios_found:0 for a kit that genuinely has scenarios, because it never asked the catalog
	// at all. Ask ONLY when the local walk found nothing and a catalog is actually wired
	// (e.SetFetcher, item 7b's wiring) — a non-empty local set still wins outright, matching
	// VR-F6's own "local can be authoritative" posture; this never DOWNGRADES a real local count.
	// Best-effort: a catalog error leaves `count` at 0 exactly as before this fix, never fails the
	// validation itself (VALIDATE never blocks on network reachability — see the `reach` probe below,
	// the same posture).
	if count == 0 && e.SetFetcher != nil {
		cctx, ccancel := context.WithTimeout(context.Background(), catalogCallTimeout)
		set, cerr := e.SetFetcher(cctx, "", "", "")
		ccancel()
		if cerr == nil {
			count = len(set)
		}
	}
	// VR10-S2 / CR-1 (S2-b): an EXPECT bullet the MCP parser cannot classify is refused HERE,
	// before any run, naming file, line and bullet. It lands in `errors` (valid:false) for the
	// reason VR-E8's tier check does: onboard.sh dies on a failed validate-config, so this is what
	// makes "impossible to miss" real without a second gate — a refusal is never softened (CR-1).
	errs = append(errs, expectErrors(e.ScenariosDir)...)
	// VR10-S4-1 (gate-2 F3): the id rule the import applies is applied HERE too, so the validator an
	// operator runs before the import never passes an id the seed will refuse by name.
	errs = append(errs, idErrors(e.ScenariosDir)...)
	// V30-004 F-6: a Message Flow scenario that asserts CONTENT needs two facts from the SUT's
	// broker config — the incoming exchange and the key to bind with. It is reported HERE and
	// nowhere else, because this is the only path that holds both the config and the scenarios:
	// ExpectProblems is handed a scenario and never a *config.Config, and on the control plane
	// there is no SUT config in scope at all.
	errs = append(errs, tapConfigErrors(c, e.ScenariosDir)...)
	// DF-12: distinguish config TARGETS from scenario test-LAYERS (the old single
	// `layers_configured` returned targets but read as "test-layers" — a misnomer + spec
	// mismatch). Emit both clearly; keep `layers_configured` as a deprecated alias = targets.
	// VR10-S3-11: `mcp` joins the list (absent since the field was added), and every NAMED entry is
	// listed as kind:name (`http:graph`) so the operator sees it was declared — and probed — by name.
	targets := []string{}
	for _, l := range []string{"http", "mcp", "message_broker", "database", "external", "auth"} {
		if c.TargetPresentExported(l) {
			targets = append(targets, l)
		}
	}
	for _, kind := range []string{config.KindHTTP, config.KindMCP, config.KindDatabase, config.KindMessageBroker} {
		for _, n := range c.NamedTargets(kind) {
			targets = append(targets, kind+":"+n)
		}
	}
	// INT-019/UC045: the THIRD confirmation the use case asks for — is the SUT reachable from the
	// RUNNER's vantage. It was never implemented, so valid:true read as "the environment is wired" when
	// it only ever meant "the file parses". Reachability deliberately does NOT change `valid`: a SUT
	// that is down is not a malformed config, and conflating them sends the operator to edit YAML.
	// It DOES raise warnings, because the reader who stops at valid:true is the one this is for.
	reach := ProbeTargets(c, 2*time.Second)
	matrix := CapabilityMatrix(c, e.ScenariosDir)

	// VR-E8 — the OPERATOR-FACING half of section E. `argus-config.yaml` now declares
	// observability.grafana.public_url PER TIER, and the value for the tier being onboarded to is
	// MANDATORY: onboarding stops rather than producing a dashboard link that does not open.
	//
	// This lands in `errs` (so valid:false) rather than in `warnings`, and that placement IS the
	// requirement. onboard.sh already dies on a failed validate-config, so putting it here is what
	// makes the refusal real without inventing a second gate the shell has to learn about.
	//
	// Unlike reachability above, this IS a malformed config: the file genuinely cannot answer a
	// question the onboard must answer, and the fix is to edit YAML — which is exactly where the
	// operator is being sent.
	tierChecked := e.Tier
	switch {
	case e.Tier == "":
		// No tier supplied. NOT an error, and NOT silently treated as compose: a direct/dogfood run
		// has no tier and legitimately validates the rest of the file. Reported as "not checked" so
		// nobody reads a green valid:true as proof the per-tier value is present.
		tierChecked = "not-checked"
	case e.ObsMode == "none":
		// `--obs none` means no Grafana is wired for this instance, so there is no
		// dashboard base to require on ANY tier. Not an error and not a guess: reported as
		// "skipped-obs-none" so a green valid:true is never read as proof the per-tier value is
		// present. Dashboard links then resolve to "" (resolveTierGrafana says so) unless
		// observability.dashboard_link.template declares one.
		tierChecked = "skipped-obs-none"
	case e.Tier == "managed" && c.NoObservabilityBackend() && len(c.Observability.Grafana.PublicURL.Tiers()) == 0:
		// item 8b (msgbus tester 2026-09-28): a correct chain/mcp-only kit that declares no
		// observability backend AT ALL (no loki, no betterstack) and no public_url for ANY tier was
		// refused on tier managed for a dashboard link it has nothing to draw — there is no run
		// this kit could ever produce a Grafana deep link for. Scoped narrowly: any public_url
		// declared for ANY tier still means "this file answers this question", so a partially
		// declared map (e.g. compose+k3d but not managed, TS-E2) is unaffected and stays refused.
	default:
		if _, gerr := c.GrafanaPublicURL(e.Tier); gerr != nil {
			errs = append(errs, config.ConfigError{Message: gerr.Error()})
		}
	}

	// VR10-R1-10 (owner D11): how many requests this pack sends — scenarios PLUS chain steps — and,
	// when a `rate_limit` is declared and the pack would exceed it, ONE warning line saying the run
	// may pause. Both conditions or no warning. ⛔ INFORMATION ONLY: it never invalidates the config
	// and never blocks a run (the owner struck an earlier draft that asked a question and waited).
	estimated := argus.RequestsEstimated(e.ScenariosDir)
	warnings := ConfigWarnings(c, e.ScenariosDir, estimated)
	// the rules the scenario writers apply, reported per file (never changes `valid`).
	warnings = append(warnings, scenarioRuleWarnings(e.ScenariosDir)...)

	out := map[string]any{
		"valid": len(errs) == 0, "scenarios_found": count,
		"reachability":       reach,                          // per-target probe from the runner's vantage (INT-019/UC045)
		"config_path":        e.ConfigPath,                   // RO-01: echo the config we ACTUALLY validated (single-tenant — never "which config?")
		"targets_configured": targets,                        // the SUT connection targets (argus-config `targets:`)
		"scenario_layers":    scenarioLayers(e.ScenariosDir), // the test-layers the discovered scenarios exercise
		// T4.2: all eight layers, whether or not a scenario uses them — what this config CAN test, and
		// why not. Information only: it never changes `valid` (see capmatrix.go).
		"capability_matrix":  matrix,
		"capability_summary": CapabilitySummary(matrix),
		"layers_configured":  targets,  // DEPRECATED alias (= targets) — kept for back-compat (DF-12)
		"warnings":           warnings, // non-fatal nudges: log-field translation-table gaps + the bus guardrail (UC163) + the D11 rate-limit line + AC-D21 trigger hosts
		// D11: the pack's size, so the reader can check it against the SUT's declared limit instead
		// of taking our arithmetic on trust. A chain counts as its steps, not as one request.
		"requests_estimated": estimated,
		// A SEPARATE list on purpose. `warnings` means "your CONFIG could be better" and has a contract
		// test that counts it; an unreachable SUT is a statement about the ENVIRONMENT, not the file.
		// Folding them together would also make every config warning network-dependent.
		"reachability_warnings": reachabilityWarnings(reach), // unreachable / never-checked targets (INT-019)
		// VR-E8: WHICH tier this validation was performed for. Without it a reader cannot tell a
		// config that declares every tier from one that happens to declare the only tier checked —
		// and "not-checked" says plainly that the per-tier value was not examined at all.
		"tier":           tierChecked,
		"tiers_declared": c.Observability.Grafana.PublicURL.Tiers(),
		"errors":         errs,
	}
	// Echoed only when the SUT declared one — an absent key says "this SUT declares no limit", which
	// is a different statement from "its limit is zero".
	if c.RateLimit != nil {
		out["rate_limit"] = map[string]any{"requests": c.RateLimit.Requests, "per": c.RateLimit.Per}
	}
	// the environment variable NAMES the config declares for DELIVERY to the executor
	// (check_env; it does not restrict which names a check may use) — echoed so the author sees the list was read. Names only; Load already refused any that is unset.
	if names := c.CheckEnvNames(); len(names) > 0 {
		out["check_env"] = names
	}
	return out, len(errs) > 0, nil
}

// scenarioLayers returns the distinct terminal (run) layers across the discovered
// scenarios — the test-layers actually exercised (DF-12), sorted for determinism.
func scenarioLayers(scenariosDir string) []string {
	seen := map[string]bool{}
	var out []string
	for _, d := range scenario.DiscoverFiles(scenariosDir) {
		s := d.Scenario
		if len(s.Layers) == 0 {
			continue
		}
		l := s.Layers[len(s.Layers)-1] // terminal = the layer the runner keys on
		if !seen[l] {
			seen[l] = true
			out = append(out, l)
		}
	}
	sort.Strings(out)
	return out
}

// Run — VR-A3 + selective execution (VR-H12..H14). Drives argus/JMeter, writes the
// report, pushes metrics. DF-06/07: a run-level run_id is stamped into the report so
// get_report can be scoped to THIS run; if runID is "" one is generated. payload:
// {status, run_id, summary, report_path, correlation_id}.
// noMatchHint (R3-A) explains the disk-vs-catalog trap: runner__run executes the ON-DISK scenarios/
// folder, so a selection that matched 0 there is almost always a scenario authored into the CLOUD
// CATALOG (which the in-env runner cannot see). Used for both the report Note (async get_report) and
// the synchronous run error, so total=0 is never a silent mystery.
const noMatchHint = "0 scenarios matched this selection, so nothing ran and nothing was verified. runner__run resolves its set from the on-disk scenarios/ folder when it has one, and otherwise from the cloud catalog. Check the selection first (a scenario id is not a tag, and a layer must be the canonical name); if the scenario genuinely does not exist yet, author or activate it."

// unverifiedRunNote is stamped on a report whose scenarios were NOT checked against the catalog
// (VR-F6). It exists because "local-unverified" in a field is a thing a machine reads, and the
// person looking at a green report needs the caveat in the same words they would use themselves.
const unverifiedRunNote = "NOTE: the control-plane catalog was UNREACHABLE for this run and it was " +
	"executed from the local scenarios folder at an explicit request (use_local_scenarios). The set " +
	"was NOT verified against the catalog, so it may be older than what was last authored, or may " +
	"omit scenarios the catalog has. Re-run once the control plane is reachable to confirm this result."

func Run(e Env, runID, layer, tag, scenarioID string) (any, bool, error) {
	// VR9-T1 — the run's wall clock starts HERE, before materialisation and setup, because a run that
	// spent its time preparing spent it on this run. Σ of the per-scenario durations would omit
	// exactly that, which is why the elapsed time is measured rather than summed.
	runStartedAt := time.Now().UTC()
	if runID == "" {
		runID = argus.NewRunID() // shortest UTC date-time; embedded in each scenario's correlation id
	}
	c, err := config.Load(e.ConfigPath)
	if err != nil {
		return nil, false, fmt.Errorf("load config: %w", err)
	}
	resultsRoot := e.ResultsRoot
	if abs, aerr := filepath.Abs(resultsRoot); aerr == nil {
		resultsRoot = abs
	}
	var runner argus.Runner
	if e.JMeterLocal {
		// JMeter-as-a-service (D2): jmeter is a local subprocess in the serve image — no
		// `docker compose exec`, no docker socket handed to the server.
		runner = &argus.LocalJMeterRunner{TemplatesDir: e.TemplatesDir}
	} else {
		absCompose, _ := filepath.Abs(e.ComposeFile)
		runner = &argus.DockerRunner{ComposeFile: absCompose, Service: "jmeter", HostResultsRoot: resultsRoot}
	}
	resultsDir := filepath.Join(resultsRoot, e.Instance)
	// U2: resolve WHERE the scenarios come from — the local dir on compose, the control-plane
	// catalog on a k8s executor that has none. This also refuses outright when the source cannot
	// be established at all, because a 0-scenario run used to report "passed" and push a green
	// ledger row (see scenariosource.go).
	scenariosDir, setSource, cleanupSet, err := resolveScenarioDir(e, runID, layer, tag, scenarioID)
	if err != nil {
		return nil, false, err
	}
	defer cleanupSet()
	// Spec 26 (A1, observe only): the sandbox evidence reader, nil unless argus-config declares
	// observability.openshell. This is the one funnel for the MCP tool, the CLI, reveal and the relay.
	rr, err := argus.RunAllWithTarget(c, scenariosDir, resultsDir, c.Project.Name, layer, tag, scenarioID, runID, runner, evidenceFor(e, c), e.RunMode, e.CompareTarget)
	if err != nil {
		return nil, false, fmt.Errorf("run: %w", err)
	}
	rr.Report.Timestamp = nowISO()
	rr.Report.RunID = runID // DF-07: stamp the run handle so get_report(run_id) is run-scoped
	// record the run's REAL mode before the file is written, so no reader can ever see a
	// certification run's report stamped as a local "ci" run. Empty keeps argus.RunAll's "ci" stamp.
	if e.RunMode != "" {
		rr.Report.Mode = e.RunMode
	}
	// Spec 26: AFTER the mode stamp, so a certification run logs no sandbox line.
	logSandboxPolicyGaps(runID, rr.Report)
	// VR-F6: stamp WHERE the set came from, BEFORE the report is written. The tool response carries
	// it too, but that is read once; the report is what anyone consults afterwards, and a green
	// report over an unverified local set is a different claim from a green report over the
	// catalog's set. When the catalog was unreachable and a human chose to run anyway, the Note
	// says so in words as well — the field is for machines, the note is for the person reading it.
	rr.Report.ScenarioSource = setSource
	if setSource == "local-unverified" {
		rr.Report.Note = strings.TrimSpace(rr.Report.Note + " " + unverifiedRunNote)
	}
	// R3-A: stamp the disk-vs-catalog hint ONTO the report for a named 0-match, BEFORE it is written, so
	// the async get_report path surfaces WHY total=0 instead of returning a silent empty report.
	if (layer != "" || tag != "" || scenarioID != "") && rr.Report.Summary.Total == 0 {
		rr.Report.Note = noMatchHint
	}
	if err := writeReport(filepath.Join(resultsDir, "report.json"), rr.Report); err != nil {
		return nil, false, fmt.Errorf("write report: %w", err)
	}
	// RO-06: also write the run-attributed per-run file so get_report(run_id) returns
	// THIS run even after a concurrent run overwrites the shared latest report.json.
	if err := writeReport(runReportPath(resultsDir, runID), rr.Report); err != nil {
		return nil, false, fmt.Errorf("write run report: %w", err)
	}
	// Patch #4: the pushed metric `project` label must equal the LOG project label (ProjectLabel,
	// which honors observability.loki.project_label) so the dashboard's `project` var (read from
	// argus_scenarios_total) matches the Loki panels' project filter — otherwise a SUT that sets
	// project_label would diverge and the per-SUT log scoping would break.
	//
	// Both pushes are best-effort: a telemetry store being down never fails the run. But a REFUSED push
	// must be SEEN. These were `_ =`, and on hub-dev Loki refused the runner's request events for
	// 30 minutes (stream cap full, AC-D32): the dashboard's panels were empty and nothing said why.
	// Logged to stderr because two of Run's callers discard its payload (runner relay + execute).
	var obsPushWarnings []string
	// ARGUS-CMP-3: a compare run sends NO per-check telemetry. Both
	// pushes below label every scenario id, and the set of a compare run may be a sealed regression set
	// whose ids must not reach the Pushgateway or Loki. Every other mode pushes exactly as before.
	if e.RunMode != report.ModeCompare {
		if err := obsquery.PushMetrics(e.Pushgateway, e.obsInstance(), c.ProjectLabel(), e.cluster(), rr.Report, c.PushgatewayGroupRetention()); err != nil {
			obsPushWarnings = append(obsPushWarnings, "metrics push to the pushgateway failed: "+err.Error())
		}
		// r3 (1a′): ship one Loki event per request fired, stamped at its real fire-time, so Panel A
		// plots the genuine within-run distribution. Same project label as the metrics push so the
		// dashboard's $project var scopes both.
		if err := obsquery.PushRequestEvents(lokiFor(e), e.obsInstance(), c.ProjectLabel(), e.cluster(), rr.Report); err != nil {
			obsPushWarnings = append(obsPushWarnings, "request-event push to Loki failed: "+err.Error())
		}
	}
	for _, w := range obsPushWarnings {
		slog.Warn("observability push failed: the run's verdict stands, but the dashboard will be missing this run",
			"run_id", runID, "detail", w)
	}
	// D3 (R10): ALL RUNS REPORT UP — a direct in-env run reports its evidence-free tallies to the cloud
	// ledger under its OWN run_id (no run_request_id), when the serve process is CP-wired (RunReporter set).
	// The MCP server already backgrounds runner__run, so this push never blocks the tool response; it is
	// best-effort (results are local files first, so a failed push loses nothing — UC062).
	if e.RunReporter != nil {
		e.RunReporter(rr.Report, runID, runScope(layer, tag, scenarioID), runStartedAt, time.Now().UTC())
	}
	status := "passed"
	if rr.Report.Failed() {
		status = "failed"
	}
	out := map[string]any{"status": status, "run_id": runID, "summary": rr.Report.Summary,
		"report_path": filepath.Join(e.resultsDir(), "report.json"), "correlation_id": rr.CorrelationID}
	// VR12-CH1 rule 7: a run that ends with something possibly still on the SUT SAYS SO, here as
	// well as in report.json. It is reality about the RUN, not about any scenario's expected value,
	// so it is safe for both hats.
	if lines := rr.Report.ResidueLines(runID); len(lines) > 0 {
		out["residue"] = lines
	}
	if len(obsPushWarnings) > 0 {
		out["obs_push_warnings"] = obsPushWarnings
	}
	// D11: the SAME warning validate_config gives, echoed here — an operator who starts a run
	// without validating first must still be told the pack may hit the SUT's limit and that the run
	// will pause. ⛔ Information only, and after the fact by construction: the run has already run.
	if w := c.RateLimit.Warning(argus.RequestsEstimated(scenariosDir)); w != "" {
		out["rate_limit_warning"] = w
	}
	// C10: configured-vs-executed reconciliation — surface a silent skip (a scenario on disk
	// that did not run). A layer/tag/id filter runs a subset by design, so only reconcile an
	// unfiltered run.
	filtered := layer != "" || tag != "" || scenarioID != ""
	configured, notExecuted := reconcileRun(scenario.DiscoverFiles(scenariosDir), rr.Report, filtered)
	out["scenario_source"] = setSource // local | catalog | catalog-empty — never leave this to inference
	out["configured"] = configured
	out["executed"] = rr.Report.Summary.Total
	if len(notExecuted) > 0 {
		out["not_executed"] = notExecuted
		out["note"] = fmt.Sprintf("%d scenario(s) discovered on disk did not run", len(notExecuted))
	}
	// R3-7: a FILTERED run (single/layer/tag) that matched ZERO scenarios never happened — it must not
	// report "passed", or a scenario absent from the catalog yields a false green (the ORDE-006 case the
	// owner reproduced). Surface it as an error. An unfiltered full run against an empty set keeps its
	// status (nothing was named). The cloud path is guarded earlier at enqueue (control.requestRun).
	noMatch := filtered && rr.Report.Summary.Total == 0
	if noMatch {
		out["status"] = "errored"
		out["selection"] = fmt.Sprintf("layer=%q tag=%q scenario_ref=%q", layer, tag, scenarioID)
		out["error"] = noMatchHint
	}
	// An UNFILTERED run that executed zero scenarios is not a pass — it verified nothing. It used
	// to report "passed" (proven live 2026-07-22 against an empty dir), which is the worst
	// direction to be wrong in for a testing product: nobody investigates a green.
	//
	// Having NO scenarios is now refused outright before the run starts (resolveScenarioDir), so
	// reaching here means a set WAS resolved yet nothing ran — e.g. files that exist but none of
	// which parse into a runnable scenario. That is still not a pass.
	if !filtered && rr.Report.Summary.Total == 0 {
		out["status"] = "no_scenarios"
		out["note"] = "0 scenarios executed — nothing was verified. A scenario set was found but none of it was runnable; check that the files parse (argus validate-scenario) and carry the required sections."
	}
	return out, rr.Report.Failed() || noMatch, nil
}

// runScope classifies a run's selection for the cloud ledger (D3), matching the cloud request_run scope
// vocabulary: an explicit scenario id → "single", else a tag → "tag", else a layer → "layer", else "full".
func runScope(layer, tag, scenarioID string) string {
	switch {
	case scenarioID != "":
		return "single"
	case tag != "":
		return "tag"
	case layer != "":
		return "layer"
	default:
		return "full"
	}
}

// reconcileRun compares the scenarios discovered on disk against those in the report,
// returning the configured count + any IDs that did NOT run (a silent skip). Only meaningful
// for an unfiltered run — a layer/tag/id filter legitimately runs a subset (C10).
func reconcileRun(discovered []scenario.Discovered, rep *report.Report, filtered bool) (configured int, notExecuted []string) {
	configured = len(discovered)
	if filtered || rep == nil {
		return configured, nil
	}
	executed := map[string]bool{}
	for _, l := range rep.Layers {
		for _, s := range l.Scenarios {
			executed[s.ID] = true
		}
	}
	for _, d := range discovered {
		if !executed[d.Scenario.ID] {
			notExecuted = append(notExecuted, d.Scenario.ID)
		}
	}
	sort.Strings(notExecuted)
	return configured, notExecuted
}

// GetReport — VR-A3. Product hat: observed only (expected stripped, VR-D2). DF-07: when
// runID is given, return ONLY that run's report (matched by report.RunID); a different/
// unknown run_id returns a pending/not-found signal — never the stale last report as "yours".
func GetReport(e Env, hat role.Role, runID string) (any, bool, error) {
	// RO-06: a run_id is RUN-ATTRIBUTED — read THIS run's per-run file first, so a
	// concurrent run's shared report.json (the two-agents-on-one-instance race) can
	// never be returned as yours.
	if runID != "" {
		if rep, rerr := readReport(runReportPath(e.resultsDir(), runID)); rerr == nil {
			redactExpected(rep, hat)
			withholdCertification(rep, hat)
			return rep, rep.Failed(), nil
		}
	}
	rep, err := readReport(e.reportPath())
	if err != nil {
		if runID != "" {
			return map[string]any{"status": "not_found", "run_id": runID, "note": "no report on disk yet — the run may still be in progress; poll again"}, false, nil
		}
		return nil, false, fmt.Errorf("read report: %w (run first?)", err)
	}
	if runID != "" && rep.RunID != runID {
		// DF-07: no per-run file AND the on-disk latest is a different run (still running,
		// or a stale/cross-project last run) — do NOT hand it back as the requested run.
		return map[string]any{"status": "pending_or_unknown", "run_id": runID,
			"note": "the report for this run_id is not on disk (the run may still be in progress, or the run_id is unknown) — poll get_report again"}, false, nil
	}
	redactExpected(rep, hat)
	withholdCertification(rep, hat)
	return rep, rep.Failed(), nil
}

// withholdCertification is the product hat's custody rule for a report on disk: a run
// that is not a build run (final, scheduled, rehearsal — the certification set, which is the holdout)
// reaches a builder as its verdict and tallies only. Applied on both read paths of GetReport, which is
// where the in-env MCP tool, `argus get-report` and the relayed get_report all read the file. The test
// hat (author) is untouched.
func withholdCertification(rep *report.Report, hat role.Role) {
	if hat.MustRedactExpected() && !rep.ScenariosVisibleToBuilder() {
		rep.WithholdScenarios()
	}
}

// Capabilities (CHANGE-4) is the honest per-SUT preflight report: it DERIVES each SUT's
// capability state from argus-config (no new declaration schema — 1a) and lists the SOFT/
// CONDITIONAL signals it does NOT emit as DECLARED GAPS, never a mysterious blank panel. A
// gap is a reported state, NOT a failure — the SUT's behaviour stays testable. The onboarder
// prints this so the SUT owner learns the result at onboarding, not at the first RED run.
func Capabilities(e Env) (any, error) {
	c, err := config.Load(e.ConfigPath)
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	mcp := c.MCPBaseURL() != ""
	httpEdge := c.TargetPresentExported("http")
	db := c.TargetPresentExported("database")
	metrics := c.PromEnabled()

	caps := map[string]any{
		"http":        map[string]any{"declared": httpEdge},
		"mcp":         map[string]any{"declared": mcp, "transport": c.MCPTransport(), "auth": c.MCPAuthType()},
		"database_ro": map[string]any{"declared": db},
		// metrics.declared is neutral info (does the SUT expose /metrics). The request/error-rate
		// panel is now runner-owned (the Loki request-event stream), so a missing /metrics is no
		// longer a dashboard gap — it is not listed in declared_gaps.
		"metrics":           map[string]any{"declared": metrics},
		"correlation_field": c.CorrelationField(),
		"saga":              map[string]any{"event_field": c.SagaEventField(), "event_value": c.SagaEventValue()},
	}
	if !mcp {
		caps["mcp"] = map[string]any{"declared": false}
	}

	// DECLARED GAPS — the SUT owner's honest declared-gap lines (things the suite cannot
	// derive: no correlation propagation, no sagas) FIRST, then the derived CONDITIONAL gaps.
	// All are reported, never failed. (The /metrics request-rate gap is retired — the
	// request/error-rate panel is runner-owned now, so a missing /metrics is not a gap.)
	gaps := append([]string{}, c.DeclaredGaps()...)
	if !db {
		gaps = append(gaps, "no targets.database (read-only) → the Database-State layer is not testable here (CONDITIONAL)")
	}

	// One-line human summary: what IS testable + the gap headline.
	var trigger string
	switch {
	case mcp && httpEdge:
		trigger = "HTTP + MCP"
	case mcp:
		trigger = "behavioural-MCP"
	case httpEdge:
		trigger = "HTTP"
	default:
		trigger = "(no trigger layer declared!)"
	}
	summary := fmt.Sprintf("%s-testable", trigger)
	if mcp {
		summary += fmt.Sprintf(" (%s, auth %s)", c.MCPTransport(), c.MCPAuthType())
	}
	if len(gaps) == 0 {
		summary += "; no declared gaps"
	} else {
		summary += fmt.Sprintf("; %d declared gap(s) — see declared_gaps", len(gaps))
	}

	return map[string]any{
		"project":       c.Project.Name,
		"config_path":   e.ConfigPath,
		"capabilities":  caps,
		"declared_gaps": gaps,
		"summary":       summary,
		"note":          "a declared gap is a reported capability state, NOT a failure — the SUT's behaviour stays testable; close a gap only if you want that dashboard surface to populate",
	}, nil
}

// GetSagas / TailLogs / GetDashboardURL — VR-A4/A5/A6.
func GetSagas(e Env, corrID, window string) (any, error) {
	return backendFor(e).Sagas(corrID, window, anchorFromReport(e, corrID)), nil
}
func TailLogs(e Env, corrID, window string) (any, error) {
	return backendFor(e).Logs(corrID, window, anchorFromReport(e, corrID)), nil
}

// backendFor selects the query backend for this instance (AC-D13): observability.betterstack
// declared in argus-config → obsquery.BetterStack queried over its HTTP SQL API; otherwise (the
// unchanged default, every pre-existing argus-config) → obsquery.Loki via lokiFor. Both satisfy
// the same obsquery.Backend surface, so GetSagas/TailLogs don't care which one answers.
// validateObservabilityBackend (config.Load) already refuses a config declaring both, so at most
// one of the two conditions here is ever reachable.
func backendFor(e Env) obsquery.Backend {
	if c, err := config.Load(e.ConfigPath); err == nil && c.UseBetterStack() {
		return betterStackFor(c)
	}
	return lokiFor(e)
}

// betterStackFor builds the BetterStack client from argus-config's observability.betterstack
// block — same threading convention as lokiFor (config values threaded in as plain fields;
// obsquery never imports config).
func betterStackFor(c *config.Config) *obsquery.BetterStack {
	return &obsquery.BetterStack{
		QueryURL:          c.BetterStackQueryURL(),
		Credential:        c.BetterStackCredential(),
		TeamID:            c.BetterStackTeamID(),
		Sources:           c.BetterStackSources(),
		CorrelationFields: c.BetterStackCorrelationFields(),
		LevelField:        c.BetterStackLevelField(),
		SagaEventField:    c.BetterStackSagaEventField(),
		SagaEventValues:   c.BetterStackSagaEventValues(),
		SagaStepFields:    c.BetterStackSagaStepFields(),
	}
}

// lokiFor builds the Loki client for this instance, threading the SUT's declared
// correlation/saga field names from argus-config (CHANGE-2) so log/saga extraction reads
// the right field for THIS SUT (e.g. Social's request_id). Best-effort: a missing/invalid
// config falls back to the canonical defaults baked into obsquery.Loki.
func lokiFor(e Env) *obsquery.Loki {
	// T3.3: the tenant does not come from the config — it is the instance's identity in a SHARED
	// Loki, set by the deploy (--loki-tenant), so it is threaded even when the config fails to load.
	l := &obsquery.Loki{BaseURL: e.Loki, Tenant: e.LokiTenant}
	if c, err := config.Load(e.ConfigPath); err == nil {
		l.CorrelationField = c.CorrelationField()
		l.SagaEventField = c.SagaEventField()
		l.SagaEventValue = c.SagaEventValue()
		// PROB-2: the full marker SET — a SUT may mark control actions with several values
		// (Social: tool_dispatch / ayrshare_dispatch / ghost_dispatch), and with only the scalar
		// its saga could contain gateway lines and nothing else.
		l.SagaEventValues = c.SagaEventValues()
		// GAP-1: also thread the STEP field names. Without this the step fields were read under
		// hardcoded the operator names, so a SUT naming them differently produced content-free saga nodes.
		l.SagaStepFields = c.SagaStepFields()
		// #422: the declared log encoding. The reader parses logfmt ONLY when it is declared, the
		// same rule the saga-presence verdict applies (argus.go loki.log_format).
		l.LogFormat = c.LogFormat()
		// T3.1 (E3 export hosted-Loki target): the resolved Basic-Auth credential and push_url, if the
		// SUT's argus-config declares them — "" for every bundled/adopt config, and obsquery.Loki sends
		// no Authorization header at all when the credential is empty.
		//
		// both belong to the CONFIGURED Loki, so they are threaded only when this
		// executor reads it (adopt/export: --loki == observability.loki.url). A bundled or shared
		// executor may load a config that declares them too; sending the hosted password to its own
		// Loki would hand it to a different server, and pushing to push_url would take its events out
		// of the Loki its --loki names (and, shared, out of its tenant).
		if cfgURL := c.LokiURLConfigured(); cfgURL != "" && sameLokiURL(e.Loki, cfgURL) {
			l.Credential = c.LokiCredential()
			l.PushURL = c.LokiPushURLConfigured()
		}
	}
	return l
}

// evidenceFor is the spec 26 A1 reader: nil when argus-config declares no observability.openshell
// block, else the SAME Loki client get_tail_logs uses (lokiFor), so the --loki URL, the tenant (T3.3)
// and the configured-credential rule apply unchanged. It returns an untyped nil, never
// a nil *Loki inside the interface, so `ev == nil` means "no reader" downstream.
func evidenceFor(e Env, c *config.Config) obsquery.SandboxEvidence {
	if c == nil || c.OpenShellEvidence() == nil {
		return nil
	}
	return lokiFor(e)
}

// logSandboxPolicyGaps writes one structured WARN per row whose sandbox evidence is not complete, so
// an operator sees an `unavailable` or `lossy` window without opening report.json. The verdict stands
// either way (P1 observes). Only the coverage and its reason are logged — never an event.
//
// NOTHING is logged for a run whose scenarios are withheld from the builder (a
// certification run: final, scheduled, rehearsal, or a mode that is unknown or empty — the #417 rule,
// Report.ScenariosVisibleToBuilder). The line names the scenario, its correlation id and the coverage,
// and the executor's stdout is shipped into the Loki that runner__get_tail_logs reads by substring, so
// "tr-<run_id>" would hand a builder what get_report withholds. The author reads it in the report. The
// caller logs AFTER the run's mode is stamped on the report.
func logSandboxPolicyGaps(runID string, rep *report.Report) {
	if rep == nil || !rep.ScenariosVisibleToBuilder() {
		return
	}
	for _, l := range rep.Layers {
		for _, s := range l.Scenarios {
			p := s.SandboxPolicy
			if p == nil || p.Coverage == report.CoverageComplete {
				continue
			}
			slog.Warn("sandbox policy evidence is not complete for this scenario: its verdict stands, but what the sandbox denied is not fully known",
				"run_id", runID, "scenario_id", s.ID, "correlation_id", s.CorrelationID,
				"coverage", p.Coverage, "reason", p.CoverageReason)
		}
	}
}

// sameLokiURL compares two Loki base URLs, ignoring surrounding space and a trailing slash.
func sameLokiURL(a, b string) bool {
	norm := func(s string) string { return strings.TrimRight(strings.TrimSpace(s), "/") }
	return norm(a) == norm(b)
}

// parseAnchor parses a report RFC3339 timestamp into the run anchor (zero on failure → now-window).
func parseAnchor(ts string) time.Time {
	t, _ := time.Parse(time.RFC3339, ts)
	return t
}

// anchorFromReport finds the run timestamp to anchor a corr-id's saga/log window (DF-09):
// the last report's timestamp IF that corr-id belongs to it, else zero (fall back to now-window).
func anchorFromReport(e Env, corrID string) time.Time {
	rep, err := readReport(e.reportPath())
	if err != nil {
		return time.Time{}
	}
	for i := range rep.Layers {
		for j := range rep.Layers[i].Scenarios {
			if rep.Layers[i].Scenarios[j].CorrelationID == corrID {
				return parseAnchor(rep.Timestamp)
			}
		}
	}
	return time.Time{}
}

// GetDashboardURL — VR-A6. 4.6: ALWAYS offers the general overview dashboard (so a caller
// who wants the general one needn't know to omit correlation_id), and is honest that a
// templated URL is not a check that the run/correlation actually exists.
func GetDashboardURL(e Env, corrID string) any {
	// Patch #4: scope the deep link to THIS SUT's project so the Loki panels don't bleed a prior
	// SUT's logs. Best-effort: a missing/invalid config just omits var-project.
	project := ""
	if c, err := config.Load(e.ConfigPath); err == nil {
		project = c.Project.Name
	}
	// E1 (R12): also scope to the RUN. The dashboard's `current_run` (Run ID) variable has NO default,
	// so without var-current_run the run-scoped panels render empty ("nothing usable"). Derive the run
	// id from the correlation id — it embeds tr-<run_id>-… — for a scoped link; fall back to the LAST
	// run's id (from report.json), which is also what the general overview focuses on.
	lastRun := ""
	if rep, err := readReport(e.reportPath()); err == nil && rep != nil {
		lastRun = rep.RunID
	}
	scopedRun := runIDFromCorrelation(corrID)
	if scopedRun == "" {
		scopedRun = lastRun
	}
	// M3-FX (VR-E8/VR-F9): the Grafana base is now resolved PER TIER from argus-config.yaml, and that
	// resolution can legitimately fail (the tier is unknown, or the config declares no value for it).
	// When it does, the honest answer is that there is no dashboard link — not a link-shaped string
	// that 404s. INT-008 was exactly that failure wearing a URL's clothes for weeks.
	if strings.TrimSpace(e.Grafana) == "" {
		return map[string]any{
			"dashboard_url": "",
			"overview_url":  "",
			"note": "no Grafana base is configured for this instance's tier, so no dashboard link can be built. " +
				"Declare observability.grafana.public_url for this tier in argus-config.yaml (on a k8s tier " +
				"`localhost` is not your machine — use the cluster's Grafana host).",
		}
	}
	return map[string]any{
		"dashboard_url": obsquery.DashboardURL(e.Grafana, e.obsInstance(), project, corrID, scopedRun), // scoped to the run + correlation when given
		"overview_url":  obsquery.DashboardURL(e.Grafana, e.obsInstance(), project, "", lastRun),       // the GENERAL dashboard (last run, no correlation filter)
		"note":          "URL is templated from your inputs; it is not a check that the run/correlation_id exists.",
	}
}

// runIDRe matches the run id embedded in a correlation id: the shortest UTC date-time (YYYYMMDDThhmmss),
// e.g. the "20260715T120000" in "tr-20260715T120000-ORD-006-a1b2".
var runIDRe = regexp.MustCompile(`\d{8}T\d{6}`)

// runIDFromCorrelation extracts the embedded run id from a correlation id, or "" when none is present.
func runIDFromCorrelation(corrID string) string { return runIDRe.FindString(corrID) }

// RunDashboardURL is the run-scoped Grafana deep-link for a run_id (var-current_run, no correlation) —
// the link that travels UP to the cloud ledger row (E1/R12, D3 report-up) so the web Runs page opens the
// correct dashboard view for that run. Uses e.Grafana (the host-facing public URL the serve process
// resolves). "" when no Grafana base is configured.
func RunDashboardURL(e Env, runID string) string {
	if e.Grafana == "" {
		return ""
	}
	project := ""
	if c, err := config.Load(e.ConfigPath); err == nil {
		project = c.Project.Name
	}
	return obsquery.DashboardURL(e.Grafana, e.obsInstance(), project, "", runID)
}

// --- author (scenario-side) ---

// ListScenarios — VR-AUTH5.
func ListScenarios(e Env) (any, error) {
	type row struct {
		ID         string   `json:"ID"`
		Layer      string   `json:"Layer"`       // DF-02: the TERMINAL (run) layer — what `run layer:X` filters on
		LayerChain []string `json:"layer_chain"` // DF-02: the full chain (HTTP Ingestion -> Database State)
		Path       string   `json:"Path"`
		Tags       []string `json:"Tags"`
	}
	var rows []row
	for _, d := range scenario.DiscoverFiles(e.ScenariosDir) {
		s := d.Scenario
		layer := ""
		if len(s.Layers) > 0 {
			layer = s.Layers[len(s.Layers)-1] // terminal layer (the runner keys on it), not the first hop
		}
		// VR12-M2 (V30-003): the `Priority` column is gone with the key. This listing was its ONLY
		// consumer anywhere in the product, which is precisely why the field could be removed.
		rows = append(rows, row{ID: s.ID, Layer: layer, LayerChain: s.Layers, Path: d.Path, Tags: s.Tags})
	}
	return map[string]any{"scenarios": rows}, nil
}

// ReadScenario — VR-AUTH5. Unknown id → not-found error.
func ReadScenario(e Env, id string) (any, error) {
	p := findScenarioPath(e.ScenariosDir, id)
	if p == "" {
		return nil, fmt.Errorf("scenario %q not found", id)
	}
	b, _ := os.ReadFile(p)
	s := scenario.Parse(string(b))
	return map[string]any{"path": p, "markdown": string(b), "parsed": s.AsParsedMap(), "references": s.References}, nil
}

// ValidateScenario — VR-AUTH2/AUTH3. Uncoverable EXPECT → valid:true + warning (DF-DEC-M25-03).
// VR10-S2-8 / CR-1: an mcp/chain EXPECT bullet the parser cannot classify (or a `matching` regex
// that does not compile) is an ERROR with its line, not a warning — `author__validate_scenario`
// and `author__write_scenario` both come through here.
func ValidateScenario(body string) (any, bool, error) {
	s, verrs := ValidateAll(body)
	warns := scenario.Warnings(body)
	out := map[string]any{"valid": len(verrs) == 0, "errors": verrs, "warnings": warns}
	// VR12-T3.c (V29-018): tell the author WHICH headers their scenario will send. Until 0.3.31 the
	// parser populated every one and the runner sent exactly one of them, so an author had no way to
	// discover that their `Accept:` line was being dropped — the tool that exists to tell them their
	// scenario is correct said nothing about it.
	//
	// ⛔ NAMES ONLY, NEVER VALUES. A header value is the likeliest place in a scenario to hold a
	// credential, and this map is returned to an agent and pasted into chat.
	if hs := scenario.HeaderNames(s); len(hs) > 0 {
		out["trigger_headers"] = hs
	}
	return out, len(verrs) > 0, nil
}

// ValidateScenarioWithConfig is ValidateScenario PLUS the full per-record schema check design §8
// promises for a standalone authoring run: "argus validate-scenario --config argus-config.yaml
// run locally with the app's config, so they can check the record against the declared schema
// fully." c is the SUT's loaded config (nil when no --config was given, e.g. the control plane's
// own author_validate_scenario, which never has the app's argus-config — design §8's "the
// control plane's checks are shape-only"); a nil c makes this function IDENTICAL to
// ValidateScenario, so `argus validate-scenario` without --config is byte-for-byte unchanged.
//
// argus.CheckScenarioSchemas runs the SAME check (buildAMQPRecordTemplate / avroschema.BuildNative
// in Authoring mode) runChainScenarioWithMoneyWrites already applies on every run — reusing it
// rather than a second implementation is what makes "checks the record against the declared schema
// fully" true of validate-scenario and not just of a real run.
func ValidateScenarioWithConfig(body string, c *config.Config) (any, bool, error) {
	out, failed, err := ValidateScenario(body)
	if err != nil || c == nil {
		return out, failed, err
	}
	schemaErrs := argus.CheckScenarioSchemas(scenario.Parse(body), c)
	if len(schemaErrs) == 0 {
		return out, failed, nil
	}
	m, _ := out.(map[string]any)
	if m == nil {
		m = map[string]any{}
	}
	m["valid"] = false
	m["schema_errors"] = schemaErrs
	return m, true, nil
}

// ValidateAll is THE tier-2 answer — the ONE place that decides whether a scenario is valid, and the
// only thing any author path may ask.
//
// ⛔ WHY IT EXISTS. There are three author paths:
//
//	toolcore.ValidateScenario   — author__validate_scenario
//	toolcore.WriteScenario      — author__write_scenario
//	control/cloudtools.go       — the control plane's author_write_scenario
//
// and until this round the last two called scenario.Validate ALONE while only the first added
// argus.ExpectProblems. So a rule that lived in ExpectProblems was REPORTED by validate and
// SILENTLY BYPASSED by both writers: an author was told the scenario was invalid and could store it
// regardless — "either enforced or reported, there is no third state" violated by the product's own
// authoring tools. Every author path now goes through here, and
// TestWriteRefusesEverythingValidateRefuses fails if one stops.
func ValidateAll(body string) (*scenario.Scenario, []scenario.Error) {
	s, verrs := scenario.Validate(body)
	for _, p := range argus.ExpectProblems(s) {
		verrs = append(verrs, scenario.Error{Line: lineOfBullet(body, p.Bullet), Message: p.Message})
	}
	return s, verrs
}

// ValidateScenarioAt validates a scenario referenced by the MCP `scenario` PATH arg,
// resolved RELATIVE to the scenarios dir (symmetry with read/list/write/delete) — FX-2.
// An absolute path is read as-is; a relative path is taken under scenariosDir (the colleague
// passed "<layer>/<file>.md" and it failed because os.ReadFile resolved vs the container cwd).
func ValidateScenarioAt(scenariosDir, p string) (any, bool, error) {
	full, err := scenarioReadPath(scenariosDir, p)
	if err != nil {
		return nil, false, err
	}
	b, err := os.ReadFile(full)
	if err != nil {
		return nil, false, fmt.Errorf("read scenario %q under %s: %w", p, scenariosDir, err)
	}
	return ValidateScenario(string(b))
}

// scenarioReadPath resolves a read path: absolute as-is; relative under scenariosDir (then
// a cwd fallback for back-compat). Errors naming scenariosDir when found in neither.
func scenarioReadPath(scenariosDir, p string) (string, error) {
	if strings.TrimSpace(p) == "" {
		return "", fmt.Errorf("empty scenario path")
	}
	if filepath.IsAbs(p) {
		return p, nil
	}
	if under := filepath.Join(scenariosDir, p); fileExists(under) {
		return under, nil
	}
	if fileExists(p) { // back-compat: already relative to cwd
		return p, nil
	}
	return "", fmt.Errorf("scenario %q not found under the scenarios dir %s (nor cwd)", p, scenariosDir)
}

func fileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}

// WriteScenario — VR-AUTH4. Validate-then-write, atomic (temp + rename). DF-05: the
// destination `path` is resolved RELATIVE to the scenarios dir (symmetry with
// read/list/delete/run, which all key on ScenariosDir) — an absolute path or a `..`
// escape is rejected, and the resolved absolute path is returned so the author can see
// exactly where it landed (no more "wrote it but the runner never looks there").
func WriteScenario(scenariosDir string, body []byte, path string) (any, bool, error) {
	dstPath, err := resolveUnder(scenariosDir, path)
	if err != nil {
		return nil, false, fmt.Errorf("destination: %w", err)
	}
	// VR12-E8 R3 / THE ONE RULE: the SAME validator author__validate_scenario answers with. A write
	// that refused less than validate is the trap this round removes.
	if _, verrs := ValidateAll(string(body)); len(verrs) > 0 {
		return map[string]any{"written": false, "valid": false, "errors": verrs}, true, nil
	}
	if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
		return nil, false, fmt.Errorf("mkdir: %w", err)
	}
	tmp := dstPath + ".tmp"
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return nil, false, fmt.Errorf("write temp: %w", err)
	}
	if err := os.Rename(tmp, dstPath); err != nil {
		return nil, false, fmt.Errorf("rename: %w", err)
	}
	return map[string]any{"written": true, "valid": true, "path": dstPath, "scenarios_dir": scenariosDir}, false, nil
}

// resolveUnder resolves a caller-supplied relative path against base, rejecting absolute
// paths and `..` escapes; returns the cleaned absolute path under base (DF-05).
func resolveUnder(base, rel string) (string, error) {
	if strings.TrimSpace(rel) == "" {
		return "", fmt.Errorf("empty path")
	}
	if filepath.IsAbs(rel) || strings.HasPrefix(rel, "/") || strings.HasPrefix(rel, `\`) {
		return "", fmt.Errorf("path must be RELATIVE to the scenarios dir, got absolute %q", rel)
	}
	absBase, err := filepath.Abs(base)
	if err != nil {
		return "", err
	}
	abs := filepath.Clean(filepath.Join(absBase, rel))
	r, err := filepath.Rel(absBase, abs)
	if err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes the scenarios dir: %q", rel)
	}
	return abs, nil
}

// DeleteScenario — VR-AUTH6 (NEW in M2.5). Idempotent: deleting an absent id is a
// success no-op (DF-DEC-M25-04), never a crash.
func DeleteScenario(e Env, id string) (any, error) {
	p := findScenarioPath(e.ScenariosDir, id)
	if p == "" {
		return map[string]any{"deleted": false, "scenario": id, "note": "not found (idempotent no-op)"}, nil
	}
	if err := os.Remove(p); err != nil {
		return nil, fmt.Errorf("delete %q: %w", id, err)
	}
	return map[string]any{"deleted": true, "scenario": id, "path": p}, nil
}

// ProposeScenario — VR-AUTH1. Scaffolds a valid argus-format draft from a plain-text
// description. DF-16: it is an HONEST skeleton-only INITIALIZER (it does NOT translate
// the intent into the real endpoint/method/status — the agent fills those from the SUT
// source/config, per the initializer/worker model). DF-16 adds LAYER-AWARENESS (the
// right sections per layer — a content layer scaffolds a VERIFY, not the HTTP stub) and
// DF-03 a CLEAN title (no mid-word truncation). Every inferred field is flagged in
// needs_user_review[].
func ProposeScenario(description, targetService, layer string) (any, error) {
	if description == "" {
		return nil, fmt.Errorf("propose-scenario needs a description")
	}
	id := "SVC-001"
	if targetService != "" {
		id = scenarioIDFromService(targetService)
	}
	lyr := canonicalProposeLayer(layer)
	verify, expect, layerNeeds := layerSkeleton(lyr)
	draft := "# Scenario: " + cleanTitle(description) + "\n\n" +
		"## Metadata\n" +
		"- **ID**: " + id + "\n" +
		"- **Layer**: " + lyr + "\n" +
		// VR12-M1/M2: `**Priority**` is gone from the contract. VR12-M3: `**Tags**` must carry
		// exactly ONE dispatch tag, and it is the thing that decides which ENGINE runs the
		// scenario — so the scaffold declares it rather than leaving the author to discover the
		// rule from a refusal. `http` is right for every layer this tool can infer; an `mcp`,
		// `chain` or `ui` scenario is not something a free-text layer hint can tell us, so it is
		// flagged for review instead of guessed.
		"- **Tags**: " + dispatchFor(lyr) + ", proposed\n\n" +
		"## TRIGGER\n" +
		"POST `${INGESTION_URL}/api/v1/resource`\n\n" +
		"```json\n{}\n```\n\n" +
		"## VERIFY\n" + verify + "\n\n" +
		// VR12-E1: the draft this tool hands an author must be VALID under the split, or the very
		// next call — author__validate_scenario — refuses the product's own skeleton. The proposer
		// emits the runnable sub-heading; the author adds `### Non-runnable` when they have something
		// to say about what the test proves.
		"## EXPECT\n### Runnable\n" + expect + "\n\n" +
		"## TIMEOUT\n30s\n\n" +
		// VR12-C1: `## CLEANUP` is REQUIRED and it RUNS. The skeleton must carry a section the
		// validator accepts — but it must NOT invent a deletion for a scenario whose shape the tool
		// cannot know, so it scaffolds the honest exemption and FLAGS it for review. An author who
		// leaves it unchanged has DECLARED that this scenario creates nothing, which is a claim they
		// can be held to; a scaffolded `DELETE` would be a guess wearing a command's authority.
		"## CLEANUP\nN/A — REVIEW THIS: state why this scenario leaves nothing behind, or " +
		"replace this line with a ```sql / ```bash block that removes what it creates.\n"
	needs := append([]string{
		"ID (" + id + " is a placeholder — set the real domain prefix + number)",
		"Layer (assumed \"" + lyr + "\" — confirm the actual layer; chain as `A -> B` if it verifies downstream)",
		"TRIGGER method/path (POST /api/v1/resource is a SKELETON — set the real endpoint + payload from the SUT source/config)",
		"Tags — the scaffold declares the `" + dispatchFor(lyr) + "` dispatch tag. If this scenario is an MCP call, a multi-step chain or a Playwright page, change it to `mcp` / `chain` / `ui`: the tag decides which ENGINE runs it (V30-003)",
		"CLEANUP — the scaffold declares `N/A`. If this scenario writes ANYTHING, replace it with a ```sql or ```bash block: the section is required, it RUNS after every scenario, and it never affects the verdict (V29-015)",
	}, layerNeeds...)
	return map[string]any{
		"drafts": []map[string]any{{"scenario": draft, "needs_user_review": needs}},
		"note":   "skeleton-only initializer (DF-16): fill the real endpoint/method/EXPECT from the SUT source/config, then validate + write.",
	}, nil
}

// ProposeMoneyGuardScenario (T5.4 follow-up) is ProposeScenario's twin for a
// money-handling instance: the control plane cannot offer a POST skeleton against a SUT that moves
// real money ('s rule — a money-handling SUT may only be READ), so generation itself
// changes shape rather than emitting a draft the money guard would only refuse a moment later.
//
// GENERATES, DOES NOT VALIDATE-AGAINST: this always emits a plain HTTP GET against a placeholder
// probe/health/metrics path with CLEANUP N/A, which by construction carries none of
// scenario.MoneyGuardViolations' refusal reasons (no write method, no order/quote/swap/rebalance/
// claim path, no MCP/UI/chain tag, no runnable CLEANUP) — proposeScenario (cloudtools.go) still runs
// the SAME moneyGuardRefusal check on the parsed draft before writing it, exactly as it does for
// author_write_scenario, because a generator that is merely believed safe is not the two-door rule
// the rest of T5.4 follows.
//
// Always "HTTP Ingestion": the other layers' skeletons (Database State's ```sql, Message Flow/External
// Delivery's broker/capture-server checks) have no read-only http form to offer, and a money-handling
// SUT's proposal is GET-only by definition — there is nothing layer-specific left to infer.
func ProposeMoneyGuardScenario(description, targetService string) (any, error) {
	if description == "" {
		return nil, fmt.Errorf("propose-scenario needs a description")
	}
	id := "SVC-001"
	if targetService != "" {
		id = scenarioIDFromService(targetService)
	}
	draft := "# Scenario: " + cleanTitle(description) + "\n\n" +
		"## Metadata\n" +
		"- **ID**: " + id + "\n" +
		"- **Layer**: HTTP Ingestion\n" +
		"- **Tags**: http, proposed\n\n" +
		"## TRIGGER\n" +
		"GET `${INGESTION_URL}/health`\n\n" +
		"## VERIFY\nN/A — a status layer judges by response code; there is no query to run. (A body check in ## EXPECT is enforced — DF-04:\n" +
		"`body contains <x>` anywhere in the answer, or `body has <field> containing <x>` on a\nfield of a JSON answer.)\n\n" +
		"## EXPECT\n### Runnable\n- status=200\n\n" +
		"## TIMEOUT\n30s\n\n" +
		"## CLEANUP\nN/A — this instance is declared money_handling (T5.4/): generation " +
		"offers only a GET against a read-only probe/health/metrics path, which creates nothing to clean up.\n"
	needs := []string{
		"ID (" + id + " is a placeholder — set the real domain prefix + number)",
		"TRIGGER path (/health is a PLACEHOLDER probe/health/metrics path — this instance is " +
			"money_handling, so generation offers only a read-only GET; set the real probe path from the SUT source/config)",
		"Tags — money-handling generation only ever offers the `http` dispatch tag; `mcp`/`ui`/`chain` " +
			"are never scaffolded here because the money guard refuses all three outright",
	}
	return map[string]any{
		"drafts": []map[string]any{{"scenario": draft, "needs_user_review": needs}},
		"note": "skeleton-only initializer (DF-16), GET-ONLY (T5.4/): this instance is " +
			"declared money_handling, so only a read-only probe/health/metrics scaffold is offered. Fill " +
			"the real probe path from the SUT source/config, then validate + write.",
	}, nil
}

// canonicalProposeLayer maps a free-text layer hint to a canonical layer (default HTTP Ingestion).
func canonicalProposeLayer(hint string) string {
	h := strings.ToLower(strings.TrimSpace(hint))
	for _, l := range scenario.CanonicalLayers {
		if l == scenario.AMQPLoadLayer || l == scenario.HTTPLoadLayer {
			continue // no skeleton is offered for it (its `## LOAD` + closed vocabulary are authored by hand, see the skill docs)
		}
		if strings.EqualFold(l, hint) || strings.Contains(h, strings.ToLower(l)) {
			return l
		}
	}
	switch {
	case strings.Contains(h, "db") || strings.Contains(h, "database") || strings.Contains(h, "row"):
		return "Database State"
	case strings.Contains(h, "msg") || strings.Contains(h, "message") || strings.Contains(h, "queue") || strings.Contains(h, "amqp"):
		return "Message Flow"
	case strings.Contains(h, "webhook") || strings.Contains(h, "external") || strings.Contains(h, "delivery"):
		return "External Delivery"
	}
	return "HTTP Ingestion"
}

// dispatchFor returns the dispatch tag a scaffolded scenario should carry (VR12-M3).
//
// ⚠ It can only be `http` or `ui`, because those are the only two a LAYER implies. `mcp` and
// `chain` describe how the scenario CALLS the SUT, which no layer hint can tell us — the scaffold
// says so in needs_user_review rather than guessing, because a wrong dispatch tag routes the
// scenario to the wrong engine.
func dispatchFor(layer string) string {
	if layer == "Web UI" {
		return "ui"
	}
	return "http"
}

// layerSkeleton returns the VERIFY body, EXPECT body, and extra review-flags for a layer (DF-16).
func layerSkeleton(layer string) (verify, expect string, needs []string) {
	switch layer {
	case "Database State":
		// RO-09: scaffold an EXECUTABLE ```sql VERIFY (not prose) — a prose-only Database
		// State VERIFY is rejected by validate_scenario (it would degrade to a SELECT 1 tautology).
		return "```sql\nSELECT <columns> FROM <table> WHERE correlation_id = '${correlation_id}'\n```",
			"- row_count == 1\n- <column> == <value>   (or `no rows` for a negative/rejected case)",
			[]string{"VERIFY (the ```sql query is a SKELETON — set the real table/columns; it MUST be an executable query, not prose — RO-09)", "EXPECT (row_count/columns assumed — set the real DB assertions; use `no rows` for a rejected-input negative test)"}
	case "Message Flow":
		// VR12-VF3: the Message Flow template does its own broker check; there is no query to run,
		// so the honest form is the exemption plus the reason.
		return "N/A — the Message Flow layer's own template inspects the broker for this correlation_id; there is no query to run here.",
			// ⛔ VR12-E4: `- 1 message with correlation_id` is assertion-SHAPED and NO grammar executes
			// it — measured by V30-004, message-flow.jmx reads no `expect.*` property at all; its own
			// template does the correlation-id check. Emitting it taught authors to write a claim that
			// proves nothing. The runnable half now uses a form the grammar TAKES, and the sentence
			// describing the template's own check goes where an un-automatable expected result belongs.
			// AC-D16: a THIRD alternative to `row_count ==` / `no rows` — the scenario provokes a
			// broker REFUSAL (a bad `user_id` property, an unauthorized publish) instead of
			// asserting what landed. `<code>` is a 3-digit AMQP reply code or one of its names
			// (PRECONDITION_FAILED/ACCESS_REFUSED/NOT_FOUND/RESOURCE_LOCKED); the TRIGGER's
			// `User-Id:` line sets the property that provokes it.
			"- row_count == 1\n- <field> == <value>   (or `no rows` for a poison-message case, or `broker refuses with <code>` for an expected-refusal case)\n\n### Non-runnable\n- the Message Flow template finds exactly one message for this correlation_id and none on the DLQ",
			[]string{"VERIFY (the queue check is a SKELETON — set the real exchange/queue)", "EXPECT (assumed; set the real message assertions)"}
	case "External Delivery":
		// VR12-VF3: as above — the External Delivery template polls the capture server itself.
		return "N/A — the External Delivery layer's own template polls the capture server for this correlation_id; there is no query to run here.",
			// ⛔ VR12-E4 / V30-004, as above: external-delivery.jmx reads no `expect.*` property either.
			"- row_count == 1\n- <field> == <value>   (or `no rows` for a rejected case)\n\n### Non-runnable\n- the External Delivery template finds exactly one delivery for this correlation_id on the capture server",
			[]string{"VERIFY (the webhook check is a SKELETON — set the real sink)", "EXPECT (assumed; set the real delivery assertions)"}
	default: // status layers (HTTP Ingestion / Error Path / Rate Limiting / Permissions)
		// ⛔ V31-004 (VR13-BF): this offered ONLY the field-scoped form. An author who wanted "does
		// this string appear anywhere in the answer" had nothing on offer and invented a field name
		// for it — `text` — which since V29-017 means a JSON field literally called `text`. Both forms
		// are named now, with the difference stated, because the difference is the whole defect.
		// ⚠ NEVER break one of these lines before a `##` — the parser reads a leading `##` as a
		// SECTION HEADING and the whole scaffold is then refused by the product's own validator
		// (TestEveryProposedSkeletonPassesTheProductsOwnValidator caught exactly that here).
		// VR12-VF3: a prose-only VERIFY is refused — `Verify.Description` is read by nothing, so it
		// is a section that looks like it does something and does not.
		return "N/A — a status layer judges by response code; there is no query to run. (A body check in ## EXPECT is enforced — DF-04:\n`body contains <x>` anywhere in the answer, or `body has <field> containing <x>` on a\nfield of a JSON answer.)",
			"- status=202",
			// V29-016: the old wording was `assumed status=202 — set the real status`. It TAUGHT the
			// default this row exists to remove: an author who left it alone shipped a scenario judged
			// against a code nobody chose. There is no default any more — the status is REQUIRED
			// (VR12-E6), so the scaffold says so and names the consequence of leaving the placeholder.
			[]string{"EXPECT — `status=202` is a PLACEHOLDER, not a default: a scenario judged by response code MUST declare its real expected status (VR12-E6 refuses it otherwise). Add `body contains <x>` (anywhere in the answer) or `body has <field> containing <x>` (a field of a JSON answer) for body assertions"}
	}
}

// cleanTitle derives a clean, short H1 title from the intent — first line, trimmed at a
// WORD boundary (no mid-word truncation — DF-03).
func cleanTitle(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	const max = 60
	if len(s) <= max {
		return s
	}
	cut := s[:max]
	if sp := strings.LastIndex(cut, " "); sp > 0 {
		cut = cut[:sp]
	}
	return strings.TrimRight(cut, " ,;:-")
}

// --- helpers ---

func writeReport(path string, r *report.Report) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(r, "", "  ")
	return os.WriteFile(path, b, 0o644)
}

func readReport(path string) (*report.Report, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r report.Report
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

func findScenarioPath(dir, id string) string {
	for _, d := range scenario.DiscoverFiles(dir) {
		if d.Scenario.ID == id {
			return d.Path
		}
	}
	return ""
}

func scenarioIDFromService(svc string) string {
	pfx := ""
	for _, r := range svc {
		if r >= 'a' && r <= 'z' {
			pfx += string(r - 32)
		} else if r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			pfx += string(r)
		}
		if len(pfx) >= 4 {
			break
		}
	}
	if pfx == "" {
		pfx = "SVC"
	}
	return pfx + "-001"
}

// tapConfigErrors is V30-004's F-6: a Message Flow scenario that asserts CONTENT needs two facts
// from the SUT's broker config, and a config that carries neither is reported BEFORE a run.
//
// The tap binds a queue of ours to the SUT's incoming exchange with a routing key. An exchange is
// only half an address: a `topic` exchange delivers NOTHING to a queue bound with the wrong key, so
// a tap with no key receives nothing and every content assertion fails for a reason that is OURS,
// not the SUT's. That is precisely the misattribution this product exists to prevent, so the
// operator is told while they can still fix the config.
//
// ⛔ REPORTED HERE BEFORE A RUN — and, since the 0.3.32 rebuild, by the run too for the routing key
// (argus.ErrNoRoutingKey: the scenario is `error` before anything is sent). The authoring path still
// cannot say it: ExpectProblems is handed a SCENARIO and never a *config.Config, and on the control
// plane there is no SUT config in scope at all. validate-config is the one pre-run path holding both.
//
// ⚠ Only for a scenario that actually declares a tap. A Message Flow scenario asserting just
// `- status=202` needs neither fact, and demanding them would refuse a config that is complete for
// what the catalogue actually does.
func tapConfigErrors(c *config.Config, scenariosDir string) []config.ConfigError {
	var out []config.ConfigError
	for _, d := range scenario.DiscoverFiles(scenariosDir) {
		s := d.Scenario
		if argus.PrimaryLayer(s) != "Message Flow" {
			continue
		}
		// ⛔ THE BROKER ENTRY THIS SCENARIO RUNS AGAINST, chosen exactly as argus.DeriveProps chooses it: the plain slot,
		// or the message_broker_targets entry its **Target** names. Judging only the plain slot passed a named entry
		// with no key and flagged a complete named entry (0.3.32 rebuild adversary gate). A **Target** that does not
		// resolve is refused by the named-target check, not here.
		mb := c.Targets.MessageBroker
		sel, err := c.SelectTarget(s)
		if err != nil {
			continue
		}
		if sel != nil && sel.Kind == config.KindMessageBroker {
			mb = sel.MQ
		}
		dbx := scenario.ParseDBExpect(s.RunnableExpect())
		// the same gate DeriveProps applies: a tap only for a POSITIVE content assertion
		if !dbx.HasRows || (len(dbx.Columns) == 0 && dbx.RowCount < 0) {
			continue
		}
		var missing []string
		// blank is missing, as the run treats it (argus.DeriveProps trims the key)
		if mb == nil || strings.TrimSpace(mb.Exchanges["incoming"]) == "" {
			missing = append(missing, "`exchanges.incoming`")
		}
		if mb == nil || strings.TrimSpace(mb.RoutingKeys["incoming"]) == "" {
			missing = append(missing, "`routing_keys.incoming`")
		}
		if len(missing) == 0 {
			continue
		}
		out = append(out, config.ConfigError{Message: fmt.Sprintf(
			"%s asserts message CONTENT, which is read through a queue bound to the SUT's incoming exchange — "+
				"and its broker target (`targets.message_broker`, or the `message_broker_targets` entry it selects) "+
				"declares no %s. An exchange is only half an address: a `topic` "+
				"exchange delivers nothing to a queue bound with the wrong key, so the assertion would fail for a "+
				"reason that is ours, not the SUT's. Declare it, or move the assertion to `### Non-runnable` (V30-004)",
			s.ID, strings.Join(missing, " and "))})
	}
	return out
}
