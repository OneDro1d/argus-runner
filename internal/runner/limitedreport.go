package runner

import (
	"bytes"
	"encoding/json"
	"errors"
	"regexp"
	"strings"

	"github.com/OneDro1d/argus-runner/internal/report"
)

// AC-F2 — the LIMITED report: what the relayed verb get_limited_report answers (control-plane tool
// author_get_report_limited). It is the run's report with every field that can carry something the app returned
// left out, cut HERE, on the executor, before any of it travels: the control plane never holds the full report for
// this verb, so the app's data never reaches it, apart from its status codes (below). A load test's errors are sent
// only as words and counts (AC-F2.2):
// limitedReport projects the load records (report.LoadErrorsByKind, report.LoadStepWithoutTexts) before the cut.
//
// It is a KEEP LIST, not a remove list. limitedProject copies a field only if its path is in limitedSent; every
// other field — including one added to a report type later — is not sent, at any depth.
//
// ⛔ THE RULE FOR WHOEVER ADDS A REPORT FIELD: author_get_report (verb get_full_report) sends every field, always.
// List the new field in limitedSent only after checking that NO writer of it can put there anything the app
// returned — its body, its error text, a header, a number it produced, a value saved from its answer
// (${saved.<var>}), or another server's reply. Otherwise list it in limitedNotSent. A field in neither list fails
// TestLimitedReport_EveryReportFieldIsClassified. The one kind of answer that IS sent is a protocol's own status code: an HTTP status
// (observed_status, observed_status_codes, an HTTP load step's status:<code> class) and, since AC-F2.1, a db step's
// SQLSTATE (observed_sqlstate) — the two AC-F2.1 fields each in a shape that holds nothing else.
//
// Paths are the field's JSON path: "a.b" for an object key, "a[].b" for the elements of an array. Every list entry
// is one LEAF (a field whose type is not a struct: a scalar, a slice of scalars, a map of scalars, time.Time).

