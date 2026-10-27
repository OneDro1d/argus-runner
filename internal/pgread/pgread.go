// Package pgread is the executor's one client of a PostgreSQL system under test: it opens a connection for a chain
// `db` step and runs ONE read-only SELECT on it.
//
// ⛔ IT IS THE ONLY FILE ON THE EXECUTOR SIDE THAT IMPORTS pgx. No executor code opened a SQL connection to a
// system under test before this: `## VERIFY` runs through JMeter's JDBC sampler. The control plane uses pgx for its
// own store, and that is a different binary's business.
//
// ⛔ THE THIRD LOCK. The authoring validator and the executor's preflight refuse a query that could write
// (scenario.DBStepQueryErrors / DBStepReadOnlyViolations). This file refuses it a third time, AT THE SERVER:
// every statement runs inside BEGIN READ ONLY, with the statement timeout and the session time zone set for that
// transaction only, and the transaction is ALWAYS rolled back, passed or not. A write that was smuggled past the
// first two (a data-modifying CTE is the one the keyword scan can miss in a dialect it does not parse) is stopped by
// PostgreSQL itself with SQLSTATE 25006. The role the target connects as is still the real guarantee; the operator
// should make it a read-only role.
package pgread

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"encoding/json"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/OneDro1d/argus-runner/internal/chain"
)

// Dial is the production chain.DBDialer.
//
// ⚠ pgx reads the PG* environment variables for whatever the settings below leave unset. sslmode is therefore
// always written out (prefer, pgjdbc's own default, when the URL names none), and the executor's environment is
// not expected to carry PG* variables.
func Dial(ctx context.Context, c chain.DBConn) (chain.DBQuerier, error) {
	u := url.URL{Scheme: "postgres", Host: net.JoinHostPort(c.Host, strconv.Itoa(c.Port)), Path: "/" + c.Database}
	u.User = url.UserPassword(c.User, c.Password)
	q := url.Values{}
	sslmode := c.SSLMode
	if sslmode == "" {
		sslmode = "prefer"
	}
	q.Set("sslmode", sslmode)
	if c.AppName != "" {
		q.Set("application_name", c.AppName)
	}
	if c.Schema != "" {
		q.Set("search_path", c.Schema) // a runtime parameter: sent in the startup message, where the schema must be
	}
	u.RawQuery = q.Encode()
	cfg, err := pgx.ParseConfig(u.String())
	if err != nil {
		// pgx's own message quotes the connection string; ours must not.
		return nil, errors.New("the target's connection settings could not be read")
	}
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, connectError(err)
	}
	return &querier{conn: conn}, nil
}

// connectError words a failed connection WITHOUT the connection string pgx puts in its message (host, user and
// database in backquotes): the server's own refusal (an authentication failure, say) as `SQLSTATE: message`, else
// the underlying cause.
func connectError(err error) error {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return fmt.Errorf("%s: %s", pe.Code, pe.Message)
	}
	var ce *pgconn.ConnectError
	if errors.As(err, &ce) {
		if inner := ce.Unwrap(); inner != nil {
			return inner
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return errors.New("the connection could not be made")
}

type querier struct {
	conn *pgx.Conn
}

func (q *querier) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return q.conn.Close(ctx)
}

