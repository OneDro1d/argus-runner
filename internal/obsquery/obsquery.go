// Package obsquery is the runner-core's observability-query layer: Loki/LogQL for
// get-sagas + tail-logs (GoKit-native sagas + structured logs, by correlation_id)
// and the Grafana dashboard-url template. All queries are best-effort: if Loki is
// unreachable the result reports available:false (the failure-context bundle stays
// usable — best-effort-maximal, VR-C6). M2 sagas are stdout JSON tagged event_type=saga.
package obsquery

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/OneDro1d/argus-runner/internal/failcontext"
)

// ObsInfraExclusion is the LogQL `service!~` list that drops the suite's own
// observability infra. Without it a correlation-id query self-matches Loki's own
// query-engine logs (which echo the correlation_id back). The CLI (queryLines) and
// the spec-18a dashboard's two log panels MUST use the SAME exclusion — the
// dashboard JSON is asserted against this constant in dashboard_test.go.
// (FINDING-4, 2026-06-18.)
const ObsInfraExclusion = "loki|promtail|grafana|prometheus|pushgateway|jmeter"

// DashboardURL templates the spec-18a Grafana deep link (VR-A6). Patch #4: it also sets
// var-project so the dashboard's Loki panels (saga/logs) scope to THIS SUT and a prior SUT's
// logs cannot bleed through (project is the canonical SUT identity, matching the metric +
// log project labels). E1 (R12): it sets var-current_run (the dashboard's "Run ID" variable,
// which has NO default — without it the run-scoped panels render empty) so the deep-link actually
// focuses the run. correlationID/runID are each optional (empty = the general dashboard for that axis).
func DashboardURL(grafanaBase, instance, project, correlationID, runID string) string {
	// R5-1: point a REGISTERED instance at ITS OWN dashboard (argus-overview-<id>), which onboarding
	// provisions in the shared Grafana wired to THAT instance's own Loki datasource (loki-<id> ->
	// argus-inst-<id>-loki-1). The generic `argus-overview` resolves datasource uid `loki` ->
	// http://loki:3100, and EVERY per-instance Loki joins argus-obs-net under that same alias, so
	// docker DNS round-robins and its saga/logs panels show a random instance's logs — or none. The
	// standalone/direct path (instance "local" or empty) has no per-instance copy and keeps the generic.
	dash := "argus-overview"
	if instance != "" && instance != "local" {
		dash += "-" + instance
	}
	// M3-FX (VR-E8/VR-F9): NO BASE MEANS NO LINK. Without this guard an empty base produced
	// "/d/argus-overview-<inst>?…" — a relative path that reads like a URL in a report and opens
	// nothing. Since the per-tier Grafana base can now legitimately fail to resolve, an absent base
	// must surface as ABSENCE rather than as a link-shaped string; the caller says why.
	if strings.TrimSpace(grafanaBase) == "" {
		return ""
	}
	u := strings.TrimRight(grafanaBase, "/") + "/d/" + url.PathEscape(dash) + "?var-argus_instance=" + url.QueryEscape(instance)
	if project != "" {
		u += "&var-project=" + url.QueryEscape(project)
	}
	if runID != "" {
		u += "&var-current_run=" + url.QueryEscape(runID)
		// The dashboard's own default is now-1h. Setting var-current_run alone therefore focuses
		// the run but leaves the WINDOW wrong for any run older than an hour: the Loki-backed Saga
		// and Logs panels query an empty range and render blank, while the Prometheus panels (whose
		// series carry the run_id label and are re-scraped from the pushgateway) keep showing data.
		// The result reads as "this run produced no sagas and no logs" — indistinguishable from a
		// SUT that emitted none. Pin the range to the run instead. (2026-07-23, owner-reported
		// against orderservice-compose run 20260723T093859438, for which Loki in fact held 464
		// lines and 113 saga lines.)
		u += "&from=" + defaultWindowFrom + "&to=now"
	}
	if correlationID != "" {
		u += "&var-correlation_id=" + url.QueryEscape(correlationID)
	}
	return u
}