// limitedSent is the keep list: the fields author_get_report_limited sends. Written by Argus, by the author, by the
// operator's config, by Kubernetes about the namespace's shape, or an Argus measurement (a time, a count).
var limitedSent = []string{
	// the run
	"project", "timestamp", "run_id", "mode", "note", "scenario_source", "claims_as_written",
	"summary.total", "summary.passed", "summary.failed", "summary.skipped", "summary.errored", "summary.degraded",
	"summary.not_applicable", "summary.rate_limit_pauses", "summary.rate_limited_scenarios", "summary.rate_limit_cap_hit",
	"summary.note",

	// the environment's shape, read from Kubernetes (envcapture)
	"environment.captured", "environment.reason", "environment.namespace", "environment.captured_at",
	"environment.pods[].name", "environment.pods[].owner_kind", "environment.pods[].owner_name", "environment.pods[].node",
	"environment.pods[].containers[].name", "environment.pods[].containers[].image", "environment.pods[].containers[].image_id",
	"environment.pods[].containers[].requests_cpu", "environment.pods[].containers[].requests_memory",
	"environment.pods[].containers[].limits_cpu", "environment.pods[].containers[].limits_memory",
	"environment.workloads[].kind", "environment.workloads[].name",
	"environment.workloads[].replicas_desired", "environment.workloads[].replicas_ready",
	"environment.nodes[].name", "environment.nodes[].allocatable_cpu", "environment.nodes[].allocatable_memory",
	"environment.warnings[]", "environment.fingerprint",

	// faults the run applied, and the run's preconditions (probes in the executor's own namespace)
	"faults[].step", "faults[].op", "faults[].target", "faults[].params", "faults[].applied_at", "faults[].restored_at",
	"faults[].restore_outcome", "faults[].toxiproxy",
	"preconditions[].id", "preconditions[].kind", "preconditions[].verdict", "preconditions[].positive",
	"preconditions[].negative", "preconditions[].residue",

	// each check
	"layers[].layer",
	"layers[].scenarios[].id", "layers[].scenarios[].status", "layers[].scenarios[].duration_ms",
	"layers[].scenarios[].correlation_id",
	"layers[].scenarios[].failure.expected",
	"layers[].scenarios[].failure.failed_body_check.index", "layers[].scenarios[].failure.failed_body_check.bullet",
	"layers[].scenarios[].cleanup[].form", "layers[].scenarios[].cleanup[].outcome", "layers[].scenarios[].cleanup[].duration_ms",
	"layers[].scenarios[].faults[].step", "layers[].scenarios[].faults[].op", "layers[].scenarios[].faults[].target",
	"layers[].scenarios[].faults[].params", "layers[].scenarios[].faults[].applied_at",
	"layers[].scenarios[].faults[].restored_at", "layers[].scenarios[].faults[].restore_outcome",
	"layers[].scenarios[].faults[].toxiproxy",
	"layers[].scenarios[].not_applicable.precondition", "layers[].scenarios[].not_applicable.kind",
	"layers[].scenarios[].not_applicable.positive", "layers[].scenarios[].not_applicable.negative",
	"layers[].scenarios[].residue",
	"layers[].scenarios[].req_success", "layers[].scenarios[].req_failed", "layers[].scenarios[].req_error",
	"layers[].scenarios[].rate_limited", "layers[].scenarios[].rate_limited_retries",
	"layers[].scenarios[].unexecuted[].bullet", "layers[].scenarios[].unexecuted[].reason",
	"layers[].scenarios[].assertions_enforced[]", "layers[].scenarios[].assertions_enforced_count",
	"layers[].scenarios[].observed_status", "layers[].scenarios[].observed_status_codes[]",

	// each chain step: the claims as written (${saved.<var>} unbound), never the app's text or data. AC-F2.1: plus the
	// code the step got, each in a field whose shape can hold nothing else — observed_status, an http step's HTTP status,
	// written only when it is a three-digit code 100-999 (chain.observedHTTPStatus), the same kind of code as the
	// check-level observed_status above; observed_sqlstate, a db step's SQLSTATE, written only when it is exactly 5
	// characters of 0-9 / A-Z (chain.observedSQLState) and never the database's message
	"layers[].scenarios[].steps[].name", "layers[].scenarios[].steps[].status", "layers[].scenarios[].steps[].correlation_id",
	"layers[].scenarios[].steps[].assertions_enforced[]", "layers[].scenarios[].steps[].assertions_enforced_count",
	"layers[].scenarios[].steps[].failed_claims[].claim", "layers[].scenarios[].steps[].failed_claims_omitted",
	"layers[].scenarios[].steps[].elapsed_ms", "layers[].scenarios[].steps[].ran_after_failure",
	"layers[].scenarios[].steps[].rate_limited",
	"layers[].scenarios[].steps[].observed_status", "layers[].scenarios[].steps[].observed_sqlstate",

	// load numbers, and each load error's WORD and count (AC-F2.2): kind is one of report.LoadErrorKinds, chosen from
	// the error type, and a broker pause's kind is memory, disk or other (report.BlockedKinds), sent with its times.
	// limitedReport projects the load records with report.LoadErrorsByKind / report.LoadStepWithoutTexts before the cut,
	// which sums the counts per word, maps any kind off the closed list to other, and drops the reason: the load tool's
	// message (it can quote a short part of a malformed answer) and the broker's words stay in the full report. An HTTP
	// load step's errors map is keyed by closed classes, status:<code> among them (httpload.errorClass): a status code
	// the app answered.
	"layers[].scenarios[].load.samples", "layers[].scenarios[].load.p50_ms", "layers[].scenarios[].load.p95_ms",
	"layers[].scenarios[].load.p99_ms", "layers[].scenarios[].load.error_rate", "layers[].scenarios[].load.target_p95_ms",
	"layers[].scenarios[].load.max_error_rate", "layers[].scenarios[].load.breached", "layers[].scenarios[].load.survival_checked",
	"layers[].scenarios[].load.degraded", "layers[].scenarios[].load.degraded_note",
	"layers[].scenarios[].load.errors[].kind", "layers[].scenarios[].load.errors[].count",
	"layers[].scenarios[].load.timeline_bucket_s", "layers[].scenarios[].load.timeline[].at_s",
	"layers[].scenarios[].load.timeline[].started", "layers[].scenarios[].load.timeline[].answered",
	"layers[].scenarios[].load.timeline[].failed",
	"layers[].scenarios[].load.generator_cpu_throttled_share", "layers[].scenarios[].load.generator_not_measured",
	"layers[].scenarios[].load_driver", "layers[].scenarios[].load_target", "layers[].scenarios[].load_stopped_at_step",
	"layers[].scenarios[].load_steps[].step", "layers[].scenarios[].load_steps[].sessions", "layers[].scenarios[].load_steps[].status",
	"layers[].scenarios[].load_steps[].window_seconds", "layers[].scenarios[].load_steps[].offered_per_s",
	"layers[].scenarios[].load_steps[].sent_per_s", "layers[].scenarios[].load_steps[].confirmed_per_s",
	"layers[].scenarios[].load_steps[].delivered_per_s", "layers[].scenarios[].load_steps[].delivered_ratio",
	"layers[].scenarios[].load_steps[].publish_confirm_us.min", "layers[].scenarios[].load_steps[].publish_confirm_us.p50",
	"layers[].scenarios[].load_steps[].publish_confirm_us.p75", "layers[].scenarios[].load_steps[].publish_confirm_us.p95",
	"layers[].scenarios[].load_steps[].publish_confirm_us.p99", "layers[].scenarios[].load_steps[].publish_confirm_us.max",
	"layers[].scenarios[].load_steps[].publish_deliver_us.min", "layers[].scenarios[].load_steps[].publish_deliver_us.p50",
	"layers[].scenarios[].load_steps[].publish_deliver_us.p75", "layers[].scenarios[].load_steps[].publish_deliver_us.p95",
	"layers[].scenarios[].load_steps[].publish_deliver_us.p99", "layers[].scenarios[].load_steps[].publish_deliver_us.max",
	"layers[].scenarios[].load_steps[].response_us.min", "layers[].scenarios[].load_steps[].response_us.p50",
	"layers[].scenarios[].load_steps[].response_us.p75", "layers[].scenarios[].load_steps[].response_us.p95",
	"layers[].scenarios[].load_steps[].response_us.p99", "layers[].scenarios[].load_steps[].response_us.max",
	"layers[].scenarios[].load_steps[].errors", "layers[].scenarios[].load_steps[].blocked[].since_ms",
	"layers[].scenarios[].load_steps[].blocked[].until_ms", "layers[].scenarios[].load_steps[].blocked[].kind",
	"layers[].scenarios[].load_steps[].blocked_seconds",
	"layers[].scenarios[].load_steps[].generator_limited", "layers[].scenarios[].load_steps[].restarts_delta",
	"layers[].scenarios[].load_steps[].not_ready", "layers[].scenarios[].load_steps[].comfortable",
	"layers[].scenarios[].load_steps[].generator_cpu_throttled_share", "layers[].scenarios[].load_steps[].generator_not_measured",
	"layers[].scenarios[].load_steps[].error_reasons[].kind", "layers[].scenarios[].load_steps[].error_reasons[].count",
}