// Query runs one statement in a fresh read-only transaction (see the package comment) and reads at most limit rows.
// Parameters are bound as $1..$n and sent as text with an UNKNOWN type, so the server infers each one's type from
// where it stands (`WHERE id = $1` makes it the id's type): that is what lets a saved id travel as a string.
// Results come back in text format, so a number reaches the caller with every digit the server wrote.
//
// ⛔ CONTRACT: after a read that returned limit rows the connection is SPENT and must not be used again - Query
// cancels the statement once it has limit rows, and pgx closes the connection on a cancelled query. A later Query on
// it returns a non-*chain.DBServerError error ("the connection to the database failed"). The caller closes it.
func (q *querier) Query(ctx context.Context, query string, params []string, statementTimeout time.Duration, limit int) (chain.DBRows, error) {
	tx, err := q.conn.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return chain.DBRows{}, classify(err)
	}
	defer func() {
		rctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = tx.Rollback(rctx) // always: nothing a db step does is ever committed
	}()

	// The statement timeout, and the time zone and date style the typing table relies on, for THIS transaction only.
	// The number is built from a time.Duration, never from the scenario.
	ms := statementTimeout.Milliseconds()
	if ms < 1 {
		ms = 1
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL statement_timeout = %d; SET LOCAL TimeZone = 'UTC'; SET LOCAL DateStyle = 'ISO, YMD'", ms),
		pgx.QueryExecModeSimpleProtocol); err != nil {
		return chain.DBRows{}, classify(err)
	}

	args := make([]any, 0, len(params)+2)
	args = append(args, pgx.QueryExecModeExec, pgx.QueryResultFormats{pgx.TextFormatCode})
	for _, p := range params {
		args = append(args, p)
	}
	qctx, cancelQuery := context.WithCancel(ctx)
	defer cancelQuery()
	rows, err := tx.Query(qctx, query, args...)
	if err != nil {
		return chain.DBRows{}, classify(err)
	}
	defer rows.Close()

	fds := rows.FieldDescriptions()
	out := chain.DBRows{Columns: make([]string, len(fds))}
	oids := make([]uint32, len(fds))
	for i, fd := range fds {
		out.Columns[i] = fd.Name
		oids[i] = fd.DataTypeOID
	}
	for rows.Next() {
		raw := rows.RawValues()
		row := make([]any, len(raw))
		for i, b := range raw {
			row[i] = cell(oids[i], b)
		}
		out.Rows = append(out.Rows, row)
		if limit > 0 && len(out.Rows) >= limit {
			// Enough to know whether the cap was hit. Stop here instead of draining what the server still has:
			// cancelling kills this connection's query, which is fine, the step closes the connection next.
			cancelQuery()
			return out, nil
		}
	}
	if err := rows.Err(); err != nil {
		return chain.DBRows{}, classify(err)
	}
	return out, nil
}

// classify separates the server REFUSING a statement (a *chain.DBServerError: it did not run) from everything else
// (the connection, the deadline).
func classify(err error) error {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return &chain.DBServerError{SQLState: pe.Code, Message: pe.Message}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return errors.New("the connection to the database failed")
}

// cell types one text-format value by its column's type OID. NULL is nil. Integers and numeric are json.Number with
// the server's own digits (never a float64); float4/float8 are floats; bool; timestamp, timestamptz and date are
// time.Time (the session is UTC); json and jsonb are the JSON value itself; every other type is the server's text
// form as a string (uuid, text, varchar, bytea as \x…, interval, arrays, …).
func cell(oid uint32, raw []byte) any {
	if raw == nil {
		return nil
	}
	t := string(raw)
	switch oid {
	case pgtype.Int2OID, pgtype.Int4OID, pgtype.Int8OID, pgtype.OIDOID, pgtype.NumericOID:
		return json.Number(t) // "NaN" / "Infinity" are not JSON numbers: the document builder keeps those as text
	case pgtype.Float4OID, pgtype.Float8OID:
		if f, err := strconv.ParseFloat(t, 64); err == nil {
			return f
		}
		return t
	case pgtype.BoolOID:
		return t == "t"
	case pgtype.JSONOID, pgtype.JSONBOID:
		return json.RawMessage(t)
	case pgtype.TimestamptzOID:
		for _, layout := range []string{"2006-01-02 15:04:05.999999999Z07:00", "2006-01-02 15:04:05.999999999Z07"} {
			if v, err := time.Parse(layout, t); err == nil {
				return v.UTC()
			}
		}
	case pgtype.TimestampOID:
		if v, err := time.Parse("2006-01-02 15:04:05.999999999", t); err == nil {
			return v.UTC()
		}
	case pgtype.DateOID:
		if v, err := time.Parse("2006-01-02", t); err == nil {
			return v.UTC()
		}
	}
	return strings.Clone(t)
}
