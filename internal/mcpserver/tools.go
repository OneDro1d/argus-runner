package mcpserver

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/OneDro1d/argus-runner/internal/toolcore"
)

const defaultWindow = "10m"

// Argument descriptions shared by more than one tool (VR10-S1-9: what it is · required or optional ·
// an example shape · what happens when omitted — written for an agent that has never seen Argus).
const (
	correlationArg = "The correlation id of one scenario execution, shaped tr-<run_id>-<SCENARIO>-<hash> (example: \"tr-20260902T134305716-ORDE-017-3f9a1c\"); read it from runner__get_report. Optional in the schema but needed for an answer: omitted, there is no id to match and nothing comes back."
	windowArg      = "Optional. How far back to look, as a duration (example: \"10m\" or \"15m\"). Omitted: 10m. The window is a +/- band ANCHORED to the run when the correlation_id belongs to the last run (so a small window still finds an older run), else relative to now."
)

// DefaultTools returns the 6 in-env tools — the runner__* namespace ONLY (M3 fix-plan C1).
// The in-env server is RUNNER-ONLY: authoring is a cloud-plane capability now (the author__*
// tools live on the CP MCP server, control.CloudTools) — the doc ruling locks in-env = 6 runner
// and "authoring → cloud registry, not disk". The 6 author__* tools this function used to expose
// were the M2.5 single-plane leftover; TestDefaultTools_InEnvRunnerOnly asserts their absence.
// Each tool is a thin handler over internal/toolcore so the MCP path and the CLI produce
// byte-identical data (VR-H1, one core). runner__get_failure_context (the build-exit 7th runner
// tool) was REMOVED in round FX4: the holdout reads via runner__get_report; triage is multi-call
// (get_sagas + get_tail_logs + get_dashboard_url). holdout_test.go asserts its absence.
// The Env supplies per-instance config; tool args (always carrying instance_id) override the
// scenario-specific bits.
func DefaultTools(env toolcore.Env) []Tool {
	tools := []Tool{
		// --- runner namespace (both hats; output role-redacted) — the ONLY in-env namespace ---
		{Name: "runner__validate_config", Namespace: NSRunner, InputSchema: schema(map[string]string{}),
			Description: "Validate THIS instance's config against its scenarios. Single-tenant: the server owns its one config, so pass ONLY instance_id — never a config path, and don't hunt for an argus-config.yaml; any other argument is refused (VR10-S1).",
			Handler: func(a json.RawMessage, _ Principal) Outcome {
				p, _, err := toolcore.ValidateConfig(env)
				return opOutcome(p, err)
			}},
		{Name: "runner__run", Namespace: NSRunner, Async: true, InputSchema: schema(map[string]string{
			"scenario_ref": "Optional. Run exactly one scenario, by its id (example: \"ORDE-017\"). Omitted: no single-scenario filter.",
			"tag":          "Optional. Run only the scenarios carrying this tag (example: \"smoke\"). Omitted: no tag filter. Given together with layer, both must match.",
			"layer":        "Optional. Run only the scenarios whose canonical layer is this one (example: \"HTTP Ingestion\"). Omitted: no layer filter.",
			// VR-F6: PLAN B, and only that. It changes nothing while the control plane is reachable.
			"use_local_scenarios": "Optional, PLAN B only. Pass \"true\" ONLY after the control plane proved unreachable: run the scenarios on local disk anyway, accepting they were NOT verified against the catalog and may be stale. Omitted (the default): while the control plane is reachable this changes nothing; when it is unreachable the run refuses and waits instead.",
		}),
			Description: "Run scenarios ASYNC (real argus/JMeter): returns {status:running, run_id} immediately - poll runner__get_report{run_id}. Selects a SUBSET by tag and/or layer (AND-applied). Syncs the set from the control-plane catalog first; if the CP is unreachable it REFUSES and offers use_local_scenarios rather than silently running what is on disk. Never pass run_id: the server mints it and returns it.",
			Handler: func(a json.RawMessage, _ Principal) Outcome {
				// DF-06: the server dispatch runs this handler in the background (holding the run
				// lock) and injects run_id; Run stamps it into the report. (Synchronous fallback
				// if called directly without a run_id — still stamps a generated one.)
				// VR-F6: the caller's explicit plan-B choice rides on a COPY of the Env for this call
				// only, so one agent opting in cannot change what the next run does.
				renv := env
				renv.UseLocalScenarios = argBool(a, "use_local_scenarios")
				p, _, err := toolcore.Run(renv, argStr(a, "run_id"), argStr(a, "layer"), argStr(a, "tag"), argStr(a, "scenario_ref"))
				return opOutcome(p, err)
			}},
		{Name: "runner__get_report", Namespace: NSRunner, InputSchema: schema(map[string]string{"run_id": "Optional. The run to report on, as returned by runner__run (example: \"20260902T134305716\"). Omitted: the last run on this instance. The report always covers the whole run — find your scenario inside it; there is no per-scenario filter."}),
			Description: "The report.json model for one WHOLE run, scoped to run_id when given (else the last run); product hat: expected values stripped. Find your scenario inside it — a per-scenario filter is not an argument.",
			Handler: func(a json.RawMessage, pr Principal) Outcome {
				p, _, err := toolcore.GetReport(env, pr.Hat, argStr(a, "run_id")) // DF-07
				return opOutcome(p, err)
			}},
		{Name: "runner__get_sagas", Namespace: NSRunner, InputSchema: schema(map[string]string{"correlation_id": correlationArg, "window": windowArg}),
			Description: "The saga timeline (what/why/by-whom) for a correlation id, from Loki.",
			Handler: func(a json.RawMessage, _ Principal) Outcome {
				p, err := toolcore.GetSagas(env, argStr(a, "correlation_id"), windowOf(a))
				return opOutcome(p, err)
			}},
		{Name: "runner__get_tail_logs", Namespace: NSRunner, InputSchema: schema(map[string]string{"correlation_id": correlationArg, "window": windowArg}),
			Description: "Windowed, correlation-scoped log lines (== CLI tail-logs).",
			Handler: func(a json.RawMessage, _ Principal) Outcome {
				p, err := toolcore.TailLogs(env, argStr(a, "correlation_id"), windowOf(a))
				return opOutcome(p, err)
			}},
		{Name: "runner__get_dashboard_url", Namespace: NSRunner, InputSchema: schema(map[string]string{"correlation_id": "Optional. Deep-link the dashboard to this correlation id, shaped tr-<run_id>-<SCENARIO>-<hash> (example: \"tr-20260902T134305716-ORDE-017-3f9a1c\"). Omitted: the general dashboard."}),
			Description: "Grafana links: dashboard_url (scoped to correlation_id when given, else general) + overview_url (always the general dashboard). The URL is templated, not a check the run exists.",
			Handler: func(a json.RawMessage, _ Principal) Outcome {
				return Ok(toolcore.GetDashboardURL(env, argStr(a, "correlation_id")))
			}},
		// NOTE (C1): the 6 author__* tools were REMOVED from the in-env server. Authoring is a
		// cloud-plane capability — see control.CloudTools (author__propose/validate/write/list/read/
		// delete + request_run + get_run_status + get_executor_status). The in-env plane never writes
		// scenarios to disk anymore (doc ruling: authoring → cloud registry, not disk).
	}
	// UC031/C8: instance_id is MANDATORY on EVERY tool — an omitted id is a structured parameter error,
	// and a non-matching id is REJECTED (never silently serve the local instance's data). Both planes
	// enforce this; the cloud plane does so via resolveWorkspace.
	for i := range tools {
		inner := tools[i].Handler
		// The check itself, side-effect free, so it can ALSO run as a PreCheck before an async
		// tool takes the run lock / CP fence (see Tool.PreCheck). Sharing one closure keeps the
		// pre-check and the handler guard from drifting apart.
		check := func(a json.RawMessage) *Outcome {
			id := argStr(a, "instance_id")
			if id == "" {
				o := ToolErr(map[string]any{"error": "instance_id is required"})
				return &o
			}
			if id != env.Instance {
				o := ToolErr(map[string]any{"error": fmt.Sprintf("unknown instance_id %q — this server serves instance %q only", id, env.Instance)})
				return &o
			}
			return nil
		}
		tools[i].PreCheck = func(a json.RawMessage, _ Principal) *Outcome { return check(a) }
		tools[i].Handler = func(a json.RawMessage, pr Principal) Outcome {
			if o := check(a); o != nil {
				return *o
			}
			return inner(a, pr)
		}
	}
	return tools
}

