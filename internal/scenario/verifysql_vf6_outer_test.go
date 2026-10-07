package scenario

import (
	"strings"
	"testing"
)

// VF6 reads the OUTERMOST select list. Measured 2026-09-17 on a shipped scenario: a select list
// that opens with a sub-select was read only up to the sub-select's FROM, so the rule refused a
// correct query whose aliases it never reached — and, on the older shape of the same query, said
// nothing at all because the sub-select's `count(*)` looked like `SELECT *`. These cases pin both.
func TestVF6_ReadsTheOutermostSelectList(t *testing.T) {
	corr := "WHERE p.last_correlation_id = '${correlation_id}'"
	cases := []struct {
		name    string
		query   string
		columns []string
		refused []string // columns VF6 must name; nil = must pass
	}{
		{
			"aliases after a leading sub-select are seen (the shipped shape)",
			"SELECT (SELECT count(DISTINCT hc.hub_id) FROM hub_connections hc WHERE hc.hub_id::text IN (p.a, p.b)) AS hubs,\n" +
				"       (SELECT count(*) FROM user_service_tokens t WHERE t.hub_id::text IN (p.a, p.b)) AS tokens,\n" +
				"       (CASE WHEN p.status = 'complete' THEN 1 ELSE 0 END) AS complete\nFROM argus_provisioning p\n" + corr,
			[]string{"hubs", "tokens", "complete"},
			nil,
		},
		{
			"count(*) inside a sub-select is NOT `SELECT *` — a missing alias is still refused",
			"SELECT (SELECT count(*) FROM hubs h WHERE h.id IN (p.a, p.b)) AS hubs FROM argus_provisioning p " + corr,
			[]string{"hubs", "connections"},
			[]string{"connections"},
		},
		{
			"a bare * in the outer list still covers everything",
			"SELECT o.* FROM orders o " + strings.Replace(corr, "p.last_correlation_id", "o.correlation_id", 1),
			[]string{"anything"},
			nil,
		},
		{
			"count(*) as the whole outer list selects one column, its alias",
			"SELECT count(*) AS n FROM orders o " + strings.Replace(corr, "p.last_correlation_id", "o.correlation_id", 1),
			[]string{"n", "status"},
			[]string{"status"},
		},
		{
			"a CTE is not read, so nothing is refused",
			"WITH x AS (SELECT 1 AS a FROM t) SELECT a FROM x " + strings.Replace(corr, "p.last_correlation_id", "x.correlation_id", 1),
			[]string{"b"},
			nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var vf6 []string
			for _, e := range VerifySQLErrors(c.query, c.columns, false) {
				if strings.Contains(e, "(VF6)") {
					vf6 = append(vf6, e)
				}
			}
			if c.refused == nil {
				if len(vf6) != 0 {
					t.Fatalf("VF6 refused a correct query:\n%s", strings.Join(vf6, "\n"))
				}
				return
			}
			if len(vf6) != len(c.refused) {
				t.Fatalf("VF6 refusals = %d, want %d:\n%s", len(vf6), len(c.refused), strings.Join(vf6, "\n"))
			}
			for i, col := range c.refused {
				if !strings.Contains(vf6[i], "`"+col+"`") {
					t.Fatalf("refusal %d names the wrong column:\n%s\nwant `%s`", i, vf6[i], col)
				}
			}
		})
	}
}

func TestVF6_OuterSelectListStopsAtDepthZero(t *testing.T) {
	list, ok := outerSelectList("SELECT (SELECT 1 FROM a) AS x, b FROM t")
	if !ok || strings.TrimSpace(list) != "(SELECT 1 FROM a) AS x, b" {
		t.Fatalf("outerSelectList = %q, %v", list, ok)
	}
	if _, ok := outerSelectList("WITH c AS (SELECT 1 FROM a) SELECT * FROM c"); ok {
		t.Fatal("a CTE must not be read as an outer select list")
	}
	if selectsEverything("count(*) AS n") {
		t.Fatal("count(*) is not SELECT *")
	}
	if !selectsEverything("o.*, b") {
		t.Fatal("o.* is SELECT *")
	}
}
