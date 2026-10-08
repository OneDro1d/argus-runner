// Package report reads argus's native results/<id>/report.json and normalizes it
// into the Argus report contract (report.schema.yaml): the per-scenario string
// `failure` becomes a structured failure{expected, observed} (VR-A3/D2). `observed`
// comes from the argus run; `expected` is joined from the scenario's EXPECT and is
// supplied ONLY for the test hat (the runner passes nil expecteds for the product
// hat, so `expected` stays omitted — the role-gating happens at the call site).
package report

import (
	"encoding/json"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/OneDro1d/argus-runner/internal/compare"
	"github.com/OneDro1d/argus-runner/internal/envcapture"
)

// ModeCompare is the run mode of a comparison run (ARGUS-CMP-3): one member's run of a
// sealed set whose checks declare `## COMPARE`. It is certifying for custody (ModeIsCertifying), so a
// builder gets the verdict only, and it is the ONLY mode in which an output is recorded.
const ModeCompare = "compare"

// RecordedOutput is what a check handed back for ScenarioResult.Outputs: the digest record and the
// stored form (the body, in memory, until the executor writes it to its own disk). Runtime only: it
// rides StepResult.Output between the engine and the run loop and is never serialised, so no body can
// reach report.json through it.
type RecordedOutput struct {
	Record compare.OutputRecord
	Stored *compare.Stored
}

// executorFailureSignatures are substrings (lower-cased) that mark a run failure as an
// EXECUTOR/HARNESS failure (the rig is down) rather than a SUT failure — RO-04. They
// must be specific to infra so a genuine SUT/test signal is never swallowed as `error`.
var executorFailureSignatures = []string{
	"is not running",     // `service "jmeter" is not running`
	"jmeter exec failed", // DockerRunner could not launch JMeter
	"no such service",    // compose service missing
	"cannot connect to the docker daemon",
	"docker daemon is not running",
	"executor unavailable", // the preflight's own message
	"executor unreachable",
}

// IsExecutorFailure reports whether an observed/error string is an execution/harness
// failure (the rig is down) — to be classified `error`, never N SUT `failed` (RO-04).
func IsExecutorFailure(s string) bool {
	low := strings.ToLower(s)
	for _, sig := range executorFailureSignatures {
		if strings.Contains(low, sig) {
			return true
		}
	}
	return false
}

// Report is the normalized Argus report (matches schemas/report.schema.yaml).
type Report struct {
	Project   string `json:"project"`
	Timestamp string `json:"timestamp,omitempty"`
	RunID     string `json:"run_id,omitempty"` // DF-06/07: the run-level handle so get_report is run-scoped (not "the last report")
	Mode      string `json:"mode,omitempty"`
	// Note (R3-A): a human hint stamped onto the report itself (so the ASYNC get_report path surfaces it,
	// not just the synchronous run return) — currently used for the 0-match disk-vs-catalog explanation.
	Note string `json:"note,omitempty"`
	// ScenarioSource is WHERE this run's scenario set came from (VR-F6):
	//
	//   catalog           materialised from the control-plane catalog - the authority
	//   local-verified    on disk, and the catalog's set hash matched it byte for byte
	//   local-unverified  on disk, catalog UNREACHABLE, and a human explicitly chose to run anyway
	//   local             no catalog is wired at all (a standalone rig)
	//
	// On the REPORT rather than only in the tool response, because the tool response is read once
	// and the report is what anyone consults afterwards. A green report over an unverified set is
	// not wrong, but it is a DIFFERENT CLAIM from a green report over the catalog's set, and a
	// reader cannot tell them apart unless the report says which it is.
	ScenarioSource string  `json:"scenario_source,omitempty"`
	Summary        Summary `json:"summary"`
	Layers         []Layer `json:"layers"`
	// BuildRecordObjectID (AC-10) is the Memstore object id the runner got back after writing this
	// build-mode run's argus.build_record into the developer's own library through the customer's
	// hub — so runner__get_report can answer with it. Empty for a final/scheduled run (no build
	// record is ever written for one) and for a build run that had no hub/token configured.
	BuildRecordObjectID string `json:"build_record_object_id,omitempty"`
	// BuildRecordNote (AC-10) says WHY no build record was written for a build-mode run — the hub or
	// the runner's own token was absent — so the gap is a reported state, never a silent skip.
	BuildRecordNote string `json:"build_record_note,omitempty"`
	// Environment (P3 #23, tester msgbus 2026-09-27) is the SUT namespace's Kubernetes shape at
	// the moment of this run — pods, workloads, nodes, and a stable fingerprint — captured ONLY when
	// this run declared at least one `## LOAD` profile (internal/argus.captureEnvironmentIfNeeded).
	// nil on every non-load run, exactly the existing report.ScenarioResult.Load convention: a field
	// never attempted is omitted, not reported empty. A load run that DID attempt capture but could
	// not read anything still gets a non-nil Environment with Captured=false and a named Reason —
	// see envcapture.Capture's doc comment. The operator's principle this exists for: "we can't say
	// 'it breaks under load'; we must say 'this environment, with these resources, breaks under this
	// load'".
	Environment *envcapture.Capture `json:"environment,omitempty"`
}