// opOutcome maps a toolcore result to an MCP outcome: an operational error is the
// tool-error plane (result.isError:true + a structured error payload); otherwise success.
func opOutcome(payload any, err error) Outcome {
	if err != nil {
		return ToolErr(map[string]any{"error": err.Error()})
	}
	return Ok(payload)
}

func schema(props map[string]string) map[string]any {
	p := map[string]any{"instance_id": map[string]any{"type": "string",
		// VR10-S1-9 / acceptance 10 (shared with V28-017): the description never names `local` — the
		// agent passes the id its environment registered and the router translates on the runner plane.
		"description": "REQUIRED on every tool (UC031/C8). The id of the instance this call is for, exactly as your environment names it (example: \"orders-k3d\"). This executor serves one instance and refuses the call when instance_id is omitted or names another instance."}}
	for k, desc := range props {
		p[k] = map[string]any{"type": "string", "description": desc}
	}
	// instance_id is declared REQUIRED so a conforming client refuses the call locally,
	// rather than relying on the server guard alone (gate-2026-08-06).
	// additionalProperties:false (VR10-S1) closes the object the same way: a conforming client never
	// sends an undeclared key, and Server.Dispatch refuses the ones that arrive anyway.
	return map[string]any{"type": "object", "properties": p, "required": []string{"instance_id"}, "additionalProperties": false}
}

// argBool reads a boolean argument. A STRING "true" counts, because an MCP client that types its
// arguments loosely (or a human pasting JSON) will send one, and silently reading that as false
// would drop a deliberate choice on the floor - which for use_local_scenarios means refusing a run
// the caller explicitly authorised, then telling them to do the thing they just did.
func argBool(args json.RawMessage, key string) bool {
	if len(args) == 0 {
		return false
	}
	var m map[string]any
	if err := json.Unmarshal(args, &m); err != nil {
		return false
	}
	switch v := m[key].(type) {
	case bool:
		return v
	case string:
		return strings.EqualFold(v, "true") || v == "1" || strings.EqualFold(v, "yes")
	}
	return false
}

func argStr(args json.RawMessage, key string) string {
	if len(args) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(args, &m); err != nil {
		return ""
	}
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func windowOf(args json.RawMessage) string {
	if w := argStr(args, "window"); w != "" {
		return w
	}
	return defaultWindow
}
