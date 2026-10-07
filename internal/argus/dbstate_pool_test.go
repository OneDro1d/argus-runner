package argus

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The database-state plan runs ONE query, and JMeter opens its whole pool up front
// (DataSourceElement.initPool: setInitialSize(poolMax)). poolMax 5 therefore needed 5 connections
// for one SELECT, and a read-only role with a lower connection limit refused the pool (a hosted dev cluster:
// rolconnlimit 3).
func TestDatabaseStateTemplate_PoolIsOneConnection(t *testing.T) {
	b, err := os.ReadFile("../../templates/database-state.jmx")
	if err != nil {
		t.Fatalf("read template: %v", err)
	}
	m := regexp.MustCompile(`<stringProp name="poolMax">([^<]*)</stringProp>`).FindAllStringSubmatch(string(b), -1)
	if len(m) != 1 {
		t.Fatalf("want exactly one poolMax in database-state.jmx, got %d", len(m))
	}
	// Default 1, overridable (-Jdb.pool.max) for a load run with several threads.
	if m[0][1] != "${__P(db.pool.max,1)}" {
		t.Errorf("database-state.jmx poolMax = %q, want \"${__P(db.pool.max,1)}\": one query needs one connection by default, and JMeter opens the whole pool up front", m[0][1])
	}
}

// A JDBC failure (refused connection or login, permission or syntax error) leaves the sample
// unsuccessful with the exception text as its one-line body. The EXPECT evaluator must say the
// query did not run BEFORE it reads the body as a result set: otherwise it overwrites the
// exception with "query returned no rows", and a refused connection reads as an empty table.
// (has_rows=false stayed red with the raw exception, because the sample was already failed; the
// guard makes both modes say the same thing.)
func TestDatabaseStateTemplate_FailedQueryIsNotReadAsARowSet(t *testing.T) {
	b, err := os.ReadFile("../../templates/database-state.jmx")
	if err != nil {
		t.Fatalf("read template: %v", err)
	}
	s := string(b)
	guard := strings.Index(s, "if (!prev.isSuccessful())")
	if guard < 0 {
		t.Fatal("the EXPECT evaluator does not check that the JDBC sample succeeded")
	}
	if !strings.Contains(s[guard:], `fail("database query did not run (" + prev.getResponseCode() + "): " + why)`) {
		t.Error("a failed JDBC sample must be reported as \"database query did not run (<code>): <error>\"")
	}
	for _, later := range []string{`if (hasRows == "false")`, "def lines = body.split"} {
		i := strings.Index(s, later)
		if i < 0 {
			t.Fatalf("expected %q in the evaluator", later)
		}
		if i < guard {
			t.Errorf("%q runs before the did-the-query-run check, so a JDBC failure is read as a result set", later)
		}
	}
}
