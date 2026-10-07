package obsquery

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/OneDro1d/argus-runner/internal/failcontext"
)

// ─────────────────────────────────────────────────────────────────────────────────────────
// BetterStack (AC-D13): the second implementation of Backend, queried over BetterStack's
// ClickHouse-flavoured HTTP SQL API (https://betterstack.com/docs/logs/query-api/connect-remotely/)
// instead of Loki's LogQL. One HTTP round trip per configured SOURCE (BetterStack: one source per
// service, logs only — there is no cross-service query surface the way a Loki stream can carry
// several services), merged in Go; a row is tagged with the SOURCE's configured service name
// (per-service grouping falls out of that, not a SQL GROUP BY).
//
// SQL injection posture (REQUIRED 2): every user-supplied VALUE (the correlation id, the window
// edges, the row cap, the saga marker values) is sent as a ClickHouse HTTP query PARAMETER
// (`?param_name=value`, referenced in the SQL body as `{name:Type}`) — never string-concatenated
// into the SQL text. The only things built directly into the SQL text are IDENTIFIERS (the team
// id, the source slug/id that names the table, and a field path that names a JSON key to read) —
// each is checked against a safe allow-list pattern (config.bsSafeIdentRe / bsFieldPathRe,
// re-checked here defensively) BEFORE it is used; an identifier that fails the check is skipped
// rather than ever reaching the request.
// ─────────────────────────────────────────────────────────────────────────────────────────