// defaultWindowFrom is the deep link's LEFT edge, expressed in Grafana's relative syntax so the
// picker reads "Last 1 hour" — the same wording as the dashboard's own default.
//
// This deliberately replaced an ABSOLUTE left edge derived from the run handle. That version was
// correct for old runs but displayed as "2026-07-23 17:25:00 to a few seconds ago", which reads as a
// strange arbitrary period rather than a window anyone recognises; owner call 2026-07-23 was to use
// the familiar relative range.
//
// The trade-off is recorded rather than hidden: a run OLDER than this window will render empty
// Loki-backed panels (saga, logs, request distribution), because those are RANGE queries and the
// run's lines fall outside it. The Prometheus-backed panels are unaffected — they are INSTANT
// queries at `to`, and the per-instance pushgateway keeps re-serving the last push, so a run's
// pass/fail counts stay readable at `now` indefinitely. Widen this constant if that starts to bite.
//
// `to=now` is load-bearing and must not become an absolute stamp again. An earlier cut set it to
// runStart+2h, i.e. a right edge in the FUTURE, and every instant-query panel then evaluated two
// hours past the last sample — far outside Prometheus's 5-minute staleness horizon — so they all
// rendered "No data" while the Loki panel beside them was fine.
const defaultWindowFrom = "now-1h"

// Loki queries a Loki instance over HTTP. The *Field knobs (CHANGE-2) declare how THIS
// SUT names its correlation id + tags a saga line; empty means the canonical default. The
// LogQL line filter matches by VALUE, so the QUERY is field-name-agnostic; only the Go-side
// extraction (which field to read out of the parsed JSON) needs the declared name.
type Loki struct {
	BaseURL          string
	CorrelationField string // default correlation_id
	SagaEventField   string // default event_type
	SagaEventValue   string // default saga
	// SagaEventValues (PROB-2) is the SET of marker values when the SUT uses more than one
	// (Social: tool_dispatch / ayrshare_dispatch / ghost_dispatch). Empty falls back to the single
	// SagaEventValue, so the the operator one-value convention is untouched.
	SagaEventValues []string
	// SagaStepFields (GAP-1, 2026-07-22) maps a LOGICAL saga step field —
	// saga_id / step / step_name / step_status / timestamp / error / service — to the name THIS
	// SUT uses. Empty map = the the operator defaults plus a fallback chain (see stepFieldFallbacks).
	// Before this existed the step field names were hardcoded, so a SUT that marked its saga
	// lines correctly but named the steps differently (Social: msg/time/level) produced a
	// timeline of content-free nodes that still reported available:true. See sagafields.go.
	SagaStepFields map[string]string
	// LogFormat (#422) is the SUT's DECLARED log encoding (observability.loki.log_format): "logfmt"
	// or anything else/"" = json (the default). It governs how the reader parses a line: json →
	// JSON lines only; logfmt → JSON first, then logfmt. The same rule as the saga-presence
	// verdict template (templates/saga-presence.jmx), so reader and verdict agree.
	LogFormat string
	// Credential (T3.1, E3 export hosted-Loki target) is the RESOLVED "username:password" Basic-
	// Auth pair for a hosted Loki that requires it (config.LokiCredential — same shape and same
	// resolution mechanism as obsquery.BetterStack.Credential). "" (the bundled/adopt case, and
	// every pre-T3.1 config) sends NO Authorization header at all — queryLines must not guess one.
	Credential string
	// PushURL is the hosted Loki's full PUSH endpoint (observability.loki.push_url),
	// used VERBATIM by PushRequestEvents. "" pushes to BaseURL + /loki/api/v1/push, as before.
	PushURL string
	// Tenant (T3.3, E3 shared ingest) is the Loki tenant this instance reads as — the instance id,
	// sent as X-Scope-OrgID on every request (see setTenant). A shared Loki runs auth_enabled: true
	// and refuses a request without one. "" (bundled, adopt, export) sends NO X-Scope-OrgID at all.
	Tenant string
}

// TenantHeader is the Loki multi-tenancy header. It SCOPES a request; it does not authenticate
// one — anything that can reach a shared Loki can claim any tenant. The boundary is the shared
// Loki's NetworkPolicy (k8srender.RenderSharedLoki), not this header.
const TenantHeader = "X-Scope-OrgID"

// setTenant adds X-Scope-OrgID when a tenant is configured and leaves the header ABSENT otherwise:
// never present-and-empty, which a proxy or a Loki may treat differently from "absent". Every
// request this package makes to Loki goes through here — queryLines and PushRequestEvents.
func setTenant(req *http.Request, tenant string) {
	if t := strings.TrimSpace(tenant); t != "" {
		req.Header.Set(TenantHeader, t)
	}
}

