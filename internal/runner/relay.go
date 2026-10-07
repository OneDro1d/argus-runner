package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/OneDro1d/argus-runner/internal/argus"
	"github.com/OneDro1d/argus-runner/internal/federation"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/role"
	"github.com/OneDro1d/argus-runner/internal/toolcore"
)

// CommandFunc executes one relayed builder command envelope through the SAME in-process code the
// local router's runner__* tools use (toolcore), never a shell, and returns its answer already
// reduced to whatever the verb's own custody rule allows through. A non-nil error becomes the
// command's error field on the push back — the executor answers every command it dequeues, never
// silently drops one.
type CommandFunc func(ctx context.Context, verb string, args json.RawMessage) (json.RawMessage, error)

// RelayedReport is the custody-safe projection of a report.Report for the builder-relay get_report
// answer (AC-17): the verdict, failing scenario ids, correlation ids, a dashboard link and, in build
// mode, the AC-10 build-record object id — NEVER an expected assertion, an observed value, or a
// scenario body (the struct simply has no field for either, the same locality-by-schema discipline
// federation.ResultsPush uses). A `final` run's relayed get_report holds the verdict fields ONLY —
// the control plane persists nothing from a result beyond the verdict fields it already keeps on
// run_ledger, and the relay must never hand a builder more than that ledger already carries.
type RelayedReport struct {
	Status              string   `json:"status"`
	RunID               string   `json:"run_id,omitempty"`
	Mode                string   `json:"mode,omitempty"`
	Passed              int      `json:"passed"`
	Failed              int      `json:"failed"`
	Errored             int      `json:"errored"`
	Degraded            int      `json:"degraded"`
	Total               int      `json:"total"`
	FailingScenarios    []string `json:"failing_scenarios,omitempty"`
	CorrelationIDs      []string `json:"correlation_ids,omitempty"`
	DashboardURL        string   `json:"dashboard_url,omitempty"`
	BuildRecordObjectID string   `json:"build_record_object_id,omitempty"`
}

// reduceReport builds the RelayedReport a relayed get_report answers with — the ONE place a report
// is reduced before any of it rides toward the control plane. rep has already been through the
// product hat's own redaction (role.Product strips `expected`, toolcore.GetReport); this strips
// `observed` too, and everything else the custody rule does not name, by simply never copying it.
func reduceReport(rep *report.Report, dashboardURL string) RelayedReport {
	status := "passed"
	switch {
	case rep.Summary.Failed > 0 || rep.Summary.Errored > 0:
		status = "failed"
	case rep.Summary.Degraded > 0:
		// the assertions held but the SUT was in distress during the run: never relayed as a pass
		status = "degraded"
	}
	out := RelayedReport{
		Status: status, RunID: rep.RunID, Mode: rep.Mode,
		Passed: rep.Summary.Passed, Failed: rep.Summary.Failed, Errored: rep.Summary.Errored, Degraded: rep.Summary.Degraded,
		Total: rep.Summary.Total,
	}
	// The verdict only, for every run that is not the builder's own: final, scheduled and rehearsal ran the
	// certification set, and an unknown or empty mode is not proven to have run anything else. Keyed on
	// the mode the EXECUTOR recorded from the assignment (execute.go, toolcore.Env.RunMode); before
	// this tested rep.Mode == "final", which the "ci" stamp never matched.
	if !rep.ScenariosVisibleToBuilder() {
		return out
	}
	for _, l := range rep.Layers {
		for _, sc := range l.Scenarios {
			if sc.Status != "passed" {
				out.FailingScenarios = append(out.FailingScenarios, sc.ID)
			}
			if sc.CorrelationID != "" {
				out.CorrelationIDs = append(out.CorrelationIDs, sc.CorrelationID)
			}
		}
	}
	out.DashboardURL = dashboardURL
	out.BuildRecordObjectID = rep.BuildRecordObjectID
	return out
}

