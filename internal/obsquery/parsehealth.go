package obsquery

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Live check 2026-10-19 - PARSE HEALTH of a shared-Loki read.
//
// A shared Loki has no ingest-time labels, so `level` / `correlation_id` / `event_type` exist only if the query's
// parser stage (`| json`, `| logfmt`, `| regexp`) can read the SUT's lines. Measured against the real shared Loki
// (a hosted AKS cluster, shop-uni-arb): the SUT logs PLAIN TEXT (`10:06:24.852569 arb-engine WARN: [PollTick] ...`), so
// `| json` parsed ZERO of 9,521 lines in an hour. A `labels: {level: error}` claim with `count == 0` then reads zero
// lines for a reason that has nothing to do with errors - the lines are there (so NoStream does not fire) but none
// yields a level - and PASSES vacuously.
//
// So a shared read that returns no line AND filtered on a parsed label asks once more, over the SAME selector and
// window: how many lines are there, and how many of them does the parser read? Lines but NO parsed line = the
// declared format does not parse this SUT = LogsRead.Unparsed (the step is `error`). Some parsed = the zero is a
// real zero and is kept.
//
// ⛔ A METRIC QUERY WITH A PARSER MUST CARRY `| __error__=""`. Measured: real Loki REJECTS
// `count_over_time({...} | json [5m])` with `pipeline error: 'JSONParserErr' ... Use a label filter to intentionally
// skip this error`. The probe's parsed count therefore always filters on it.
// The `| regexp` parser sets no __error__ on a line it does not match (it just sets no label), so a plain-text
// format is counted with the same expression as a LINE filter (`|~`), which is exactly "the lines the expression
// matches".
// ⛔ COUNTS ONLY. Nothing of a line reaches the result.

// UnparsedLines says a shared read found lines that its parser stage read NONE of.
type UnparsedLines struct {
	Format string // "json" | "logfmt" | "regexp"
	Lines  int    // how many lines the selector holds in the window (a count, never content)
}

// splitParser cuts a shared-mode source query at its parser stage: the stream selector, the parser stage text
// (` | json level="level"`, ` | logfmt ...`, ` | regexp "<expr>"`) and its name. Quotes are honoured, so a line
// filter that happens to contain " | json" is not a stage. ok is false for a query with no parser stage.
func splitParser(q string) (sel, stage, format string, ok bool) {
	sel, found := leadingSelector(q)
	if !found {
		return "", "", "", false
	}
	rest := q[len(sel):]
	inStr := false
	start := -1
	for i := 0; i < len(rest); i++ {
		c := rest[i]
		switch {
		case inStr && c == '\\':
			i++
			continue
		case c == '"':
			inStr = !inStr
			continue
		case inStr:
			continue
		}
		if !strings.HasPrefix(rest[i:], " | ") {
			continue
		}
		if start >= 0 {
			return sel, rest[start:i], format, true
		}
		for _, p := range []string{"json", "logfmt", "regexp"} {
			after := rest[i+3:]
			if strings.HasPrefix(after, p) && (len(after) == len(p) || after[len(p)] == ' ') {
				start, format = i, p
				break
			}
		}
	}
	if start >= 0 {
		return sel, rest[start:], format, true
	}
	return "", "", "", false
}

// ParseHealthQueries are the two metric queries of the probe over a window of that many seconds: every line of the
// selector, and the lines its parser stage reads. ok is false when the query has no parser stage (nothing to probe).
func ParseHealthQueries(query string, windowSeconds int) (total, parsed, format string, ok bool) {
	sel, stage, format, ok := splitParser(query)
	if !ok {
		return "", "", "", false
	}
	if windowSeconds < 1 {
		windowSeconds = 1
	}
	rng := " [" + strconv.Itoa(windowSeconds) + "s]"
	total = "sum(count_over_time(" + sel + rng + "))"
	if format == "regexp" {
		lit := strings.TrimPrefix(stage, " | regexp ")
		return total, "sum(count_over_time(" + sel + " |~ " + lit + rng + "))", format, true
	}
	return total, "sum(count_over_time(" + sel + stage + ` | __error__=""` + rng + "))", format, true
}

// parseHealth runs the probe for a shared read that returned no line. nil = nothing to report (no parser stage, no
// lines at all, or some lines parse).
func (l *Loki) parseHealth(query string, from, to time.Time) (*UnparsedLines, error) {
	secs := int(math.Ceil(to.Sub(from).Seconds()))
	totalQ, parsedQ, format, ok := ParseHealthQueries(query, secs)
	if !ok {
		return nil, nil
	}
	total, err := l.instantCount(totalQ, to)
	if err != nil {
		return nil, err
	}
	if total == 0 {
		return nil, nil
	}
	parsed, err := l.instantCount(parsedQ, to)
	if err != nil {
		return nil, err
	}
	if parsed > 0 {
		return nil, nil
	}
	return &UnparsedLines{Format: format, Lines: total}, nil
}

// instantCount is one /loki/api/v1/query of a `sum(count_over_time(...))` evaluated at `at`: the number, 0 for an
// empty vector. Same credential, tenant and timeout as queryRange; the error never carries the URL or the body.
func (l *Loki) instantCount(q string, at time.Time) (int, error) {
	api := strings.TrimRight(l.BaseURL, "/") + "/loki/api/v1/query?" + url.Values{
		"query": {q},
		"time":  {strconv.FormatInt(at.UnixNano(), 10)},
	}.Encode()
	req, err := http.NewRequest(http.MethodGet, api, nil)
	if err != nil {
		return 0, fmt.Errorf("build the Loki request: %w", withoutURL(err))
	}
	if user, pass, ok := strings.Cut(l.Credential, ":"); ok {
		req.SetBasicAuth(user, pass)
	}
	setTenant(req, l.Tenant)
	resp, err := (&http.Client{Timeout: 8 * time.Second}).Do(req)
	if err != nil {
		return 0, fmt.Errorf("the Loki request failed: %w", withoutURL(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return 0, fmt.Errorf("Loki answered HTTP %d to the parse-health count", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	var out struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Value [2]json.RawMessage `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return 0, fmt.Errorf("decode the Loki answer: %w", err)
	}
	if out.Status != "success" {
		return 0, errors.New("the Loki answer to the parse-health count does not say status \"success\"")
	}
	if out.Data.ResultType != "vector" {
		return 0, fmt.Errorf("the Loki answer to the parse-health count is a %q, not a vector", out.Data.ResultType)
	}
	if len(out.Data.Result) == 0 {
		return 0, nil
	}
	var s string
	if err := json.Unmarshal(out.Data.Result[0].Value[1], &s); err != nil {
		return 0, fmt.Errorf("the parse-health count is not a string sample: %w", err)
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f < 0 {
		return 0, errors.New("the parse-health count is not a number")
	}
	return int(f), nil
}