func (l *Loki) corrField() string {
	if l.CorrelationField != "" {
		return l.CorrelationField
	}
	return "correlation_id"
}
func (l *Loki) sagaField() string {
	if l.SagaEventField != "" {
		return l.SagaEventField
	}
	return "event_type"
}
func (l *Loki) sagaValue() string {
	if l.SagaEventValue != "" {
		return l.SagaEventValue
	}
	return "saga"
}

// sagaValues is the SET of marker values that mark a saga line for THIS SUT (PROB-2). the operator's
// convention is one value (event_type=saga) with step_name varying, so a scalar sufficed — but a
// SUT may instead encode step identity in the marker field itself, as Social does across
// tool_dispatch / ayrshare_dispatch / ghost_dispatch. With a single scalar its saga could only
// ever contain gateway lines, and every worker-side control action (retry, DLQ, breaker trip) was
// invisible BY CONSTRUCTION. It stays a whitelist: an undeclared value must never match, or
// ordinary request logs would masquerade as saga steps.
func (l *Loki) sagaValues() []string {
	if len(l.SagaEventValues) > 0 {
		return l.SagaEventValues
	}
	return []string{l.sagaValue()}
}

// isSagaMarker reports whether v is one of this SUT's declared marker values.
func (l *Loki) isSagaMarker(v string) bool {
	for _, want := range l.sagaValues() {
		if v == want {
			return true
		}
	}
	return false
}

// normalizeStatus maps a SUT's step-status vocabulary onto the canonical ok/failed used to DERIVE
// failed_step and last_known_good; "" when it is neither (never guess). The raw value is always
// preserved in the timeline — this reading is only for the derived fields. Without it those two
// headline diagnostics were dead for any SUT not emitting the literals "ok"/"failed": Social's
// step_status maps to its `level`, so its steps read INFO/WARN/ERROR and no comparison ever fired.
func normalizeStatus(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "ok", "success", "succeeded", "successful", "passed", "pass", "info", "debug", "trace":
		return "ok"
	case "failed", "failure", "fail", "error", "err", "fatal", "panic", "critical":
		return "failed"
	}
	return ""
}

// sortTimeline puts the steps in chronological order (PROB-2c). Loki returns entries grouped BY
// STREAM, so a saga spanning several services comes back interleaved — with one node that never
// showed, but a real multi-service timeline reads as nonsense and, worse, deriveFailedStep walks
// the slice in order, so last_known_good would name whatever happened to sort first. Steps with an
// unparseable/absent timestamp keep their relative position at the end rather than being dropped.
func sortTimeline(saga *failcontext.Saga) {
	ts := func(s failcontext.SagaStep) (time.Time, bool) {
		t, err := time.Parse(time.RFC3339Nano, s.Timestamp)
		return t, err == nil
	}
	sort.SliceStable(saga.Timeline, func(i, j int) bool {
		ti, oki := ts(saga.Timeline[i])
		tj, okj := ts(saga.Timeline[j])
		if oki && okj {
			return ti.Before(tj)
		}
		return oki && !okj // parseable timestamps sort before unparseable ones
	})
}

// deriveFailedStep fills failed_step + last_known_good from the timeline, reading each step's
// status through normalizeStatus so a non-the operator vocabulary still yields the headline diagnostics.
// A step carrying an ERROR STRING counts as failed whatever its status says (PROB-2d): Social's
// DLQ/NACK control actions are level WARN — deliberately neither ok nor failed — so without this
// failed_step stayed empty on a saga that plainly shows the failure. A bare WARN with no error is
// still not a failure; inventing one would be worse than reporting none.
func deriveFailedStep(saga *failcontext.Saga) {
	lastGood := ""
	for _, step := range saga.Timeline {
		norm := normalizeStatus(step.StepStatus)
		failed := norm == "failed" || (norm != "ok" && step.Error != "")
		switch {
		case failed:
			if saga.FailedStep == "" {
				saga.FailedStep = step.StepName
				saga.LastKnownGood = lastGood
			}
		case norm == "ok":
			lastGood = step.StepName
		}
	}
}

const defaultQueryWindow = time.Hour

// parseWindow parses an override window; DF-09: default is LARGE (1h) so a not-just-run
// failure still returns its saga/logs (was 10m, which silently emptied an older run).
func parseWindow(w string) time.Duration {
	if d, err := time.ParseDuration(w); err == nil && d > 0 {
		return d
	}
	return defaultQueryWindow
}