type Summary struct {
	Total   int `json:"total"`
	Passed  int `json:"passed"`
	Failed  int `json:"failed"`
	Skipped int `json:"skipped"`
	// Errored counts execution/harness failures (status=="error", DEC-07/VR-L3) —
	// DISTINCT from a SUT "failed". omitempty so pre-existing reports are byte-unchanged.
	Errored int `json:"errored,omitempty"`
	// Degraded counts DEGRADED scenarios (AC-11, StatusDegraded): a load-mode scenario whose
	// assertions PASSED but the survival-plane read showed the SUT in distress during the run.
	// DISTINCT from both Passed and Failed — never counted in either, the same reasoning that
	// keeps RateLimitedScenarios out of them. omitempty: a run with no load profile is byte-unchanged.
	Degraded int `json:"degraded,omitempty"`
	// ── the rate limit (VR10-R1 / V28-009) ─────────────────────────────────────────────────
	// ALL omitempty: a run that never met a rate limit produces a byte-identical report, which is
	// the whole reason these ride the existing `errored` path instead of inventing a fourth status.
	//
	// RateLimitPauses is how many times THIS RUN stopped and waited for the SUT (owner D8: the run
	// pauses BETWEEN scenarios, never inside a scenario's own timeout clock).
	RateLimitPauses int `json:"rate_limit_pauses,omitempty"`
	// RateLimitedScenarios is how many scenarios ended NOT MEASURED because the SUT throttled them.
	// They are counted in Errored too — "rate-limited" is a REASON inside `errored` (owner D4), not
	// a peer of it — and never in Passed or Failed: a pass rate is over MEASURED scenarios only.
	RateLimitedScenarios int `json:"rate_limited_scenarios,omitempty"`
	// RateLimitCapHit says the run stopped pausing because a cap was reached (SA §0.14 R1-b).
	RateLimitCapHit bool `json:"rate_limit_cap_hit,omitempty"`
	// Note is the summary's own sentence to the operator — today, the cap message. Kept here rather
	// than on Report.Note, which already carries three unrelated run-level explanations.
	Note string `json:"note,omitempty"`
}

type Layer struct {
	Layer     string           `json:"layer"`
	Scenarios []ScenarioResult `json:"scenarios"`
}

