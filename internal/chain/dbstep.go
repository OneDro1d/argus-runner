package chain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/OneDro1d/argus-runner/internal/mcp"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// THE "db" CHAIN STEP: one read-only SELECT against a declared PostgreSQL target, its rows
// judged by the `body …` claims on ONE JSON document:
//
//	{"row_count": <n>, "rows": [ {"<column>": <value>, …}, … ]}
//
// ⛔ NO PARALLEL EXECUTOR AND NO SECOND CLAIM GRAMMAR. This is a Step built for the SAME chain.Run, its claims are
// the existing `body …` forms parsed by scenario.ParseBodyAsserts and judged by mcp.BodyAssertsMissReason against
// that document (a dotted path indexes an array element: `rows.0.status`), and `save` goes through the functions an
// http step's save uses.
//
// ⛔ THE STATEMENT IS READ-ONLY IN THREE PLACES. The authoring validator and the executor's preflight refuse a query
// that could write (scenario.DBStepQueryErrors / DBStepReadOnlyViolations - one implementation, shared with
// `## VERIFY`); the querier refuses it a third time, at the SERVER, by running it inside BEGIN READ ONLY and
// always rolling back (internal/pgread). A write that got past the first two is stopped by PostgreSQL itself.
//
// ⛔ A SAVED VALUE NEVER REACHES THE SQL TEXT. The query is sent byte-for-byte as the scenario wrote it (with the
// run's own id filled in by the caller); `${saved.<var>}` can only stand in `params`, which are sent as bind
// parameters. The step binds them here, at run time, from the capture store.

// DBRows is what a querier returns: the columns in the order the server named them, and each row's cells. A cell
// is one of: nil (NULL), bool, string, json.Number (an integer or numeric, EVERY digit kept - never a float64),
// float64, time.Time (rendered as RFC 3339 in UTC), json.RawMessage (json/jsonb, the JSON value itself). Anything
// else the querier has already turned into the server's text form, a string.
type DBRows struct {
	Columns []string
	Rows    [][]any
}

// DBServerError is the server REFUSING the statement (unknown table, permission denied, statement timeout, a write
// refused by READ ONLY, a syntax error): the query did not run. It is never retried by a poll - a syntax error does
// not heal - and it is a `failed` step, never "no rows".
type DBServerError struct {
	SQLState string
	Message  string
}

func (e *DBServerError) Error() string { return e.SQLState + ": " + e.Message }

// DBQuerier is one open connection to the target. Query runs ONE statement inside a read-only transaction that is
// always rolled back, with the statement timeout set for that transaction, binding params as $1..$n, and reads at
// most limit rows (limit+1 is asked of the step: the extra row is how the step knows the cap was hit). A refusal by
// the server is a *DBServerError; every other error (the connection, the deadline) is not.
//
// ⛔ CONTRACT: after a Query that returned limit rows the connection is SPENT. A querier may stop reading (and cancel
// the statement) once it has limit rows, and a driver can close the connection when it does, so the caller must not
// Query on it again; it closes it. The step honours this: an over-cap read ends the step as an `error`, on every
// attempt of a poll, and the step never queries that connection again.
type DBQuerier interface {
	Query(ctx context.Context, query string, params []string, statementTimeout time.Duration, limit int) (DBRows, error)
	Close() error
}

// DBDialer opens a DBQuerier for a parsed target. DialPG in internal/pgread is the production one; tests
// substitute a fake, so the runner is exercised without a database.
type DBDialer func(ctx context.Context, conn DBConn) (DBQuerier, error)

// DBSpec is one db step as the executor resolved it.
type DBSpec struct {
	// Query is the statement text, the run's own id already filled in. It is sent to the server UNCHANGED.
	Query string
	// Params are the bind parameters; a `${saved.<var>}` in one is bound here at run time.
	Params  []string
	Conn    DBConn
	Timeout time.Duration
	MaxRows int
	// Scrub cleans a server message of the target's credentials (nil = none).
	Scrub func(string) string
}