// parseWindowStrict parses a window, returning an ERROR for an unparseable or non-positive
// value (Unit 4 / 4.5) instead of silently defaulting; empty means "use the default". The
// returned duration is the EFFECTIVE window the caller should echo.
func parseWindowStrict(w string) (time.Duration, error) {
	w = strings.TrimSpace(w)
	if w == "" {
		return defaultQueryWindow, nil
	}
	d, err := time.ParseDuration(w)
	if err != nil {
		return 0, fmt.Errorf("invalid window %q (use a Go duration like 15m, 1h)", w)
	}
	if d <= 0 {
		return 0, fmt.Errorf("window must be positive, got %q", w)
	}
	return d, nil
}

// validateQueryInput checks the correlation_id + window BEFORE touching Loki, so a bad
// parameter is reported honestly (Unit 4) and never as "Loki unreachable" (4.2): a missing
// correlation_id is required (4.4), a run-* id is the wrong id namespace (4.10), and a bad
// window is a window error (4.5). anything that is not the full minted
// tr-<run_id>-<scenario_id>-<8 hex> id is refused too (ValidateCorrelationID), so a fragment can
// never widen the match and a quote can never reach the query. Returns the effective window AND
// the validated, trimmed id — the caller must query THAT value, never its raw argument.
func validateQueryInput(correlationID, window string) (time.Duration, string, error) {
	cid := strings.TrimSpace(correlationID)
	if cid == "" {
		return 0, "", fmt.Errorf("correlation_id is required (the tr-... id from get_report)")
	}
	if strings.HasPrefix(cid, "run-") {
		return 0, "", fmt.Errorf("%q looks like a run_id — pass the tr-... correlation_id (from get_report), not the run_id", cid)
	}
	if err := ValidateCorrelationID(cid); err != nil {
		return 0, "", err
	}
	eff, err := parseWindowStrict(window)
	return eff, cid, err
}

// lokiEntry is one Loki result value: [entry-ts-nanos, log-line]. DF-15: the entry ts
// (v[0]) is Loki's authoritative timestamp — we keep it (was discarded) to backfill a
// log line whose body carries no `ts`.
type lokiEntry struct {
	TS   string // Loki entry timestamp, unix nanoseconds (string)
	Line string // the raw log body
}

// computeWindow is the LogQL [start,end] (DF-09). With a zero anchor it is relative to
// now ([now-window, now]); with an anchor (the run's timestamp) it is a band around the
// run ([anchor-window, anchor+window]) so a run older than the window still resolves.
func computeWindow(window string, anchor, now time.Time) (start, end time.Time) {
	w := parseWindow(window)
	if anchor.IsZero() {
		return now.Add(-w), now
	}
	return anchor.Add(-w), anchor.Add(w)
}

// parseLogLine maps a Loki entry to a LogLine (DF-15): TS = the body `ts` if present
// else Loki's entry ts (always present, authoritative); Msg = the body `msg` if present
// else the saga `step_name` (so structured saga lines are self-describing). CHANGE-2: the
// correlation value + the saga tag are read from the DECLARED fields; the bundle still
// normalizes the correlation value under the canonical `correlation_id` key.
func (l *Loki) parseLogLine(e lokiEntry) failcontext.LogLine {
	// GAP-2: JSON *or* logfmt. This used to be JSON-only, so a logfmt SUT (Memstore logs via Go's
	// stdlib logger) had every line fall through to the raw-line branch — no level, no service,
	// no correlation id, nothing structured. Its triage concluded the SUT emitted nothing on
	// failure paths; in truth we could not read what it emitted.
	// #422 deliberately does NOT apply here: the declared log_format governs which lines may count as
	// SAGA STEPS (sagaStep, and the verdict), because there a forged line is a forged step. A displayed
	// log line decides nothing, so it is still read either way — tightening this would bring GAP-2 back
	// for every SUT that logs logfmt without declaring it (json is the default).
	m, prefix, ok := parseKV(e.Line)
	if !ok {
		return failcontext.LogLine{Msg: e.Line, TS: entryTS(e.TS)}
	}
	ts := l.stepField(m, "timestamp") // ts / time / timestamp / @timestamp, or the declared name
	if ts == "" {
		ts = entryTS(e.TS)
	}
	msg := str(m["msg"])
	if msg == "" {
		msg = str(m["message"])
	}
	if msg == "" && str(m[l.sagaField()]) == l.sagaValue() {
		msg = l.stepField(m, "step_name")
	}
	// logfmt only: the human-readable header (`… INFO telemetry.event`) is not a k=v pair, so
	// without this a logfmt SUT's lines would render with an empty message. JSON lines have no
	// prefix, so the JSON contract — including an intentionally EMPTY msg — is untouched.
	if msg == "" {
		msg = prefix
	}
	return failcontext.LogLine{
		TS: ts, Level: str(m["level"]), Service: l.stepField(m, "service"), Msg: msg,
		Fields: map[string]string{"correlation_id": str(m[l.corrField()])},
	}
}

