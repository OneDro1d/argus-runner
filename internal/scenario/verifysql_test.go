package scenario

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// VR12-VF4..VF13 — the nine write-time SQL rules.

func TestVerifySQL_TheNineRules(t *testing.T) {
	const ok = "SELECT status FROM orders WHERE correlation_id = '${correlation_id}'"
	cases := []struct {
		rule, name, query string
		cols              []string
		want              string // "" = must be accepted
	}{
		{"—", "the control", ok, nil, ""},
		{"VF4", "a DELETE", "DELETE FROM orders WHERE correlation_id = '${correlation_id}'", nil, "contains `DELETE`"},
		{"VF4", "an UPDATE smuggled into a subquery", "SELECT * FROM orders WHERE id IN (UPDATE x SET y=1 RETURNING id) AND correlation_id = '${correlation_id}'", nil, "contains `UPDATE`"},
		{"VF5", "not scoped to the run", "SELECT status FROM orders", nil, "must contain `${correlation_id}`"},
		{"VF6", "asserts a column the query does not SELECT", ok, []string{"tenant_id"}, "asserts on the column `tenant_id`"},
		{"VF6", "…and one it does", ok, []string{"status"}, ""},
		{"VF6", "SELECT * covers everything", "SELECT * FROM orders WHERE correlation_id = '${correlation_id}'", []string{"anything"}, ""},
		{"VF7", "a tautology", "SELECT 1 WHERE '${correlation_id}' <> ''", nil, "has no `FROM`"},
		{"VF8", "two statements", "SELECT 1 FROM orders WHERE correlation_id = '${correlation_id}'; DROP TABLE orders", nil, "more than one statement"},
		{"VF8", "…but a single trailing semicolon is fine", ok + ";", nil, ""},
		{"VF9", "does not begin with SELECT or WITH", "EXPLAIN SELECT * FROM orders WHERE correlation_id = '${correlation_id}'", nil, "must begin with `SELECT` or `WITH`"},
		{"VF9", "a CTE is legitimate", "WITH r AS (SELECT * FROM orders WHERE correlation_id = '${correlation_id}') SELECT count(*) FROM r", nil, ""},
		{"VF9", "unbalanced parentheses", "SELECT count(* FROM orders WHERE correlation_id = '${correlation_id}'", nil, "unbalanced parentheses"},
		{"VF11", "another placeholder reaches the database verbatim", "SELECT * FROM orders WHERE tenant = '${TENANT}' AND correlation_id = '${correlation_id}'", nil, "`${TENANT}` is not substituted"},
	}
	for _, c := range cases {
		t.Run(c.rule+": "+c.name, func(t *testing.T) {
			got := VerifySQLErrors(c.query, c.cols, false)
			if c.want == "" {
				if len(got) > 0 {
					t.Fatalf("must be ACCEPTED, got %v", got)
				}
				return
			}
			for _, g := range got {
				if strings.Contains(g, c.want) {
					return
				}
			}
			t.Fatalf("want a refusal containing %q; got %v", c.want, got)
		})
	}
}

// ⛔ VF12 — THE WORD-BOUNDARY RULE, AND ITS FALSE-POSITIVE GUARD.
//
// This is the case that decides whether the rule is usable at all. A naive case-insensitive
// SUBSTRING scan falsely rejects TWO shipped queries: `create` matches `created_at` in DB-005 and
// DB-006, and `update` matches `updated_at` in DB-006. A rule that refuses two correct scenarios on
// its first day is worse than no rule.
func TestVerifySQL_WordBoundariesAndMasking(t *testing.T) {
	accept := []struct{ why, q string }{
		{"created_at is not CREATE",
			"SELECT created_at FROM orders WHERE correlation_id = '${correlation_id}'"},
		{"updated_at is not UPDATE",
			"SELECT updated_at, created_at FROM orders WHERE correlation_id = '${correlation_id}'"},
		{"a table name containing a keyword",
			"SELECT id FROM orders_delete_log WHERE correlation_id = '${correlation_id}'"},
		{"a keyword inside a STRING LITERAL",
			"SELECT id FROM orders WHERE note = 'delete me later' AND correlation_id = '${correlation_id}'"},
		{"a keyword inside a LINE COMMENT",
			"SELECT id FROM orders -- update this when the schema changes\nWHERE correlation_id = '${correlation_id}'"},
		{"a keyword inside a BLOCK COMMENT",
			"SELECT id /* do not DROP this index */ FROM orders WHERE correlation_id = '${correlation_id}'"},
		// ⭐ THE TWO POSTGRES-ONLY QUERIES THAT ARE THE ACCEPTANCE TEST FOR ANY PARSER CHOICE. A
		// generic or MySQL-flavoured parser reds both, and the example-suite gate goes red on day one.
		{"ORDE-007's regex operator (PostgreSQL-only)",
			"SELECT idempotency_key FROM orders WHERE idempotency_key ~ '^01[A-Z0-9]+$' AND correlation_id = '${correlation_id}'"},
		{"DB-005's interval arithmetic (PostgreSQL-only)",
			"SELECT created_at FROM orders WHERE created_at > now() - interval '2 minutes' AND correlation_id = '${correlation_id}'"},
	}
	for _, c := range accept {
		if got := VerifySQLErrors(c.q, nil, false); len(got) > 0 {
			t.Errorf("%s: must be ACCEPTED, got %v", c.why, got)
		}
	}
	// …and the real thing is still caught.
	if got := VerifySQLErrors("SELECT id FROM orders WHERE correlation_id = '${correlation_id}'; DELETE FROM orders", nil, false); len(got) == 0 {
		t.Error("a real mutation must still be refused")
	}
}

// ⛔ VF13 — NO SQL PARSER MAY BE QUIETLY ADDED. The rule "no parser, no dialect" is not a preference
// a later change can drop by adding a dependency; it is asserted on go.mod itself.
//
// The reasons are in verifysql.go: the product is multi-dialect BY ITS OWN CODE (it parses
// `jdbc:oracle:` as well as `jdbc:postgresql:`), Validate takes markdown alone so it cannot detect
// the engine, two shipped queries are PostgreSQL-only, and the obvious library is CGO — which
// breaks the amd64+arm64 execution-plane publish.
func TestVF13_NoSQLParserDependency(t *testing.T) {
	b, err := os.ReadFile(filepath.FromSlash("../../go.mod"))
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	mod := strings.ToLower(string(b))
	for _, banned := range []string{
		"pg_query_go", "libpg_query", "vitess", "sqlparser", "xwb1989/sqlparser",
		"auxten/postgresql-parser", "cockroachdb/cockroach", "antlr",
	} {
		if strings.Contains(mod, banned) {
			t.Errorf("go.mod carries %q — VF9 is a STRUCTURAL grammar on purpose: the product is "+
				"multi-dialect by its own code, two shipped queries are PostgreSQL-only, and the "+
				"obvious parser is CGO (which breaks the multi-arch publish). If a parser is genuinely "+
				"wanted, it is its own row with its own evidence — not a quiet dependency (VF13)", banned)
		}
	}
}