// limitedNotSent is every field author_get_report_limited does NOT send, because a writer of it can put there
// something the app returned. Listed so the classification test can tell "decided: not sent" from "never decided".
var limitedNotSent = []string{
	// what the app returned where a check failed
	"layers[].scenarios[].failure.observed",
	"layers[].scenarios[].steps[].observed",
	"layers[].scenarios[].steps[].failed_claims[].observed",
	"layers[].scenarios[].steps[].body_excerpt",
	"layers[].scenarios[].mcp_envelope",
	"layers[].scenarios[].steps[].mcp_envelope",
	// the issue's own list names this field (report.go CleanupResult.Observed) among the app's data
	"layers[].scenarios[].cleanup[].observed",
	// the app's own Retry-After wait
	"layers[].scenarios[].paused_ms",
	// the build-record hub's own reply: its text can be quoted in the note, and the id is whatever string it returned
	"build_record_note", "build_record_object_id",
	// the broker's own words when it blocked a load test, and the load tool's message for a request that got no HTTP
	// status (it can quote a short part of a malformed answer): AC-F2.2 sends their words (kind) instead
	"layers[].scenarios[].load_steps[].blocked[].reason",
	"layers[].scenarios[].load.errors[].reason",
	"layers[].scenarios[].load_steps[].error_reasons[].reason",
	// what the app's AI agent's sandbox refused: the whole block
	"layers[].scenarios[].sandbox_policy.source", "layers[].scenarios[].sandbox_policy.sandbox",
	"layers[].scenarios[].sandbox_policy.window.from", "layers[].scenarios[].sandbox_policy.window.to",
	"layers[].scenarios[].sandbox_policy.coverage", "layers[].scenarios[].sandbox_policy.coverage_reason",
	"layers[].scenarios[].sandbox_policy.denied_count",
	"layers[].scenarios[].sandbox_policy.events[].time", "layers[].scenarios[].sandbox_policy.events[].class",
	"layers[].scenarios[].sandbox_policy.events[].action", "layers[].scenarios[].sandbox_policy.events[].target",
	"layers[].scenarios[].sandbox_policy.events[].process", "layers[].scenarios[].sandbox_policy.events[].rule",
	"layers[].scenarios[].sandbox_policy.events[].reason",
	"layers[].scenarios[].sandbox_policy.events_omitted", "layers[].scenarios[].sandbox_policy.shared_with[]",
	// a comparison run's recorded outputs: the Tolerance numbers, and the app's status, body length and digests
	"layers[].scenarios[].outputs[].v", "layers[].scenarios[].outputs[].step", "layers[].scenarios[].outputs[].sample",
	"layers[].scenarios[].outputs[].state", "layers[].scenarios[].outputs[].reason", "layers[].scenarios[].outputs[].status",
	"layers[].scenarios[].outputs[].parts.status", "layers[].scenarios[].outputs[].parts.headers",
	"layers[].scenarios[].outputs[].parts.body", "layers[].scenarios[].outputs[].hash",
	"layers[].scenarios[].outputs[].body_kind", "layers[].scenarios[].outputs[].body_bytes",
	"layers[].scenarios[].outputs[].truncated", "layers[].scenarios[].outputs[].masks_applied",
	"layers[].scenarios[].outputs[].values[].path", "layers[].scenarios[].outputs[].values[].rule",
	"layers[].scenarios[].outputs[].values[].value",
	"layers[].scenarios[].outputs[].load.samples", "layers[].scenarios[].outputs[].load.p50_ms",
	"layers[].scenarios[].outputs[].load.p95_ms", "layers[].scenarios[].outputs[].load.p99_ms",
	"layers[].scenarios[].outputs[].load.error_rate",
}