// entryTS converts Loki's unix-nanos entry timestamp to RFC3339; "" if unparseable.
func entryTS(nanos string) string {
	n, err := strconv.ParseInt(nanos, 10, 64)
	if err != nil || n == 0 {
		return ""
	}
	return time.Unix(0, n).UTC().Format(time.RFC3339)
}

// queryLines runs a LogQL query and returns the matching entries (ts+line),
// newest-relevant first. anchor (the run timestamp; zero = relative to now) anchors the
// window (DF-09). Best-effort: returns (nil,false) on any error.
func (l *Loki) queryLines(correlationID, window string, anchor time.Time) ([]lokiEntry, bool) {
	if l.BaseURL == "" || correlationID == "" {
		return nil, false
	}
	start, end := computeWindow(window, anchor, time.Now())
	// Filter on the correlation id, but EXCLUDE the suite's own observability infra —
	// otherwise Loki self-matches (it logs the very query string, which contains the
	// correlation id) and returns its own query-engine logs instead of the SUT's.
	// the id is rendered as an ESCAPED LogQL string literal (it is validated
	// upstream; this is the second lock — a quote or backslash can never end the literal).
	q := `{service=~".+",service!~"` + ObsInfraExclusion + `"} |= ` + logqlString(correlationID)
	entries, err := l.queryRange(q, start, end, 2000)
	if err != nil {
		return nil, false
	}
	return entries, true
}