type ScenarioResult struct {
	ID string `json:"id"`
	// passed | failed | skipped | error (execution/harness failure — DEC-07/VR-L3) | degraded
	// (AC-11 — a load-mode scenario that passed its own assertions while the SUT was in distress;
	// see StatusDegraded).
	Status        string   `json:"status"`
	DurationMs    int      `json:"duration_ms"`
	CorrelationID string   `json:"correlation_id,omitempty"`
	Failure       *Failure `json:"failure,omitempty"`
	// DEC-07 (forward-compatible, omitempty so existing single-step reports are unchanged):
	Steps []StepResult `json:"steps,omitempty"` // per-step status for chained scenarios (D3.6 / VR-K4/K5)
	// Cleanup (VR12-C2 / V29-015) — one entry per declared `## CLEANUP` block, IN ORDER.
	//
	// ⛔ IT NEVER TOUCHES THE VERDICT, in either direction: a green cleanup cannot rescue a failed
	// scenario and a red one cannot fail a passing one. It is here so that an operator — or the
	// test agent that fetches this report — can see whether the run cleaned up after itself, which
	// is the thing that was invisible for the whole life of the product.
	//
	// ⚠ THIS EXTENDS THE SPECIFIED REPORT SHAPE (specs/01-mcp-runner.md has no cleanup field).
	// Extending is established practice here (`steps`, `req_success`, `assertions_enforced` are all
	// extensions) — but it IS an extension, and the spec is updated with it in the same change.
	Cleanup []CleanupResult `json:"cleanup,omitempty"`
	// Residue (VR12-CH1 rule 7) — ONE line, on the terminal AND in report.json, when a scenario
	// ended with something it created possibly still on the SUT: a step was not fired, or a step
	// that runs after the break failed. ⛔ ABSENCE OF CLEANUP MUST NEVER BE SILENT.
	//
	// It says "may remain", never "remains": the runner knows which steps did not complete, not
	// what the SUT actually holds. Claiming more than that would be the same defect as the row —
	// a confident sentence the product cannot support.
	Residue     string          `json:"residue,omitempty"`
	MCPEnvelope json.RawMessage `json:"mcp_envelope,omitempty"` // the judged JSON-RPC envelope for mcp scenarios (VR-J10); test-hat / product-scanned
	// ReqSuccess/ReqFailed/ReqError count the REQUESTS the runner FIRED at the SUT to execute
	// this scenario, by the REQUEST'S RESPONSE outcome — NOT the scenario's pass/fail (r3):
	// success = a positive response; failed = a negative RESPONSE (HTTP 4xx/5xx; MCP isError:true
	// or a JSON-RPC protocol error) — a real SUT response, so a deliberate error-path/permissions
	// scenario fires one of these and still PASSES; error = the test couldn't get a response (HTTP
	// conn-fail/code 0; MCP unreachable/transport; chain/UI exec failure; dead rig). Per executor:
	// HTTP = one per JMeter .jtl SUT-trigger sample; MCP = one per tool call; chain = one per
	// EXECUTED step; UI = one per spec run. omitempty keeps a no-request scenario byte-clean.
	ReqSuccess int `json:"req_success,omitempty"`
	ReqFailed  int `json:"req_failed,omitempty"`
	ReqError   int `json:"req_error,omitempty"`
	// ── the rate limit (VR10-R1) ───────────────────────────────────────────────────────────
	// RateLimited says this scenario ended NOT MEASURED because the SUT refused it with the
	// throttle signature the SUT itself declared. It rides `Status: error` (grey), never `failed`.
	RateLimited bool `json:"rate_limited,omitempty"`
	// RateLimitedRetries is how many times the run re-fired this scenario after pausing (0 or 1 —
	// `retry_once`). Recorded even when the retry SUCCEEDED, because the row then carries the
	// retry's outcome (SA §0.14 R1-a) and a reader must be able to see that it was retried at all.
	RateLimitedRetries int `json:"rate_limited_retries,omitempty"`
	// PausedMs is the wall-clock the run spent waiting for this scenario's throttle.
	PausedMs int `json:"paused_ms,omitempty"`
	// RetryAfterMs is what the SUT asked us to wait. Runtime-only (json:"-"): it is the run loop's
	// input, and the same number is already spelled out in the observed line a human reads.
	RetryAfterMs int `json:"-"`
	// Requests is the per-request distribution source (r3, 1a′): one sample per request stamped
	// at its REAL fire-time, fed to the Loki request-event push so Panel A can plot the genuine
	// within-run distribution. Runtime-only (json:"-") — it is NOT part of the report contract
	// (it would bloat report.json with hundreds of samples), and the Loki push reads it from the
	// in-memory report at run time, before serialization.
	Requests []RequestSample `json:"-"`
	// ── VR12-E13 — TIER 3, THE RUN-REPORT BACKSTOP ─────────────────────────────────────────
	//
	// Unexecuted names every bullet in a scenario's `### Runnable` that REACHED THIS RUN and could
	// not be executed — quoted verbatim, with the reason. It is the PEER of the per-step
	// AssertionsEnforced: that field says what WAS enforced, this says what was NOT.
	//
	// The owner's three-tier model, and why this tier is not belt-and-braces:
	//   1. the authoring skill GUIDES  2. validation REFUSES  3. the run REPORTS
	// Validation is author-path only, so it bites when someone WRITES a scenario and never when one
	// runs. Every catalogue already written was written before these rules existed and keeps running
	// untouched — without tier 3 an unexecutable claim in one of them stays silently ignored for
	// ever, which is the exact defect this round exists to remove, surviving its own fix.
	//
	// ⛔ IT NEVER CHANGES THE VERDICT. A scenario is not failed for carrying one; it is reported.
	// ⛔ IT CARRIES THE TEST'S EXPECTED VALUE, so it is holdout material and redactExpected NILs it
	//    for the product hat exactly as it does Failure.Expected (VR12-E14, asserted by a test —
	//    a brand-new field is not covered by the existing redaction by accident).
	Unexecuted []Unexecuted `json:"unexecuted,omitempty"`

	// AssertionsEnforced / AssertionsEnforcedCount (V29-021) are the SCENARIO-level twin of the
	// per-step fields on StepResult: the declared CONTENT checks this scenario's engine actually
	// evaluated. The rule is the same for every engine, and it is the chain step's rule:
	//   - http/JMeter: the body checks and the database checks (a declared row count or "no rows",
	//     and each column) the running template was handed, recorded on a pass and on a `failed`
	//     whose failing plane was the content;
	//   - mcp: the body checks the judge evaluated, on a pass and on a body-plane miss;
	//   - ui: the spec's executed bullets, read back from its outcomes file.
	// A status line (`status=…`, `result.isError …`) is never counted, so a count of 0 means only
	// the status / no-error was checked — not that nothing ran.
	//
	// The text carries the scenario's expected value, so it is test-hat only (redactExpected strips
	// it for the product hat); the COUNT is what both hats keep.
	//
	// ⛔ THE COUNT HAS NO `omitempty`, for the reason V31-005 removed it from StepResult: a count of
	// 0 — "this scenario declared no checks" — would emit no key, which a consumer cannot tell from
	// an older executor that never wrote the field. Absence read as health is what this round removes.
	AssertionsEnforced      []string `json:"assertions_enforced,omitempty"`
	AssertionsEnforcedCount int      `json:"assertions_enforced_count"`

	// ObservedStatus is the HTTP status code the SUT ANSWERED on
	// the http/JMeter engine — read from the run's own result (the .jtl responseCode of the first
	// SUT-trigger sample), never from the scenario's expected value, on a pass AND on a fail. It exists
	// because the status line is enforced but, by design, not counted in AssertionsEnforcedCount, so a
	// passing row with a count of 0 gave a reader no sign that the status had been checked at all.
	//
	// It is REALITY, not expectation (VR-C8): the observed line of a failing row already names this
	// code, so both hats keep it. Absent (omitempty) when no HTTP status exists to report — the SUT
	// never answered (a transport error is code 0, which is not a status) — and on the engines that do
	// not read a .jtl (mcp, ui, chain steps carry their own per-step Observed).
	ObservedStatus int `json:"observed_status,omitempty"`
	// ObservedStatusCodes lists the DISTINCT SUT-trigger response codes, once each in first-seen .jtl
	// order and at most 16, ONLY when the samples answered with more than one (an idempotency pair: 202
	// then 409), where one number would hide the other. Never one entry per sample: a load run fires
	// thousands, and the .jtl keeps that record.
	ObservedStatusCodes []int `json:"observed_status_codes,omitempty"`

	// Load (AC-11) carries load-mode measurements — present only when the scenario declared a
	// `## LOAD` profile: the percentile/error-rate read from the JTL, the declared targets, whether
	// they were breached, and the survival-plane (Prometheus) verdict.
	Load *LoadStats `json:"load,omitempty"`
	// SandboxPolicy (spec 26, A1 — observe only) is what the SUT agent's OpenShell sandbox denied in
	// this scenario's padded window, read from Loki. Present only when argus-config declares
	// observability.openshell; nil keeps report.json and the evidence hash byte-identical for every
	// other config (TestScenarioResult_BytesUnchangedWithoutSandboxPolicy).
	//
	// ⛔ IT NEVER CHANGES THE VERDICT in this release: nothing reads it to set Status or Failure.
	// It is the SUT agent's own observed traffic — observed reality, not the test's expected value —
	// so redactExpected leaves it alone and the product hat reads it on an ordinary run (build, or the
	// builder's own local "ci" run). On a certification run it is per-scenario evidence like the rest,
	// and WithholdScenarios drops it with every row: the builder gets the verdict only (,
	// the #417 rule; toolcore TestGetReport_SandboxPolicyFollowsTheRunsCustody pins both halves).
	SandboxPolicy *SandboxPolicy `json:"sandbox_policy,omitempty"`
	// LoadWindowStartMs / LoadWindowEndMs are this scenario's own request fire-time window, unix ms
	// (AC-11) — runtime-only (json:"-"), like Requests: the run loop's input to the survival-plane
	// windowed-delta comparison, not part of the report contract.
	LoadWindowStartMs, LoadWindowEndMs int64 `json:"-"`

	// LoadDriver / LoadTarget / LoadSteps (, design §4.1) are an `AMQP Load` ramp's result:
	// "amqp", the NAME of the load target (never its URL), and one record per step. omitempty on all
	// three keeps every other scenario's bytes -- and therefore its evidence hash -- unchanged.
	LoadDriver string     `json:"load_driver,omitempty"`
	LoadTarget string     `json:"load_target,omitempty"`
	LoadSteps  []LoadStep `json:"load_steps,omitempty"`
	// LoadStoppedAtStep (HTTP Load) is the 1-based step at which the ramp stopped because that
	// step was not comfortable; 0 (absent) otherwise and on every AMQP row, whose bytes stay as they were.
	LoadStoppedAtStep int `json:"load_stopped_at_step,omitempty"`
	// Outputs (ARGUS-CMP-3) are the digests of the check's recorded outputs: one
	// record per sample (a chain http step, a JMeter sampler), never a body, a header value or a claim.
	// Set ONLY in mode `compare` and only for a check that declares `## COMPARE`; nil everywhere else,
	// so report.json and the evidence hash are byte-identical in every other mode
	// (TestScenarioResult_BytesUnchangedWithoutOutputs). redactExpected nils it by name for the product
	// hat and WithholdScenarios drops it with every row.
	Outputs []compare.OutputRecord `json:"outputs,omitempty"`
	// WindowStart / WindowEnd are the wall-clock bracket around this scenario's own execution (its
	// re-fire included, its CLEANUP excluded) — runtime-only, the run loop's input to the sandbox
	// evidence pass (spec 26). DurationMs cannot serve: for HTTP scenarios it is JMeter's elapsed time.
	WindowStart, WindowEnd time.Time `json:"-"`
}