// limitedReportNote rides every limited REPORT, so a reader never takes a missing field for an empty one.
const limitedReportNote = "limited report: built by the executor from a keep list — the fields that can carry what " +
	"the app returned (observed values, response excerpts, the app's own MCP answers, sandbox events, recorded " +
	"comparison outputs, cleanup output, the app's Retry-After wait, a load error's message, a load target's blocked " +
	"reason, the build-record hub's reply) were not sent in this answer; a chain http step's status code " +
	"(steps[].observed_status) and a chain db step's SQLSTATE (steps[].observed_sqlstate, never the database's message) " +
	"are sent; each load error is sent as a word (refused, timeout, reset, unknown_host, tls, protocol or other) and a " +
	"count, and a broker pause as memory, disk or other and its times — never the load tool's message or the broker's " +
	"words; argus get-report in your environment shows the full report"

// A refusal is answered as a STATUS ANSWER, in the shape of toolcore.GetReport's not_found / pending_or_unknown maps
// (status, a closed reason, a fixed note, and the run id only when it passed the check) — never as an error. A
// command's error text is rewritten by AC-F1's rule (ownwords.go: an error no Argus site vouched for is sent as one
// generic sentence); a command's result is sent as it is, so the author reads why. Every byte of a refusal is
// Argus's own.
const (
	limitedRefusedRunID         = "run_id"
	limitedRefusedOlderExecutor = "written_by_an_older_executor"

	limitedRunIDNote = "refused: run_id is required and must be a run id (letters, digits, '.', '_' or '-', up to " +
		"128, no '..'); nothing was read"
	// a report that lacks report.ClaimsAsWritten: an older executor wrote it, and its claim texts can hold a value
	// saved from the app's answer, which the report no longer knows and so cannot be cut out afterwards
	limitedNotVouchedNote = "refused: this run's report was written by an older executor, whose claim texts can hold " +
		"a value saved from the app's answers, so nothing was sent; run the check again on this executor to read its " +
		"limited report"
)