// NewCommandFunc builds the executor-side command executor for AC-17's relay. Each of the four verbs
// the builder namespace publishes runs through the exact toolcore call the local router's own
// runner__* tools make, against the SAME Env this package already builds for a real run (execEnv) —
// one implementation, reached from a third transport now (local router, direct CLI, relay).
//
// `run` stays async at the SOURCE: it mints a run id, starts toolcore.Run in the background, and
// answers immediately with {running, run_id} — the relay's bounded wait is for this acknowledgement,
// never for the run to finish. The verdict arrives later through a separate relayed get_report.
func NewCommandFunc(cfg ExecConfig) CommandFunc {
	env := execEnv(cfg, cfg.ScenariosDir)
	// the directory a compare run of THIS executor stored its outputs in (env.Instance is the tool
	// identity every run's results are written under)
	outputsBase := argus.OutputsBase(filepath.Join(cfg.ResultsRoot, env.Instance))
	// item 7b: wire the SAME catalog client the poll loop already authenticates with (Bootstrap,
	// runner.go) into THIS env — the relay's own, separate from the federated run path's per-
	// assignment Env — so a relayed run/validate_config on a catalog-driven executor is not stuck
	// with the (possibly empty) local ScenariosDir alone.
	if cfg.CatalogClient != nil {
		client := cfg.CatalogClient
		env.SetFetcher = func(ctx context.Context, layer, tag, scenarioID string) ([]toolcore.CatalogScenario, error) {
			sel := federation.Selection{Layer: layer, Tag: tag, ScenarioRef: scenarioID}
			resp, ferr := client.FetchSet(ctx, relayRunScope(sel), sel)
			if ferr != nil {
				return nil, relayCatalogErr(ferr)
			}
			out := make([]toolcore.CatalogScenario, 0, len(resp.Scenarios))
			for _, sc := range resp.Scenarios {
				out = append(out, toolcore.CatalogScenario{Path: sc.Path, Body: sc.Body})
			}
			return out, nil
		}
		env.SetHasher = func(ctx context.Context, layer, tag, scenarioID string) (string, error) {
			sel := federation.Selection{Layer: layer, Tag: tag, ScenarioRef: scenarioID}
			h, herr := client.FetchSetHash(ctx, relayRunScope(sel), sel)
			return h, relayCatalogErr(herr)
		}
	}
	return func(ctx context.Context, verb string, args json.RawMessage) (json.RawMessage, error) {
		switch verb {
		case "validate_config":
			p, _, err := toolcore.ValidateConfig(env)
			if err != nil {
				return nil, err
			}
			return json.Marshal(p)

		case "get_dashboard_url":
			var a struct {
				CorrelationID string `json:"correlation_id"`
			}
			_ = json.Unmarshal(args, &a)
			return json.Marshal(toolcore.GetDashboardURL(env, a.CorrelationID))

		case "get_report":
			var a struct {
				RunID string `json:"run_id"`
			}
			_ = json.Unmarshal(args, &a)
			p, _, err := toolcore.GetReport(env, role.Product, a.RunID)
			if err != nil {
				return nil, err
			}
			// item 7a (msgbus tester 2026-09-28): a run_id with no on-disk report yet (still
			// running, or unknown) answers with a status map (status/run_id/note — see
			// toolcore.GetReport's DF-07 not_found/pending_or_unknown cases), not a *report.Report.
			// That is a normal answer, never a decode error — pass it through as-is.
			if m, ok := p.(map[string]any); ok {
				return json.Marshal(m)
			}
			rep, ok := p.(*report.Report)
			if !ok {
				return nil, fmt.Errorf("get_report: unexpected report shape %T", p)
			}
			var dash string
			if dm, ok := toolcore.GetDashboardURL(env, "").(map[string]any); ok {
				dash, _ = dm["dashboard_url"].(string)
			}
			return json.Marshal(reduceReport(rep, dash))

		// get_full_report (P1 #8, msgbus tester 2026-09-27) is the author-scope twin of get_report
		// above: the SAME on-disk run file (toolcore.GetReport, per-run-attributed — RO-06), read with
		// role.Test instead of role.Product, so redactExpected is a no-op (role.Test.MustRedactExpected()
		// is false) and the answer is marshaled WHOLE — never through reduceReport, which is the
		// runner__get_report holdout rule and stays exactly as it was. Enqueued ONLY by the control
		// plane's author_get_report tool (internal/control/cloudtools.go); the builder-relay
		// runner__get_report tool hard-codes verb "get_report" and its closed schema has no way to ask
		// for "full", so this verb existing changes nothing about what a runner-scope caller can reach.
		case "get_full_report":
			var a struct {
				RunID string `json:"run_id"`
			}
			_ = json.Unmarshal(args, &a)
			p, _, err := toolcore.GetReport(env, role.Test, a.RunID)
			if err != nil {
				return nil, err
			}
			// item 7a: same not_found/pending_or_unknown map passthrough as get_report above — the
			// SAME toolcore.GetReport call produces the SAME map shape for either hat.
			if m, ok := p.(map[string]any); ok {
				return json.Marshal(m)
			}
			rep, ok := p.(*report.Report)
			if !ok {
				return nil, fmt.Errorf("get_full_report: unexpected report shape %T", p)
			}
			return json.Marshal(rep)

		case "run":
			var a struct {
				ScenarioRef string `json:"scenario_ref"`
				Tag         string `json:"tag"`
				Layer       string `json:"layer"`
			}
			_ = json.Unmarshal(args, &a)
			runID := argus.NewRunID()
			log := cfg.Log
			go func() {
				// item 7b: a relayed run must never fail SILENTLY — before this the error was
				// discarded outright (`_, _, _ = toolcore.Run(...)`), which is exactly what left the
				// msgbus builder's failed relayed run with no report file, no log line and no ledger
				// row anywhere on the executor. Named by run id, so it is findable against the same
				// id the caller was handed above.
				if _, _, rerr := toolcore.Run(env, runID, a.Layer, a.Tag, a.ScenarioRef); rerr != nil && log != nil {
					log("relayed run %s failed: %v", runID, rerr)
				}
			}()
			return json.Marshal(map[string]any{"running": true, "run_id": runID})

		// get_output (ARGUS-CMP-3, design 10.3, executor side only) answers the drill-down of a compare
		// run: the stored output file of ONE sample of ONE check of ONE run, straight from this executor's
		// disk. It is the only place a recorded body ever leaves the executor, and it is enqueued only by an
		// author tool on the control plane (no runner__* tool names it). The four inputs are checked by
		// argus.ReadOutputFile so nothing outside the outputs directory can be read, and every failure is a
		// closed error that carries no filesystem path and no echo of the input.
		case "get_output":
			var a struct {
				RunID      string `json:"run_id"`
				ScenarioID string `json:"scenario_id"`
				Step       string `json:"step"`
				Sample     *int   `json:"sample"`
			}
			if err := json.Unmarshal(args, &a); err != nil || a.Sample == nil {
				return nil, argus.ErrBadOutputRef
			}
			b, err := argus.ReadOutputFile(outputsBase, a.RunID, a.ScenarioID, a.Step, *a.Sample)
			if err != nil {
				return nil, err
			}
			return json.RawMessage(b), nil

		default:
			return nil, fmt.Errorf("unknown relayed verb %q", verb)
		}
	}
}

// relayRunScope classifies a selection for the CP materialize/set-hash call, matching the
// request_run scope vocabulary — the SAME rule cmd/argus/main.go's runScopeOf applies for the
// direct-run and in-env-serve catalog wiring (item 7b: one rule, three callers).
func relayRunScope(sel federation.Selection) string {
	switch {
	case sel.ScenarioRef != "":
		return "single"
	case sel.Tag != "":
		return "tag"
	case sel.Layer != "":
		return "layer"
	default:
		return "full"
	}
}

// relayCatalogErr translates the federation client's REFUSED-credential sentinel into toolcore's —
// the SAME translation cmd/argus/main.go's catalogErr performs for the other two catalog-wiring
// call sites, duplicated here (rather than exported and shared) because cmd/argus imports this
// package and a shared helper would need to live somewhere both can reach; toolcore deliberately
// does not import the federation client (relay.go's own env-building comment gives the reason).
func relayCatalogErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrCatalogUnauthorized) {
		return fmt.Errorf("%w: %v", toolcore.ErrCatalogUnauthorized, err)
	}
	return err
}