// Coverage values of a sandbox_policy block (spec 26 §4). `unavailable` is never an empty "clean":
// it always carries a reason, and P2 will read it as not measured.
const (
	CoverageComplete    = "complete"
	CoverageLossy       = "lossy"
	CoverageUnavailable = "unavailable"
)

// SandboxPolicy is one scenario's sandbox_policy block (spec 26 §4, A1). Only Denied/Blocked events
// and findings are listed, with no payloads: the builder (obsquery.SandboxPolicyFor) decodes a fixed
// list of OCSF fields and nothing else. Credentials are removed by shape only (#448): a token-shaped
// path segment or run of a title or reason becomes "[redacted]", and a secret that does not look like
// one is carried as logged (obsquery/openshell.go header).
type SandboxPolicy struct {
	Source  string `json:"source"`  // "loki" in v1
	Sandbox string `json:"sandbox"` // the sandbox id (OCSF container.uid)
	// Window is the padded window the events were attributed to, RFC 3339 with ms, UTC. Absent on a
	// row that never ran (an executor preflight short-circuit).
	Window         *SandboxPolicyWindow `json:"window,omitempty"`
	Coverage       string               `json:"coverage"`        // complete | lossy | unavailable — never omitted
	CoverageReason string               `json:"coverage_reason"` // never omitted, never empty
	// DeniedCount counts every kept event, denials and findings alike, including any beyond
	// the listed cap. Set only on a `complete` or `lossy` block, where a 0 is a measured zero. On an
	// `unavailable` block it is nil and serialises as "denied_count":null (no omitempty, so the key is
	// always there): nothing was measured, and a number there would read as "no denials" (spec 26 §4,
	// §7).
	DeniedCount *int                 `json:"denied_count"`
	Events      []SandboxPolicyEvent `json:"events"` // never null: the builder sets []; always [] on `unavailable`
	// EventsOmitted is how many kept events are not listed because of the cap.
	EventsOmitted int `json:"events_omitted,omitempty"`
	// SharedWith names the other scenarios of this run whose padded window also holds one of these
	// events: no OCSF event carries a correlation id, so such an event cannot be pinned to
	// one scenario.
	SharedWith []string `json:"shared_with,omitempty"`
}

// SandboxPolicyWindow is the padded window, RFC 3339 with milliseconds, UTC.
type SandboxPolicyWindow struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// SandboxPolicyEvent is one Denied/Blocked event or finding, in the spec 26 §4 shape. Every field is
// a short string, at most 256 runes: no payload, header or query string ever reaches it, nor userinfo
// in an HTTP target, and token-shaped values in a path, a title or a reason are "[redacted]" (#448, a
// shape check; obsquery/openshell.go header says exactly what is carried).
type SandboxPolicyEvent struct {
	Time    string `json:"time"`    // the OCSF event time, RFC 3339 ms UTC
	Class   string `json:"class"`   // e.g. NET:OPEN, HTTP:POST, FINDING:CREATE
	Action  string `json:"action"`  // e.g. Denied (OCSF action, else disposition)
	Target  string `json:"target"`  // host:port, METHOD scheme://host:port/path (8 segments at most; METHOD host:port when the URL is dropped), or the finding title (128 runes)
	Process string `json:"process"` // actor.process.name
	Rule    string `json:"rule"`    // firewall_rule.name
	Reason  string `json:"reason"`  // status_detail, query strings and scheme://user:pw@ userinfo removed, token-shaped runs redacted
}

// StatusDegraded (AC-11) marks a load-mode scenario whose own assertions PASSED but the
// survival-plane read (internal/obsquery, the Prometheus half — Loki-first) showed the SUT's pod in
// distress (throttled or restarting) during the run window. Distinct from `failed` (the scenario's
// claims were wrong) and from `error` (nothing could be judged): DEGRADED means "it did what was
// asked, under duress". It NEVER supersedes a genuine failure or the existing throttled-`error`
// (429) case — both are decided first; DEGRADED only ever replaces what would otherwise be `passed`.
const StatusDegraded = "degraded"