// queryRange is the one Loki query_range call: Basic Auth only with a credential (T3.1), the tenant
// header only with a tenant (T3.3), a 4 s timeout, direction forward, the given limit. Unlike
// queryLines it returns WHY it failed (spec 26 finding A3): "HTTP 401", the transport error without
// the URL, or a decode error — never the response body (the server's free text) and never the URL
// (its query string holds the selector and ids, and a base URL may carry userinfo).
func (l *Loki) queryRange(q string, start, end time.Time, limit int) ([]lokiEntry, error) {
	api := strings.TrimRight(l.BaseURL, "/") + "/loki/api/v1/query_range?" + url.Values{
		"query":     {q},
		"start":     {strconv.FormatInt(start.UnixNano(), 10)},
		"end":       {strconv.FormatInt(end.UnixNano(), 10)},
		"limit":     {strconv.Itoa(limit)},
		"direction": {"forward"},
	}.Encode()

	req, err := http.NewRequest(http.MethodGet, api, nil)
	if err != nil {
		return nil, fmt.Errorf("build the Loki request: %w", withoutURL(err))
	}
	// T3.1 (E3 export hosted-Loki target): Basic Auth ONLY when a credential is configured — a
	// bundled/adopt Loki (no auth in front of it) must see no Authorization header at all, never
	// an empty one, which some proxies treat differently from "absent".
	if user, pass, ok := strings.Cut(l.Credential, ":"); ok {
		req.SetBasicAuth(user, pass)
	}
	setTenant(req, l.Tenant) // T3.3: X-Scope-OrgID only when a tenant is configured
	client := &http.Client{Timeout: 4 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("the Loki request failed: %w", withoutURL(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("Loki answered HTTP %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	var out struct {
		Data struct {
			Result []struct {
				Values [][2]string `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode the Loki answer: %w", err)
	}
	var entries []lokiEntry
	for _, r := range out.Data.Result {
		for _, v := range r.Values {
			entries = append(entries, lokiEntry{TS: v[0], Line: v[1]}) // [ts, line] — keep both (DF-15)
		}
	}
	return entries, nil
}

// withoutURL drops the URL net/http wraps into a request error (*url.Error carries the full URL,
// query string included) and keeps only what went wrong.
func withoutURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// Sagas returns the saga timeline for a correlation id (VR-A4) — the lines tagged
// event_type=saga, parsed into ordered steps.
func (l *Loki) Sagas(correlationID, window string, anchor time.Time) failcontext.Saga {
	eff, correlationID, err := validateQueryInput(correlationID, window)
	if err != nil {
		// 4.2/4.4/4.10: an input error is NOT a Loki outage — say what's actually wrong.
		return failcontext.Saga{Available: false, Timeline: []failcontext.SagaStep{}, Note: err.Error()}
	}
	entries, reachable := l.queryLines(correlationID, window, anchor)
	if !reachable {
		return failcontext.Saga{Available: false, Timeline: []failcontext.SagaStep{}, Window: eff.String(),
			Note: "saga query failed — Loki is unreachable or returned an error (check the obs stack)"}
	}
	saga := failcontext.Saga{Available: true, Window: eff.String(), Timeline: []failcontext.SagaStep{}} // C7: echo the effective window
	for _, entry := range entries {
		// GAP-1/GAP-2: parse JSON *or* logfmt, and read the step fields under the names THIS SUT
		// uses (declared saga_step_fields, else the the operator defaults + fallback chain). sagafields.go.
		step, ok := l.sagaStep(entry.Line)
		if !ok {
			continue
		}
		if step.Timestamp == "" {
			step.Timestamp = entryTS(entry.TS) // Loki's entry ts is authoritative and always present
		}
		saga.Timeline = append(saga.Timeline, step)
	}
	// PROB-2c: chronological order FIRST — Loki groups by stream, and deriveFailedStep walks the
	// slice in order, so last_known_good is only meaningful on a sorted timeline.
	sortTimeline(&saga)
	// PROB-2b: derive the headline diagnostics through the normalized status reading, so they work
	// for a SUT whose vocabulary is not the literal ok/failed (Social's steps read INFO/WARN/ERROR).
	deriveFailedStep(&saga)
	// 4.3: a successful-but-empty query — the id may be unknown OR the run is outside the
	// window. Say BOTH (the old note asserted only "widen window", a red herring for a wrong id).
	if len(saga.Timeline) == 0 {
		saga.Available = false
		saga.Note = "no saga events for this correlation_id in the window — the correlation_id may be unknown, or the run is outside the window (widen `window` or re-check the id)"
	}
	// GAP-1 honesty gate: lines matched the marker but no step field was readable → say UNREADABLE,
	// never a clean available:true. A blind panel that reports healthy defeats the checks meant to
	// catch it, and it silently degraded a whole triage round to log-scraping. See sagafields.go.
	markDegradedIfContentFree(&saga, l)
	return saga
}

// Logs returns windowed, correlation-scoped log lines (VR-A5). anchor (the run
// timestamp; zero = relative to now) anchors the window (DF-09).
func (l *Loki) Logs(correlationID, window string, anchor time.Time) failcontext.Logs {
	eff, correlationID, err := validateQueryInput(correlationID, window)
	if err != nil {
		// 4.2/4.4/4.10: honest input error, not "Loki unreachable".
		return failcontext.Logs{Available: false, Source: "loki", Window: window, Note: err.Error(), Lines: []failcontext.LogLine{}}
	}
	entries, reachable := l.queryLines(correlationID, window, anchor)
	out := failcontext.Logs{Available: reachable, Source: "loki", Window: eff.String(), // 4.5: echo the EFFECTIVE window
		Note: "primary signal is the saga; logs are the fallback / detail", Lines: []failcontext.LogLine{}}
	if !reachable {
		out.Note = "log query failed — Loki is unreachable or returned an error"
		return out
	}
	for _, entry := range entries {
		out.Lines = append(out.Lines, l.parseLogLine(entry)) // DF-15: ts/msg backfilled
	}
	// 4.3: empty is reported honestly — unknown id OR outside the window, not just "widen window".
	if len(out.Lines) == 0 {
		out.Available = false
		out.Note = "no log lines for this correlation_id in the window — the correlation_id may be unknown, or the run is outside the window (widen `window` or re-check the id)"
	}
	return out
}

func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
func strPtr(v any) *string {
	if s, ok := v.(string); ok && s != "" {
		return &s
	}
	return nil
}
func toInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case string:
		i, _ := strconv.Atoi(n)
		return i
	}
	return 0
}