// betterStackIdentRe / betterStackFieldPathRe mirror the config package's safe-pattern checks
// (config.bsSafeIdentRe / config.bsFieldPathRe) — duplicated rather than imported so obsquery
// stays decoupled from config (the same convention Loki already follows: config values are
// THREADED IN as plain fields, obsquery never imports config). Defense in depth: an identifier
// is validated again here even though config.Load already refused a bad one at load time.
var (
	betterStackIdentRe     = regexp.MustCompile(`^[A-Za-z0-9_]+$`)
	betterStackFieldPathRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)*$`)
)

// betterStackTimeout is the default per-request HTTP deadline (mirrors Loki's 4s query timeout,
// widened slightly for an external hosted API rather than the bundled, same-network Loki).
const betterStackTimeout = 8 * time.Second

// betterStackRowCap is the default LIMIT applied to every query (mirrors Loki's queryLines
// limit=2000) — a row cap, always present, never an unbounded query.
const betterStackRowCap = 2000

var defaultBetterStackCorrelationFields = []string{"message_json.correlationId", "correlation_id"}

// BetterStack queries a BetterStack (Telemetry) account over its HTTP SQL API. The *Field/
// *Fields knobs mirror Loki's — threaded in from argus-config by the caller (toolcore), same
// pattern as Loki, so obsquery need not import config.
type BetterStack struct {
	QueryURL string
	// Credential is the ALREADY-RESOLVED "username:password" Basic-Auth pair (config.Load
	// resolves the ${VAR} reference before this is ever constructed).
	Credential string
	TeamID     string
	// Sources maps a source slug/id (the table name's `<source>` segment) to the service name it
	// carries. BetterStack: one source per service (CONTEXT), so this is also how a row's Service
	// field is derived — no per-line "service" field to read, unlike Loki.
	Sources map[string]string
	// CorrelationFields are JSON field PATHS (dot-separated) tried in order; empty means the
	// default message_json.correlationId → correlation_id chain.
	CorrelationFields []string
	LevelField        string
	SagaEventField    string
	SagaEventValue    string
	SagaEventValues   []string
	SagaStepFields    map[string]string
	// HTTPClient overrides the client (tests only); nil uses a client with Timeout (or the
	// package default betterStackTimeout when Timeout is zero).
	HTTPClient *http.Client
	Timeout    time.Duration
	// RowCap overrides the default LIMIT (tests only); <=0 uses betterStackRowCap.
	RowCap int
}

func (b *BetterStack) correlationFields() []string {
	if len(b.CorrelationFields) > 0 {
		return b.CorrelationFields
	}
	return defaultBetterStackCorrelationFields
}
func (b *BetterStack) levelField() string {
	if b.LevelField != "" {
		return b.LevelField
	}
	return "level"
}
func (b *BetterStack) sagaField() string {
	if b.SagaEventField != "" {
		return b.SagaEventField
	}
	return "event_type"
}
func (b *BetterStack) sagaValues() []string {
	if len(b.SagaEventValues) > 0 {
		return b.SagaEventValues
	}
	if b.SagaEventValue != "" {
		return []string{b.SagaEventValue}
	}
	return []string{"saga"}
}
func (b *BetterStack) isSagaMarker(v string) bool {
	for _, want := range b.sagaValues() {
		if v == want {
			return true
		}
	}
	return false
}
func (b *BetterStack) rowCap() int {
	if b.RowCap > 0 {
		return b.RowCap
	}
	return betterStackRowCap
}
func (b *BetterStack) timeout() time.Duration {
	if b.Timeout > 0 {
		return b.Timeout
	}
	return betterStackTimeout
}
func (b *BetterStack) httpClient() *http.Client {
	if b.HTTPClient != nil {
		return b.HTTPClient
	}
	return &http.Client{Timeout: b.timeout()}
}

// stepField resolves ONE logical step field (saga_id/step/step_name/step_status/timestamp/error/
// service): the SUT's declared name wins, else the stepFieldFallbacks chain (shared with Loki,
// sagafields.go) — SAME meaning as observability.loki.saga_step_fields (CONTEXT).
func (b *BetterStack) stepField(m map[string]any, logical string) string {
	if name := b.SagaStepFields[logical]; name != "" {
		return str(m[name])
	}
	for _, cand := range stepFieldFallbacks[logical] {
		if v, ok := m[cand]; ok {
			if s := str(v); s != "" {
				return s
			}
		}
	}
	return ""
}

// tableName builds `t<team>_<source>_logs` — ONLY after both segments pass the safe-identifier
// check. ok=false (never a raw/half-built string) when either fails, so a bad identifier simply
// drops that source from the query rather than ever reaching an HTTP request.
func (b *BetterStack) tableName(sourceSlug string) (string, bool) {
	if !betterStackIdentRe.MatchString(b.TeamID) || !betterStackIdentRe.MatchString(sourceSlug) {
		return "", false
	}
	return "t" + b.TeamID + "_" + sourceSlug + "_logs", true
}

// jsonExtractExpr builds a JSONExtractString(raw, 'seg', 'seg', ...) fragment for a dot-path —
// ONLY after every segment passes the safe field-path pattern. ok=false drops the field from the
// query (never half-built with an unchecked segment).
func jsonExtractExpr(path string) (string, bool) {
	if !betterStackFieldPathRe.MatchString(path) {
		return "", false
	}
	segs := strings.Split(path, ".")
	quoted := make([]string, len(segs))
	for i, s := range segs {
		quoted[i] = "'" + s + "'"
	}
	return "JSONExtractString(raw, " + strings.Join(quoted, ", ") + ")", true
}

// bsQuery is a built SQL statement + its PARAMETERS (never a value concatenated into sql).
type bsQuery struct {
	sql    string
	params map[string]string
}

// buildQuery builds the SELECT for one source table: a correlation-id predicate (every declared
// field path, ORed, each compared against the SAME {correlation_id:String} PARAMETER — never the
// raw id text), a window predicate ({start:DateTime64}/{end:DateTime64}), and — when sagaOnly —
// an additional predicate matching any declared saga marker VALUE, each its OWN parameter
// ({saga_marker_0:String}, {saga_marker_1:String}, ...; never concatenated).
func (b *BetterStack) buildQuery(table string, start, end time.Time, sagaOnly bool) bsQuery {
	params := map[string]string{
		"correlation_id": "", // filled by caller (queryAllSources) — kept here for a stable key order
		"start":          start.UTC().Format("2006-01-02 15:04:05"),
		"end":            end.UTC().Format("2006-01-02 15:04:05"),
		"row_cap":        strconv.Itoa(b.rowCap()),
	}
	var corrPreds []string
	for _, p := range b.correlationFields() {
		if expr, ok := jsonExtractExpr(p); ok {
			corrPreds = append(corrPreds, corrPredicate(expr))
		}
	}
	if len(corrPreds) == 0 {
		// no usable field path at all — fall back to the canonical flat key so the query still
		// makes sense rather than degenerating to "WHERE ()".
		corrPreds = []string{corrPredicate("JSONExtractString(raw, 'correlation_id')")}
	}
	where := "(" + strings.Join(corrPreds, " OR ") + ")" +
		" AND dt >= {start:DateTime64} AND dt <= {end:DateTime64}"

	if sagaOnly {
		var markerPreds []string
		if expr, ok := jsonExtractExpr(b.sagaField()); ok {
			for i, v := range b.sagaValues() {
				key := "saga_marker_" + strconv.Itoa(i)
				params[key] = v
				markerPreds = append(markerPreds, expr+" = {"+key+":String}")
			}
		}
		if len(markerPreds) > 0 {
			where += " AND (" + strings.Join(markerPreds, " OR ") + ")"
		}
	}

	sql := "SELECT dt, raw FROM remote(" + table + ") WHERE " + where +
		" ORDER BY dt ASC LIMIT {row_cap:UInt32}" +
		" SETTINGS output_format_json_array_of_rows = 1 FORMAT JSONEachRow"
	return bsQuery{sql: sql, params: params}
}

// corrPredicate matches a log field equal to the correlation id OR carrying it as the prefix of a
// per-call MCP request id (`<corr>.<8 hex>`, mcp.PerCallRequestID). An exact match
// alone would lose every MCP call's line on a SUT that logs its request id. Loki needs no twin: its
// lookup is already a substring filter. The id is still ONLY a parameter, never text in the SQL.
func corrPredicate(expr string) string {
	return "(" + expr + " = {correlation_id:String} OR startsWith(" + expr + ", concat({correlation_id:String}, '.')))"
}

// doQuery POSTs the SQL text (NEVER a value baked into it — see buildQuery) with every parameter
// carried in the URL query string as ClickHouse's `param_<name>` convention, Basic-Auth from the
// resolved "user:pass" credential. Best-effort like Loki's queryLines: any error, non-200, or an
// unparseable body returns (nil,false) — never a partial/garbled result.
func (b *BetterStack) doQuery(q bsQuery) ([]bsRow, bool) {
	if b.QueryURL == "" {
		return nil, false
	}
	u, err := url.Parse(b.QueryURL)
	if err != nil {
		return nil, false
	}
	qs := u.Query()
	for k, v := range q.params {
		qs.Set("param_"+k, v)
	}
	qs.Set("output_format_pretty_row_numbers", "0")
	u.RawQuery = qs.Encode()

	req, err := http.NewRequest(http.MethodPost, u.String(), strings.NewReader(q.sql))
	if err != nil {
		return nil, false
	}
	req.Header.Set("Content-type", "plain/text")
	if user, pass, ok := strings.Cut(b.Credential, ":"); ok {
		req.SetBasicAuth(user, pass)
	}
	resp, err := b.httpClient().Do(req)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, false
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, false
	}
	var rows []bsRow
	if err := json.Unmarshal(body, &rows); err != nil {
		return nil, false
	}
	return rows, true
}

// bsRow is one result row: SELECT dt, raw FROM remote(...) — the doc's own example shape.
type bsRow struct {
	DT  string `json:"dt"`
	Raw string `json:"raw"`
}

// bsResultRow is a fetched row tagged with the SOURCE's configured service name (per-service
// grouping — REQUIRED 2).
type bsResultRow struct {
	ts      string
	raw     string
	service string
}

func sortedSourceSlugs(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// queryAllSources runs the built query against EVERY configured source, merging results and
// tagging each row with its source's service name. reachable=true when AT LEAST ONE source
// answered (best-effort-maximal, matching the package's stated Loki philosophy — a single down
// source must not blank every other service's evidence); reachable=false only when the query
// input itself has nothing usable to query (no URL/id/sources) or EVERY source failed.
func (b *BetterStack) queryAllSources(correlationID, window string, anchor time.Time, sagaOnly bool) ([]bsResultRow, bool) {
	if b.QueryURL == "" || correlationID == "" || len(b.Sources) == 0 {
		return nil, false
	}
	start, end := computeWindow(window, anchor, time.Now())
	var out []bsResultRow
	anySucceeded := false
	for _, slug := range sortedSourceSlugs(b.Sources) {
		table, ok := b.tableName(slug)
		if !ok {
			continue // an unsafe identifier is DROPPED, never built into a request
		}
		q := b.buildQuery(table, start, end, sagaOnly)
		q.params["correlation_id"] = correlationID // the ONLY place the raw value is attached — as a PARAMETER
		rows, reachable := b.doQuery(q)
		if !reachable {
			continue
		}
		anySucceeded = true
		service := b.Sources[slug]
		for _, r := range rows {
			out = append(out, bsResultRow{ts: r.DT, raw: r.Raw, service: service})
		}
	}
	return out, anySucceeded
}

// parseLogLine mirrors Loki.parseLogLine's contract (DF-15/GAP-1/GAP-2 semantics) but reads the
// correlation id via the declared PATH chain (CorrelationFields) instead of a flat key, and
// falls back to the source's configured service name when the line carries none of its own.
func (b *BetterStack) parseLogLine(r bsResultRow) failcontext.LogLine {
	m, prefix, ok := parseKV(r.raw)
	if !ok {
		return failcontext.LogLine{Msg: r.raw, TS: r.ts, Service: r.service}
	}
	ts := b.stepField(m, "timestamp")
	if ts == "" {
		ts = r.ts
	}
	msg := str(m["msg"])
	if msg == "" {
		msg = str(m["message"])
	}
	if msg == "" && str(m[b.sagaField()]) == firstOf(b.sagaValues()) {
		msg = b.stepField(m, "step_name")
	}
	if msg == "" {
		msg = prefix
	}
	service := b.stepField(m, "service")
	if service == "" {
		service = r.service
	}
	return failcontext.LogLine{
		TS: ts, Level: str(m[b.levelField()]), Service: service, Msg: msg,
		Fields: map[string]string{"correlation_id": b.correlationValue(m)},
	}
}

// correlationValue reads the correlation id via the declared path chain, trying each in order —
// the FIRST path that resolves to a non-empty value wins (mirrors the config default's stated
// order: message_json.correlationId, then correlation_id).
func (b *BetterStack) correlationValue(m map[string]any) string {
	for _, p := range b.correlationFields() {
		if v := getPath(m, p); v != "" {
			return v
		}
	}
	return ""
}

func getPath(m map[string]any, path string) string {
	var cur any = m
	for _, seg := range strings.Split(path, ".") {
		mm, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		v, ok := mm[seg]
		if !ok {
			return ""
		}
		cur = v
	}
	return str(cur)
}

func firstOf(v []string) string {
	if len(v) == 0 {
		return ""
	}
	return v[0]
}

// sagaStep mirrors Loki.sagaStep (sagafields.go): the line must carry a DECLARED saga marker
// value, then every step field is resolved through the declared/fallback chain.
func (b *BetterStack) sagaStep(r bsResultRow) (failcontext.SagaStep, bool) {
	m, _, ok := parseKV(r.raw)
	if !ok {
		return failcontext.SagaStep{}, false
	}
	if !b.isSagaMarker(str(m[b.sagaField()])) {
		return failcontext.SagaStep{}, false
	}
	service := b.stepField(m, "service")
	if service == "" {
		service = r.service
	}
	return failcontext.SagaStep{
		SagaID:     b.stepField(m, "saga_id"),
		Step:       toInt(m[orDefaultKey(b.SagaStepFields["step"], "step")]),
		StepName:   b.stepField(m, "step_name"),
		Service:    service,
		Timestamp:  b.stepField(m, "timestamp"),
		StepStatus: b.stepField(m, "step_status"),
		Error:      b.stepField(m, "error"),
		Fields: failcontext.SagaFields{
			What: strPtr(m["what"]), Why: strPtr(m["why"]), ByWhom: strPtr(m["by_whom"]),
		},
	}, true
}

// markDegradedIfContentFree mirrors sagafields.go's honesty gate for Loki: marker-matched lines
// whose step fields are ALL unreadable are DEGRADED, never a clean available:true.
func (b *BetterStack) markDegradedIfContentFree(saga *failcontext.Saga) {
	if len(saga.Timeline) == 0 {
		return
	}
	for _, s := range saga.Timeline {
		if s.StepName != "" || s.StepStatus != "" || s.Timestamp != "" {
			return
		}
	}
	saga.Available = false
	saga.Note = "saga lines WERE found for this correlation_id (" + itoa(len(saga.Timeline)) +
		" matched the declared marker " + b.sagaField() + "=" + firstOf(b.sagaValues()) +
		") but none of their step fields could be read — every step_name, step_status and timestamp is empty. " +
		"This SUT names its saga step fields differently: declare them in argus-config under " +
		"observability.betterstack.saga_step_fields (step_name / step_status / timestamp / saga_id / step / error). " +
		"Treat this as UNREADABLE, not as a healthy saga."
}

// Sagas returns the saga timeline for a correlation id — same contract as Loki.Sagas (VR-A4):
// input errors are honest (never "unreachable"), a query failure is the typed unavailable
// outcome, an empty-but-successful result names both possible causes, and a content-free
// marker-matched timeline is reported degraded rather than falsely healthy.
func (b *BetterStack) Sagas(correlationID, window string, anchor time.Time) failcontext.Saga {
	eff, correlationID, err := validateQueryInput(correlationID, window)
	if err != nil {
		return failcontext.Saga{Available: false, Timeline: []failcontext.SagaStep{}, Note: err.Error()}
	}
	rows, reachable := b.queryAllSources(correlationID, window, anchor, true)
	if !reachable {
		return failcontext.Saga{Available: false, Timeline: []failcontext.SagaStep{}, Window: eff.String(),
			Note: "saga query failed — BetterStack is unreachable or returned an error (check the obs stack)"}
	}
	saga := failcontext.Saga{Available: true, Window: eff.String(), Timeline: []failcontext.SagaStep{}}
	for _, r := range rows {
		step, ok := b.sagaStep(r)
		if !ok {
			continue
		}
		if step.Timestamp == "" {
			step.Timestamp = r.ts
		}
		saga.Timeline = append(saga.Timeline, step)
	}
	sortTimeline(&saga)
	deriveFailedStep(&saga)
	if len(saga.Timeline) == 0 {
		saga.Available = false
		saga.Note = "no saga events for this correlation_id in the window — the correlation_id may be unknown, or the run is outside the window (widen `window` or re-check the id)"
	}
	b.markDegradedIfContentFree(&saga)
	return saga
}

// Logs returns windowed, correlation-scoped log lines — same contract as Loki.Logs (VR-A5).
func (b *BetterStack) Logs(correlationID, window string, anchor time.Time) failcontext.Logs {
	eff, correlationID, err := validateQueryInput(correlationID, window)
	if err != nil {
		return failcontext.Logs{Available: false, Source: "betterstack", Window: window, Note: err.Error(), Lines: []failcontext.LogLine{}}
	}
	rows, reachable := b.queryAllSources(correlationID, window, anchor, false)
	out := failcontext.Logs{Available: reachable, Source: "betterstack", Window: eff.String(),
		Note: "primary signal is the saga; logs are the fallback / detail", Lines: []failcontext.LogLine{}}
	if !reachable {
		out.Note = "log query failed — BetterStack is unreachable or returned an error"
		return out
	}
	for _, r := range rows {
		out.Lines = append(out.Lines, b.parseLogLine(r))
	}
	if len(out.Lines) == 0 {
		out.Available = false
		out.Note = "no log lines for this correlation_id in the window — the correlation_id may be unknown, or the run is outside the window (widen `window` or re-check the id)"
	}
	return out
}