// LoadStats are the load-mode (AC-11, mode 2) percentile + error-rate figures computed from one
// scenario's JMeter .jtl SUT-trigger samples, plus the breach/survival verdict against the
// scenario's declared LOAD targets.
type LoadStats struct {
	Samples   int     `json:"samples"`
	P50Ms     int     `json:"p50_ms"`
	P95Ms     int     `json:"p95_ms"`
	P99Ms     int     `json:"p99_ms"`
	ErrorRate float64 `json:"error_rate"`
	// TargetP95Ms / MaxErrorRate echo the scenario's declared LOAD targets, so a reader of the
	// report alone (without the scenario file) can see what was being judged against.
	TargetP95Ms  int     `json:"target_p95_ms,omitempty"`
	MaxErrorRate float64 `json:"max_error_rate,omitempty"`
	// Breached is true when the MEASURED p95/error-rate exceeded the declared target — a load SLA
	// failure, reported as the scenario's `failed` status (never DEGRADED: a breach is the
	// scenario's OWN claim being wrong, not a distress signal from the SUT's infrastructure).
	Breached bool `json:"breached,omitempty"`
	// SurvivalChecked / Degraded / DegradedNote carry the Prometheus survival-plane verdict — only
	// set when a reader (obsquery.SurvivalPlaneRead) was actually consulted for this scenario.
	SurvivalChecked bool   `json:"survival_checked,omitempty"`
	Degraded        bool   `json:"degraded,omitempty"`
	DegradedNote    string `json:"degraded_note,omitempty"`
	// Errors is the bounded breakdown of TRANSPORT failures (samples with no numeric status), most
	// frequent first; absent when there were none. Timeline is the per-bucket view of the run
	// (TimelineBucketS seconds wide), absent below two samples. Both are written by the executor
	// from the .jtl and read by nobody's verdict: pass/fail, percentiles and error_rate ignore them.
	Errors          []LoadError  `json:"errors,omitempty"`
	TimelineBucketS int          `json:"timeline_bucket_s,omitempty"`
	Timeline        []LoadBucket `json:"timeline,omitempty"`
	// GeneratorCPUThrottledShare / GeneratorNotMeasured (, #615): the executor's own cgroup CPU
	// throttling during this run, as on LoadStep. RECORD ONLY here: the plain check's verdict is unchanged.
	GeneratorCPUThrottledShare *float64 `json:"generator_cpu_throttled_share,omitempty"`
	GeneratorNotMeasured       bool     `json:"generator_not_measured,omitempty"`
}

// Breaches reports whether the measured stats violate the scenario's declared load targets.
// TargetP95Ms/MaxErrorRate of 0 mean "not declared" and never breach (VR-A10 already refuses a
// declared-but-zero value at authoring time — see scenario.loadFieldBounds).
func (s LoadStats) Breaches(targetP95Ms int, maxErrorRate float64) bool {
	if targetP95Ms > 0 && s.P95Ms > targetP95Ms {
		return true
	}
	if maxErrorRate > 0 && s.ErrorRate > maxErrorRate {
		return true
	}
	return false
}

// Unexecuted is one runnable EXPECT bullet that reached a run and was not executed (VR12-E13).
type Unexecuted struct {
	// Bullet is the author's text, VERBATIM. Paraphrasing it would defeat the purpose: the author
	// has to be able to find the line in their own file.
	Bullet string `json:"bullet"`
	// Reason says WHY it could not be executed, in the product's own words — the parser could not
	// classify it, the layer has no mechanism for it, or the template does not read the property.
	Reason string `json:"reason"`
}

// Request-response outcome classes (r3). The SINGLE vocabulary every executor maps to and the
// only values the Loki request-event `outcome` label carries.
const (
	OutcomeSuccess = "success"
	OutcomeFailed  = "failed"
	OutcomeError   = "error"
)

// RequestSample is one request the runner fired at the SUT, stamped at its real fire-time.
// Runtime-only — see ScenarioResult.Requests.
type RequestSample struct {
	AtMs    int64  // unix epoch millis at fire-time (JMeter .jtl timeStamp for HTTP; wall-clock for MCP/chain/UI)
	Outcome string // OutcomeSuccess | OutcomeFailed | OutcomeError
}

// AddRequest records one fired request: it appends the timestamped sample AND increments the
// matching 3-way count, so the counts can never drift from the samples (single source of truth).
func (r *ScenarioResult) AddRequest(atMs int64, outcome string) {
	r.Requests = append(r.Requests, RequestSample{AtMs: atMs, Outcome: outcome})
	switch outcome {
	case OutcomeSuccess:
		r.ReqSuccess++
	case OutcomeFailed:
		r.ReqFailed++
	case OutcomeError:
		r.ReqError++
	}
}

// ── VR12-CH1 / VR12-CH2 (V30-001) — THE CHAIN STEP VOCABULARY ────────────────────────────────
//
// ⛔ `skipped` IS RETIRED FROM THE CHAIN PATH. It named a situation that no longer exists: the
// executor used to STOP at the first non-passing step and synthesise every later step as
// `skipped`, "not run (a prior step failed)". It now CONTINUES, so the steps that undo what the
// test created actually fire — measured on the live RACE pack, the only two runs of six that left
// a namespace behind were the two whose chain broke, and their `delete-namespace` never ran.
//
// A third option — keep the name `skipped`, cheapest, changes nothing downstream — was rejected:
// *skipped* reads as a choice we made, and the truth is we COULD NOT.
const (
	// StepNotMeasured is a step that was NOT FIRED, for a reason the step's `observed` gives:
	// its input was never captured (its `${saved.…}` producer did not pass), or we stopped
	// because the SUT would not answer. ONE name, because to a reader both mean "this step was
	// not tested" — and a why belongs in a sentence, not in a status code. Every status name is
	// something four surfaces have to learn (report.json, the Grafana panels, the terminal
	// renderer, and whoever triages the run), and "errored vs failed" is already a documented
	// source of confusion.
	StepNotMeasured = "not-measured"
	// StepRanAfterFailureOK / StepRanAfterFailureFailed — ⛔ NO STEP THAT RUNS AFTER THE FIRST
	// FAILURE MAY EVER BE RECORDED AS `passed`.
	//
	// Not cosmetic. Once a step has failed the SUT is in an unknown state, so a later step's
	// POSITIVE claim is not trustworthy — and 44 example steps assert an ABSENCE
	// (`result.isError == true`), which passes VACUOUSLY in exactly that situation. RACE-001's
	// `verify-gone` expects an error because the namespace should have been deleted; if
	// `create-namespace` had failed, the namespace never existed, the call errors, and the step
	// would report PASSED for a deletion that never happened. A false green is worse than the
	// leak, because the leak is visible.
	StepRanAfterFailureOK     = "ran-after-failure: ok"
	StepRanAfterFailureFailed = "ran-after-failure: failed"
)