// DBStep builds the db step. want is the step's `body …` claims; save, poll and always are an http step's. dial
// opens the connection (pgread.Dial in production): ONE connection per step, opened at its first attempt, reused
// by the poll attempts, closed when the step returns. No pool.
func DBStep(name string, spec DBSpec, want []mcp.BodyAssert, save map[string]scenario.SaveSpec, poll *Poll, always bool, dial DBDialer) Step {
	scrub := func(s string) string {
		if spec.Scrub != nil {
			s = spec.Scrub(s)
		}
		return s
	}
	if spec.MaxRows <= 0 {
		spec.MaxRows = scenario.DBMaxRowsDefault
	}
	if spec.Timeout <= 0 {
		spec.Timeout = scenario.DBTimeoutDefault
	}
	needs := func(vars map[string]string) error {
		for _, p := range spec.Params {
			if _, err := bindParam(p, vars); err != nil {
				return err
			}
		}
		return needsBodyAsserts(want, vars)
	}
	enforced := func() []string { return EnforcedAssertions(mcp.Expect{Body: want}) }

	run := func(cid string, vars map[string]string, chainDeadline time.Time) report.StepResult {
		params := make([]string, len(spec.Params))
		for i, p := range spec.Params {
			b, err := bindParam(p, vars)
			if err != nil {
				// An authoring error (an unresolved ${saved.<var>}), never retried and nothing is sent.
				return report.StepResult{Status: "failed", Observed: err.Error()}
			}
			params[i] = b
		}
		boundWant, cerr := bindBodyAsserts(want, vars)
		if cerr != nil {
			return report.StepResult{Status: "failed", Observed: "content assertion: " + cerr.Error()}
		}
		redact := func(s string) string { return redactEverySaved(scrub(s), vars, "") }

		timeout, interval := time.Duration(0), time.Second
		if poll != nil {
			timeout, interval = poll.Timeout, poll.Interval
			// (the floor on the interval, scenario.DBPollIntervalMin, is refused at both doors - the step trusts them,
			// so a test can poll fast without a real database to hammer)
		}
		deadline := time.Now().Add(timeout)
		var q DBQuerier
		defer func() {
			if q != nil {
				_ = q.Close()
			}
		}()
		attempts := 0
		var lastObserved string
		var lastFailed []report.FailedClaim
		lastOmitted := 0
		for {
			attempts++
			ctx, cancel := dbContext(spec.Timeout, chainDeadline)
			if q == nil {
				var derr error
				q, derr = dial(ctx, spec.Conn)
				if derr != nil {
					cancel()
					q = nil
					return dbConnectError("cannot connect to the database", derr, redact)
				}
			}
			rows, qerr := q.Query(ctx, spec.Query, params, spec.Timeout, spec.MaxRows+1)
			cancel()
			if qerr != nil {
				var se *DBServerError
				if errors.As(qerr, &se) {
					// The server refused the statement: it did not run. `failed`, said as what it is, never
					// "no rows", and never retried.
					//
					// ⛔ Observed is builder-visible and carries the SQLSTATE ONLY: a PostgreSQL message quotes the
					// query (a table or column name, a syntax fragment) and can quote a bound parameter, and the
					// query is the check's held-out content. The server's own words go to the author in BodyExcerpt
					// (scrubbed, saved values masked, cut on a rune boundary - the http step's own helper).
					st := report.StepResult{Status: "failed", Observed: fmt.Sprintf(
						"database query did not run (SQLSTATE %s)", se.SQLState)}
					// AC-F2.1: the code in its own field, only when it is a SQLSTATE (author_get_report_limited sends it).
					st.ObservedSQLState = observedSQLState(se.SQLState)
					if se.Message != "" {
						st.BodyExcerpt = bodyExcerpt([]byte(se.Message), false, vars, scrub)
					}
					return st
				}
				return dbConnectError("the database did not answer", qerr, redact)
			}
			if len(rows.Rows) > spec.MaxRows {
				return report.StepResult{Status: report.StatusError, Observed: fmt.Sprintf(
					"the query returned more than max_rows (%d) rows, so the answer would be cut and the claims judged on part of it - "+
						"add a LIMIT, narrow the WHERE, or select count(*) instead", spec.MaxRows)}
			}
			doc, derr := dbDocument(rows)
			if derr != nil {
				return report.StepResult{Status: report.StatusError, Observed: derr.Error()}
			}

			miss, nonNumeric := mcp.BodyAssertsMissReason(string(doc), boundWant)
			if !miss {
				st := report.StepResult{Status: "passed", Observed: fmt.Sprintf("db query returned %d row(s)", len(rows.Rows))}
				st.AssertionsEnforced = enforced()
				st.AssertionsEnforcedCount = len(st.AssertionsEnforced)
				if len(save) > 0 {
					captured := make(map[string]string, len(save))
					for varName, sp := range save {
						val, fail := saveFromHTTP(doc, varName, sp)
						if fail != "" {
							return report.StepResult{Status: "failed", Observed: fail}
						}
						recordCapture(captured, varName, sp, val)
					}
					st.Captured = captured
				}
				return st
			}
			reason := "the rows did not satisfy the scenario's body assertion(s)"
			if nonNumeric {
				reason = "a numeric comparison in the scenario's body assertion(s) found a non-numeric observed value"
			}
			lastObserved = fmt.Sprintf("db query returned %d row(s) but %s %s", len(rows.Rows), reason, chainClaimsNote)
			lastFailed, lastOmitted = failedBodyClaims(string(doc), want, boundWant, vars, scrub)

			if poll == nil || time.Now().After(deadline) {
				st := report.StepResult{Status: "failed", Observed: lastObserved, FailedClaims: lastFailed, FailedClaimsOmitted: lastOmitted}
				if poll != nil {
					st.Observed = fmt.Sprintf("poll timed out after %s (%d attempt(s)); last observed: %s", timeout, attempts, lastObserved)
				}
				st.AssertionsEnforced = enforced()
				st.AssertionsEnforcedCount = len(st.AssertionsEnforced)
				return st
			}
			time.Sleep(interval)
		}
	}

	return Step{
		Name:     name,
		Always:   always,
		OwnError: true,
		Needs: func(vars map[string]string) error {
			if always {
				return nil // a cleanup step attempts regardless - see ChainStep.Always
			}
			return needs(vars)
		},
		Run: func(cid string, vars map[string]string) report.StepResult {
			return run(cid, vars, time.Time{})
		},
		RunUntil: run,
	}
}