// limitedRefusal is the status answer for a refused request; runID is set only when it passed limitedRunID.
func limitedRefusal(reason, runID, note string) (json.RawMessage, error) {
	m := map[string]any{"status": "refused", "reason": reason, "note": note}
	if runID != "" {
		m["run_id"] = runID
	}
	return json.Marshal(m)
}

// The errors get_limited_report answers with when the executor itself fails (neither path is reachable through
// toolcore.GetReport today: with a checked run id it answers a report or a status map). Each is a fixed sentence,
// never an underlying error's text, so the command's error text (relay_commands.error_text) cannot carry anything
// the app returned; ownWordsf vouches for it (AC-F1, ownwords.go), so the author reads it as written. The cause of a
// failure reaches this executor's own log, where the sentence sends the author: the relay case logs a read failure
// itself (cfg.Log), and every other failure with a cause is sent as the same sentence through withhold, which keeps it.
var (
	errLimitedUnreadable  = ownWordsf("get_limited_report: this run's report could not be read on the executor; see the executor's log")
	errLimitedUnencodable = ownWordsf("get_limited_report: the limited report could not be built on the executor; see the executor's log")
)

// limitedRunIDPattern: one file-name component of a run id (argus.NewRunID writes "20260902T134305716"). The run
// id is joined into a path on the executor (toolcore runReportPath), so nothing that could leave runs/ is accepted.
var limitedRunIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9._-]{0,127}$`)

// limitedRunID reads and checks the verb's run_id. A missing one is refused: with an empty run id GetReport would
// read the latest report.json and could answer with its decode error's text.
func limitedRunID(args json.RawMessage) (string, bool) {
	var a struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", false
	}
	if !limitedRunIDPattern.MatchString(a.RunID) || strings.Contains(a.RunID, "..") {
		return "", false
	}
	return a.RunID, true
}

// limitedReport is the answer of get_limited_report for one run's report (runID, already checked): the report's JSON
// form cut to the keep list, plus limitedReportNote. A report without the ClaimsAsWritten stamp is refused whole: the
// keep list sends the claim texts, and only the stamp says they hold no saved value.
func limitedReport(rep *report.Report, runID string) (json.RawMessage, error) {
	if !rep.ClaimsAsWritten {
		return limitedRefusal(limitedRefusedOlderExecutor, runID, limitedNotVouchedNote)
	}
	// Each failure sends only errLimitedUnencodable's sentence; the cause is withheld for this executor's own log
	// (AC-F1's withhold: runCommands logs it), where the sentence sends the author.
	full, err := json.Marshal(limitedLoadWithoutTexts(rep))
	if err != nil {
		return nil, withhold(errLimitedUnencodable.Error(), err)
	}
	dec := json.NewDecoder(bytes.NewReader(full))
	dec.UseNumber() // numbers pass through as written, never through float64
	var tree any
	if err := dec.Decode(&tree); err != nil {
		return nil, withhold(errLimitedUnencodable.Error(), err)
	}
	out, ok := limitedProject(tree, "", limitedKeep())
	obj, isObj := out.(map[string]any)
	if !ok || !isObj {
		return nil, withhold(errLimitedUnencodable.Error(), errors.New("the keep list did not give a JSON object"))
	}
	obj["limited_report"] = limitedReportNote
	b, err := json.Marshal(obj)
	if err != nil {
		return nil, withhold(errLimitedUnencodable.Error(), err)
	}
	return b, nil
}

// limitedLoadWithoutTexts is rep with its load records as they leave the environment (AC-F2.2): each check's
// load.errors as report.LoadErrorsByKind, each load step as report.LoadStepWithoutTexts — words, counts and times, no
// reason. A copy: rep itself (read from the run file) is not changed. The keep list then cuts the rest; the reason
// paths are in limitedNotSent too, so either barrier alone keeps the text in.
func limitedLoadWithoutTexts(rep *report.Report) *report.Report {
	out := *rep
	out.Layers = make([]report.Layer, len(rep.Layers))
	for i, l := range rep.Layers {
		out.Layers[i] = l
		out.Layers[i].Scenarios = make([]report.ScenarioResult, len(l.Scenarios))
		for j, sc := range l.Scenarios {
			if sc.Load != nil {
				ls := *sc.Load
				ls.Errors = report.LoadErrorsByKind(ls.Errors)
				sc.Load = &ls
			}
			if sc.LoadSteps != nil {
				sc.LoadSteps = stepsWithoutTexts(sc.LoadSteps)
			}
			out.Layers[i].Scenarios[j] = sc
		}
	}
	return &out
}

// limitedKeepSet is limitedSent as a set of leaves, plus every path on the way to a leaf (the containers the
// projection may enter).
type limitedKeepSet struct {
	leaf, enter map[string]bool
}

func limitedKeep() limitedKeepSet {
	k := limitedKeepSet{leaf: map[string]bool{}, enter: map[string]bool{"": true}}
	for _, p := range limitedSent {
		k.leaf[p] = true
		for i := 0; i < len(p); i++ {
			switch {
			case p[i] == '.':
				k.enter[p[:i]] = true
			case strings.HasPrefix(p[i:], "[]"):
				k.enter[p[:i]] = true // the array; its elements' path ("…[]") is entered by the '.' after it, or is the leaf itself
			}
		}
	}
	return k
}

// limitedProject copies v (one node of the report's JSON form, at path) keeping only listed leaves. A listed leaf
// is copied whole; a container is entered only on the way to a listed leaf; anything else is dropped. The bool is
// false when nothing at this path is to be sent.
func limitedProject(v any, path string, k limitedKeepSet) (any, bool) {
	if k.leaf[path] {
		return v, true
	}
	if !k.enter[path] {
		return nil, false
	}
	switch x := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for key, val := range x {
			if pv, ok := limitedProject(val, limitedJoin(path, key), k); ok {
				out[key] = pv
			}
		}
		return out, true
	case []any:
		out := make([]any, 0, len(x))
		for _, el := range x {
			if pv, ok := limitedProject(el, path+"[]", k); ok {
				out = append(out, pv)
			}
		}
		return out, true
	case nil:
		return nil, true // an empty container (a null slice) carries nothing
	default:
		return nil, false // a scalar where the keep list expects a container: not a field anyone listed
	}
}

func limitedJoin(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}