// ResidueLines is VR12-CH1 rule 7 on the TERMINAL: one line per scenario that may have left
// something on the SUT. The same text is in report.json as ScenarioResult.Residue — a reader who
// only sees the terminal must not have to open the report to learn that cleanup did not happen.
//
// ⛔ ABSENCE OF CLEANUP MUST NEVER BE SILENT. That is the standing "absence is not health" rule
// applied to the one signal a run produces by NOT doing something.
func (r *Report) ResidueLines(runID string) []string {
	var out []string
	for _, l := range r.Layers {
		for _, s := range l.Scenarios {
			if s.Residue != "" {
				out = append(out, "run "+runID+": "+s.ID+" "+s.Residue)
			}
		}
	}
	return out
}

// CleanupResult is the outcome of ONE `## CLEANUP` block (VR12-C2).
//
// ⛔ NO CAPTURED OUTPUT. A bash cleanup's stdout would land in this struct, in report.json, which
// agents fetch and paste into chat — and there is no Go-side secret scrubber. The row offers two
// answers, scrub by VALUE or do not capture at all; this build takes the second, because a
// key-name filter is not redaction and this project has leaked live tokens twice by printing a
// structure and filtering top-level keys. `Observed` describes the SHAPE of what happened, never
// what the command said.
type CleanupResult struct {
	Form       string `json:"form"`    // sql | bash | na
	Outcome    string `json:"outcome"` // ok | failed | timeout | not-run
	DurationMs int    `json:"duration_ms,omitempty"`
	Observed   string `json:"observed,omitempty"`
}

const (
	// CleanupOK — it ran to completion. ⛔ A cleanup that found NOTHING TO REMOVE is ok: "a runnable
	// command in CLEANUP has nothing to delete. It is normal." One that went red for this reason
	// would turn every green run into a red one.
	CleanupOK = "ok"
	// CleanupFailed — it ran and did not complete.
	CleanupFailed = "failed"
	// CleanupTimeout — it exceeded its own budget. Recorded as a FAILED cleanup, never as a pass
	// and never as silence.
	CleanupTimeout = "timeout"
	// CleanupNotRun — nothing was executed: the scenario declared `N/A` (and the justification is
	// the record), or this executor cannot run that form. Either way it is SAID, not skipped.
	CleanupNotRun = "not-run"
)

// StatusError is the execution/harness-failure status — DISTINCT from a SUT "failed"
// (UC-62 / VR-L3) and from FLAKE. Normalize never emits it; every executor promotes to it via
// NeverReachedSUT below, and the UI executor also emits it directly for a dead harness.
const StatusError = "error"

// NeverReachedSUT reports that the runner fired requests for this scenario and NOT ONE of them got
// a response: every fired request was a transport error (connection refused, name not resolved,
// code 0, MCP unreachable). Nothing was tested, so there is nothing to have failed.
//
// THE ONE DEFINITION. HTTP, MCP and chain scenarios each decide pass/fail differently, and each
// used to be able to say only passed or failed — so an absent SUT arrived as a wall of red that
// reads like N product defects. They now all promote through THIS predicate rather than three
// hand-written copies, for the same reason the token tenancy rule is a shared constant: copies of
// a classification rule drift, and the drift is invisible until it matters.
//
// DELIBERATELY CONSERVATIVE, and the conservatism is the contract: it demands that NOTHING got
// through. One real response — even a 500 — means the SUT was reachable and the scenario genuinely
// failed, so a PARTIAL outage stays failed rather than being excused as infrastructure. A scenario
// that fired no requests at all is not an error either: there is no evidence of unreachability,
// and inventing some would be the same mistake in the other direction.
func (r *ScenarioResult) NeverReachedSUT() bool {
	return r.ReqError > 0 && r.ReqSuccess == 0 && r.ReqFailed == 0
}