// observedSQLState returns code when it is a SQLSTATE — exactly 5 bytes, each 0-9 or A-Z — and "" otherwise (AC-F2.1).
// The code is the database engine's own (pgread: the server's error code field), but a server can send any string
// there; only this closed shape reaches StepResult.ObservedSQLState, which the limited report sends. A byte test, so
// no multi-byte character (a Unicode digit or letter) passes.
func observedSQLState(code string) string {
	if len(code) != 5 {
		return ""
	}
	for i := 0; i < len(code); i++ {
		c := code[i]
		if !(c >= '0' && c <= '9') && !(c >= 'A' && c <= 'Z') {
			return ""
		}
	}
	return code
}

// bindParam binds the ${saved.<var>} of one bind parameter. bindSaved is asked first, for its refusals (an
// unsaved variable named, a JSON-saved one misplaced); but it JSON-escapes what it splices because it serves a JSON
// document, and a bind parameter is DATA sent as text, so the stored value is placed as it is.
func bindParam(tmpl string, vars map[string]string) (string, error) {
	if _, err := bindSaved(tmpl, vars); err != nil {
		return "", err
	}
	return savedRefRe.ReplaceAllStringFunc(tmpl, func(m string) string {
		return vars[savedRefRe.FindStringSubmatch(m)[1]]
	}), nil
}