// StepResult is one step of a chained (multi-step) scenario (D3.6). The shared
// correlation_id is preserved across ALL steps so a mid-chain failure is traceable
// end-to-end (VR-K5); a later step is never reported green when an earlier one failed.
type StepResult struct {
	Name          string `json:"name"`
	Status        string `json:"status"` // passed | failed | error
	Observed      string `json:"observed"`
	CorrelationID string `json:"correlation_id,omitempty"`
	// MCPEnvelope (PROB-1, 2026-07-22) is the SUT's own JSON-RPC envelope for a NON-PASSING mcp
	// step — the failing tool's real error text. Without it a chain-step failure reported only
	// "responder returned result.isError:true", which cannot distinguish "object not found" (the
	// fixture is gone → TEST_BUG) from "visibility check failed" (a tenancy/RLS regression against
	// pre-existing rows → CODE_BUG). A whole triage round had to leave 7 of 8 verdicts carrying an
	// un-excluded CODE_BUG alternative for exactly this reason.
	// Attached ONLY on failure/error: report.json already reaches ~120KB for a 45-scenario suite,
	// and a successful response is a transcript, not evidence. Same holdout status as the
	// scenario-level MCPEnvelope — this is the SUT's RESPONSE, never the scenario's EXPECT (which
	// redactExpected strips for the product hat), so it is test-hat / product-scanned.
	MCPEnvelope json.RawMessage `json:"mcp_envelope,omitempty"`
	// AssertionsEnforced (VR10-S2 / V28-014) lists the CONTENT assertions this step evaluated — the
	// ENFORCED marker. A green step that carried none proved only that the call did not error;
	// since V31-005 an EXPECTED-ERROR step records them too — a green error step that carried none
	// proved only its error plane, which is exactly as much as it should be credited with;
	// measured 2026-09-04: 2 of 21 green RACE steps proved their own claim. The text carries the
	// scenario's expected value, so it is test-hat only (redactExpected strips it for the product
	// hat); AssertionsEnforcedCount is what both hats keep.
	//
	// ⛔ THE COUNT HAS NO `omitempty`, AND THAT IS THE POINT (V31-005). With it, a count of 0 —
	// "this step declared no checks" — emitted NO KEY, which a consumer cannot tell apart from an
	// older executor that never wrote the field at all. Absence read as health is the pattern this
	// whole round exists to remove, so the zero is written. The TEXT keeps `omitempty`: it is
	// genuinely absent when there is none, and it is test-hat material.
	AssertionsEnforced      []string `json:"assertions_enforced,omitempty"`
	AssertionsEnforcedCount int      `json:"assertions_enforced_count"`
	// FailedClaims names, for a step that failed on its CLAIMS,
	// each claim that did not hold — AS WRITTEN (`${saved.<var>}` left unbound) — with the value the
	// SUT showed for that claim's field on the LAST attempt of the step (a polled step: the last poll).
	// A claim that held is not listed. EVERY claim that did not hold is listed, in the order the claims
	// were written, up to the backstop MaxFailedClaims; each observed value is cut to
	// MaxFailedClaimObserved bytes (see FailedClaim). Past the backstop the rest are COUNTED in
	// FailedClaimsOmitted, never silently dropped.
	//
	// ⛔ AUTHOR-ONLY, AND IT CARRIES THE SUT'S OBSERVED VALUES: redactExpected NILs it for the product
	// hat (internal/toolcore), it is not among the fields runner.RelayedReport copies, and it never
	// enters the federation push. A saved value is never printed here either: the claim is the written
	// text, and an observed value equal to a saved value is shown as its placeholder.
	FailedClaims []FailedClaim `json:"failed_claims,omitempty"`
	// FailedClaimsOmitted is how many claims did NOT hold on the same attempt FailedClaims came from
	// but are not listed there because the list reached MaxFailedClaims (the events_omitted
	// precedent). Absent (0) means the list is complete.
	//
	// ⛔ AUTHOR-ONLY, LIKE FailedClaims: redactExpected zeroes it BY NAME for the product hat, it is not
	// among the fields runner.RelayedReport copies, and it never enters the federation push. It is NEVER
	// written into Observed or any product-hat text — how many hidden claims failed is holdout material.
	FailedClaimsOmitted int `json:"failed_claims_omitted,omitempty"`
	// RanAfterFailure marks a step that executed AFTER the chain's first failure (VR12-CH1 rule 4).
	// Its status carries the same fact, but a machine consumer should not have to parse a string.
	RanAfterFailure bool `json:"ran_after_failure,omitempty"`
	// RateLimited / RetryAfterMs (VR10-R1) mark a step the SUT REFUSED because we were going too
	// fast. The chain rollup promotes the whole scenario on it: a chain is retried as a WHOLE
	// (SA §0.14 R1-d) — earlier steps may have had effects, so replaying only the tail would run
	// against state the head already changed.
	RateLimited  bool `json:"rate_limited,omitempty"`
	RetryAfterMs int  `json:"-"`
	// Captured holds what this step SAVED for later steps (chain capture, chain/capture.go):
	// variable name -> value pulled out of this step's response. Runtime-only (json:"-") like
	// Requests — it is plumbing between steps, not part of the report contract, and a captured
	// value can be a server-generated id the report has no reason to republish.
	Captured map[string]string `json:"-"`
	// Output is the recorded output of an http step in mode `compare` (nil otherwise). Runtime-only
	// (json:"-") like Captured: the run loop turns it into ScenarioResult.Outputs and the stored file.
	Output *RecordedOutput `json:"-"`
}

// MaxFailedClaims is the BACKSTOP on StepResult.FailedClaims, not a working limit: a real step lists
// every claim that did not hold (a cap of 10 cut an 11th red silently while the
// step's observed text called the list the full record). Claims past it are counted in
// StepResult.FailedClaimsOmitted. Each entry stays bounded by MaxFailedClaimObserved.
const MaxFailedClaims = 100

// MaxFailedClaimObserved is the longest observed value (in bytes, before FailedClaimTruncatedSuffix)
// one FailedClaim carries. An unscoped claim (`body contains …`) is judged against the whole response,
// and an unbounded echo of a response is its own leak.
const MaxFailedClaimObserved = 200

// FailedClaimTruncatedSuffix ends an observed value that was cut to MaxFailedClaimObserved bytes.
const FailedClaimTruncatedSuffix = "... (truncated)"

// FailedClaim is one claim of a failed chain step that did not hold: Claim as written (the same text
// as assertions_enforced, with `${saved.<var>}` unbound), Observed what the SUT showed for that
// claim's field on the last attempt (the whole answer for a claim not scoped to a field).
type FailedClaim struct {
	Claim    string `json:"claim"`
	Observed string `json:"observed"`
}

// TruncateObserved cuts s to MaxFailedClaimObserved bytes on a UTF-8 boundary and marks the cut.
func TruncateObserved(s string) string {
	if len(s) <= MaxFailedClaimObserved {
		return s
	}
	cut := MaxFailedClaimObserved
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + FailedClaimTruncatedSuffix
}

// Failure is the locked structured failure. Expected is a pointer so the product
// hat (nil) omits the key entirely; Observed is always present on a failure.
type Failure struct {
	Expected *string `json:"expected,omitempty"`
	Observed string  `json:"observed"`
	// FailedBodyCheck names WHICH `body …` bullet of an
	// http/JMeter scenario did not hold, when the status matched and the body did not. Observed stays
	// the fixed reality-only sentence for both hats; this field is the test hat's pointer to the bullet.
	//
	// ⛔ TEST-HAT ONLY, LIKE Expected: the bullet is the author's own words and carries the expected
	// value. toolcore.redactExpected NILs it for the product hat; runner.reduceReport and the
	// federation push copy named fields only and have no slot for it.
	FailedBodyCheck *FailedBodyCheck `json:"failed_body_check,omitempty"`
}

// FailedBodyCheck is the body check that failed: Index is its 1-based position among the scenario's
// runnable body checks (the N of the template's `expect.body.N` properties), Bullet the bullet as
// written under `### Runnable`. Built from a structured index the template emits, never by parsing
// the template's message for a field name or value (VR-C8).
type FailedBodyCheck struct {
	Index  int    `json:"index"`
	Bullet string `json:"bullet"`
}

// --- argus's native shape (failure is a bare string) ---
type rawReport struct {
	Project   string     `json:"project"`
	Timestamp string     `json:"timestamp"`
	Mode      string     `json:"mode"`
	Summary   Summary    `json:"summary"`
	Layers    []rawLayer `json:"layers"`
}
type rawLayer struct {
	Layer     string        `json:"layer"`
	Scenarios []rawScenario `json:"scenarios"`
}
type rawScenario struct {
	ID         string `json:"id"`
	Status     string `json:"status"`
	DurationMs int    `json:"duration_ms"`
	Failure    string `json:"failure"`
}

// Normalize parses argus's raw report.json and produces the Argus report.
// expecteds[id] = the test's expected (joined EXPECT) — pass nil/empty for the
// product hat to omit `expected`. corr[id] = the scenario's correlation_id, if known.
func Normalize(raw []byte, expecteds, corr map[string]string) (*Report, error) {
	var rr rawReport
	if err := json.Unmarshal(raw, &rr); err != nil {
		return nil, err
	}
	out := &Report{Project: rr.Project, Timestamp: rr.Timestamp, Mode: rr.Mode, Summary: rr.Summary}
	for _, rl := range rr.Layers {
		l := Layer{Layer: rl.Layer}
		for _, rs := range rl.Scenarios {
			res := ScenarioResult{ID: rs.ID, Status: rs.Status, DurationMs: rs.DurationMs}
			if corr != nil {
				if c, ok := corr[rs.ID]; ok {
					res.CorrelationID = c
				}
			}
			if rs.Status == "failed" {
				f := &Failure{Observed: rs.Failure}
				if expecteds != nil {
					if e, ok := expecteds[rs.ID]; ok && e != "" {
						ev := e
						f.Expected = &ev
					}
				}
				res.Failure = f
			}
			l.Scenarios = append(l.Scenarios, res)
		}
		out.Layers = append(out.Layers, l)
	}
	return out, nil
}

// Find returns the scenario result with the given id (and its layer), or nil.
func (r *Report) Find(id string) (*ScenarioResult, string) {
	for i := range r.Layers {
		for j := range r.Layers[i].Scenarios {
			if r.Layers[i].Scenarios[j].ID == id {
				return &r.Layers[i].Scenarios[j], r.Layers[i].Layer
			}
		}
	}
	return nil, ""
}

// Failed reports whether the run did NOT fully pass — any SUT failure OR any
// execution/harness error (status=="error"). An exec-error must never be silent-green
// (UC-62/VR-L3): it is DISTINCT in the per-scenario status + Summary.Errored, but it
// still makes the overall run non-green (non-zero exit), mirroring argus run.sh semantics.
func (r *Report) Failed() bool { return r.Summary.Failed > 0 || r.Summary.Errored > 0 }

// ScenariosVisibleToBuilder says whether a product-hat reader (a builder holding a runner token, or the
// in-env MCP tool) may be handed this report's per-scenario fields: only for a run of the BUILD set
// ("build") or the builder's own local run ("ci", the stamp argus.RunAll gives a run no assignment
// moded). Every other mode ran the certification set (final, scheduled, rehearsal) and an unknown or
// empty one is not proven to have run anything else, so it FAILS CLOSED.
func (r *Report) ScenariosVisibleToBuilder() bool { return r.Mode == "build" || r.Mode == "ci" }

// ModeIsCertifying is the same rule as ScenariosVisibleToBuilder for a bare mode string, with ONE
// deliberate difference: "" is NOT certifying here. An empty mode is a local/direct run that has no
// assignment (the executor's own log, a developer's terminal), whereas a Report with an empty Mode
// fails closed. Every non-empty mode other than build and ci is certifying, so an unknown one fails closed
// too. Keep the two in step: the list of visible modes is "build" and "ci" in both.
func ModeIsCertifying(mode string) bool { return mode != "" && mode != "build" && mode != "ci" }

// WithholdScenarios drops everything per-scenario from the report in place — scenario ids, correlation
// ids, outcomes, observed values, the environment capture, the build-record object id — and keeps the
// verdict: project, run id, mode, timestamp and the summary tallies.
func (r *Report) WithholdScenarios() {
	r.Layers = []Layer{}
	r.Environment = nil
	r.BuildRecordObjectID = ""
	r.BuildRecordNote = ""
	r.Note = "this run is not a build run: its per-scenario results belong to the author (author_get_report); the verdict and tallies above are all a builder is given"
}