// dbContext bounds ONE attempt: the step's timeout plus a margin for the server's own statement-timeout answer to
// arrive (the server cuts the statement at timeout; a client that gave up at exactly timeout would turn every
// statement timeout into a connection error), and never past the chain's own deadline.
func dbContext(stmtTimeout time.Duration, chainDeadline time.Time) (context.Context, context.CancelFunc) {
	allowed := stmtTimeout + 500*time.Millisecond
	if !chainDeadline.IsZero() {
		if left := time.Until(chainDeadline); left < allowed {
			allowed = left
			if allowed <= 0 {
				allowed = stepGrace
			}
		}
	}
	return context.WithTimeout(context.Background(), allowed)
}

// dbConnectError words a failure to reach the database or get an answer from it: `error`, never retried by a poll
// (a database that cannot be reached is not "still starting up"), with the target's credentials and URL scrubbed.
func dbConnectError(what string, err error, redact func(string) string) report.StepResult {
	msg := err.Error()
	if errors.Is(err, context.DeadlineExceeded) {
		msg = "the deadline passed before an answer"
	}
	return report.StepResult{Status: report.StatusError, Observed: what + ": " + redact(msg)}
}

// dbDocument builds the ONE JSON document the claims are judged against. Columns keep the server's order. A
// duplicate column name is an error naming the column (alias it): a JSON object cannot hold two, and silently
// keeping one would judge a claim on the wrong value. The document is capped at scenario.DBDocumentMaxBytes, as an
// http body is.
func dbDocument(r DBRows) ([]byte, error) {
	seen := map[string]bool{}
	for _, c := range r.Columns {
		if seen[c] {
			return nil, fmt.Errorf("the result has two columns named %s - alias one of them (SELECT a AS a1, …), because a row is a JSON object and cannot hold both", strconv.Quote(c))
		}
		seen[c] = true
	}
	var b bytes.Buffer
	b.WriteString(`{"row_count":`)
	b.WriteString(strconv.Itoa(len(r.Rows)))
	b.WriteString(`,"rows":[`)
	for i, row := range r.Rows {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('{')
		for j, c := range r.Columns {
			if j > 0 {
				b.WriteByte(',')
			}
			k, _ := jsonText(c)
			b.Write(k)
			b.WriteByte(':')
			var cell any
			if j < len(row) {
				cell = row[j]
			}
			v, err := dbCell(cell)
			if err != nil {
				return nil, fmt.Errorf("the value in column %s cannot be placed in the document: %v", strconv.Quote(c), err)
			}
			b.Write(v)
		}
		b.WriteByte('}')
		if b.Len() > scenario.DBDocumentMaxBytes {
			return nil, fmt.Errorf("the rows make a document over the %d-byte limit of what a claim can judge - select fewer columns or rows (LIMIT, count(*))", scenario.DBDocumentMaxBytes)
		}
	}
	b.WriteString(`]}`)
	if b.Len() > scenario.DBDocumentMaxBytes {
		return nil, fmt.Errorf("the rows make a document over the %d-byte limit of what a claim can judge - select fewer columns or rows (LIMIT, count(*))", scenario.DBDocumentMaxBytes)
	}
	return b.Bytes(), nil
}

// jsonText marshals v without HTML escaping, so an unscoped `body contains a&b` sees what the database held.
func jsonText(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}

// dbCell renders one cell as a JSON value. A number keeps every digit: json.Number is written as it came, never
// decoded; only a real float64 goes through a float, and NaN / ±Inf (not JSON numbers) become their text.
func dbCell(v any) ([]byte, error) {
	switch x := v.(type) {
	case nil:
		return []byte("null"), nil
	case bool:
		return []byte(strconv.FormatBool(x)), nil
	case json.Number:
		if !json.Valid([]byte(x)) {
			return jsonText(string(x))
		}
		return []byte(x), nil
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return jsonText(strconv.FormatFloat(x, 'g', -1, 64))
		}
		return jsonText(x)
	case time.Time:
		return jsonText(x.UTC().Format(time.RFC3339Nano))
	case json.RawMessage:
		var c bytes.Buffer
		if json.Compact(&c, x) != nil {
			return jsonText(strings.TrimSpace(string(x)))
		}
		return c.Bytes(), nil
	case string:
		return jsonText(x)
	case []byte:
		return jsonText(string(x))
	default:
		return jsonText(fmt.Sprint(x))
	}
}
