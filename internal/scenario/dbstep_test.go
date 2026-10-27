package scenario

import (
	"encoding/json"
	"strings"
	"testing"
)

// The chain `db` step at the AUTHORING door. Each refusal is asserted by its SENTENCE, because the
// sentence is the interface: it is what an author reads, and the executor's preflight must say the same thing.

const dbOKQuery = `SELECT status FROM orders WHERE id = $1`

// dbTrig builds a chain TRIGGER with one db step named "q". fields is the rest of the step, e.g. `"params":["1"]`
// (leading comma added here), "" for none.
func dbTrig(query, fields string) string {
	q, _ := json.Marshal(query)
	if fields != "" {
		fields = "," + fields
	}
	return `{"steps":[{"type":"db","name":"q","query":` + string(q) + fields + `}]}`
}

const dbOKExpect = "### Runnable\n- step q: body has row_count >= 1\n"

func dbErrs(trigger, expect string) []Error {
	_, errs := Validate(chainMD(trigger, expect))
	return errs
}

func TestValidate_DBStepWellFormedAccepted(t *testing.T) {
	cases := []struct{ name, trigger, expect string }{
		{"query with a param", dbTrig(dbOKQuery, `"params":["42"]`), dbOKExpect},
		{"no ${correlation_id} and no time window are demanded", dbTrig(`SELECT count(*) AS n FROM orders`, ``),
			"### Runnable\n- step q: body has rows.0.n == 3\n"},
		{"the run's own id in the text", dbTrig(`SELECT id FROM orders WHERE note = '${correlation_id}'`, ``), dbOKExpect},
		{"every option", dbTrig(dbOKQuery, `"params":["${correlation_id}"],"target":"ledger","timeout":"30s","max_rows":1000,`+
			`"always":true,"save":{"st":"rows.0.status"},"poll":{"timeout":"10m","interval":"1s"}`), dbOKExpect},
		{"a claim indexing a row and a column", dbTrig(dbOKQuery, `"params":["1"]`),
			"### Runnable\n- step q: body has rows.0.status equals PAID\n- step q: body every rows has status not equals FAILED\n"},
		{"a CTE", dbTrig(`WITH x AS (SELECT id FROM orders) SELECT id FROM x`, ``), dbOKExpect},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if errs := dbErrs(c.trigger, c.expect); len(errs) != 0 {
				t.Fatalf("a well-formed db step must be accepted, got %v", errs)
			}
		})
	}
}

func TestValidate_DBStepNoClaimRefused(t *testing.T) {
	errs := dbErrs(dbTrig(dbOKQuery, `"params":["1"]`), "### Runnable\n")
	if !find(errs, `step "q" has no claim`) || !find(errs, "`db` step needs at least one") || !find(errs, "- step q: body has row_count >= 1") {
		t.Fatalf("a db step with no claim must be refused with the minimum claim named; got %v", errs)
	}
}

func TestValidate_DBStepQueryRules(t *testing.T) {
	cases := []struct{ name, query, want string }{
		{"no query", ``, "this db step declares no `query`"},
		{"a second statement", `SELECT id FROM orders; DELETE FROM orders`, "the db step's query contains more than one statement"},
		{"begins with a write", `DELETE FROM orders WHERE id = 1`, "a db step's query must begin with `SELECT` or `WITH`"},
		{"a writing CTE", `WITH x AS (INSERT INTO orders (id) VALUES (1) RETURNING id) SELECT id FROM x`, "the db step's query contains `INSERT`"},
		{"an update inside", `SELECT id FROM orders WHERE 1=1 UPDATE orders SET a = 1`, "the db step's query contains `UPDATE`"},
		{"no FROM", `SELECT 1`, "the db step's query has no `FROM`"},
		{"unbalanced", `SELECT id FROM orders WHERE (id = 1`, "the db step's query has unbalanced parentheses"},
		{"unterminated literal", `SELECT id FROM orders WHERE note = 'x`, "the db step's query has an unterminated string literal"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			errs := dbErrs(dbTrig(c.query, ``), dbOKExpect)
			if !find(errs, c.want) {
				t.Fatalf("want a refusal saying %q, got %v", c.want, errs)
			}
			for _, e := range errs {
				if strings.Contains(e.Message, "VERIFY") {
					t.Fatalf("a db step's refusal must say \"the db step's query\", never the VERIFY query: %q", e.Message)
				}
			}
		})
	}
}

// A keyword inside a string literal or a comment is not a write; a column called updated_at is not UPDATE.
func TestValidate_DBStepQueryMaskingKeepsTheVerifyRules(t *testing.T) {
	q := "SELECT id, updated_at FROM orders WHERE note = 'delete me' -- insert here\n AND id = $1"
	if errs := dbErrs(dbTrig(q, `"params":["1"]`), dbOKExpect); len(errs) != 0 {
		t.Fatalf("masked literals and comments must not be read as writes, got %v", errs)
	}
}

func TestValidate_DBStepSavedValueInQueryRefusedByName(t *testing.T) {
	errs := dbErrs(dbTrig(`SELECT status FROM orders WHERE id = '${saved.oid}'`, ``), dbOKExpect)
	if !find(errs, "carries ${saved.oid}") || !find(errs, "must never be spliced into SQL text") || !find(errs, "pass the value through `params`") {
		t.Fatalf("a saved value in the query text must be refused with the sentence that sends the author to params; got %v", errs)
	}
}

func TestValidate_DBStepForeignPlaceholderInQueryRefused(t *testing.T) {
	errs := dbErrs(dbTrig(`SELECT status FROM ${TABLE} WHERE id = $1`, `"params":["1"]`), dbOKExpect)
	if !find(errs, "carries `${TABLE}`, which nothing fills in") {
		t.Fatalf("a placeholder nothing fills in must be refused in the query; got %v", errs)
	}
}

func TestValidate_DBStepParamRules(t *testing.T) {
	cases := []struct{ name, query, fields, want string }{
		{"more params than placeholders", dbOKQuery, `"params":["1","2"]`, "uses 1 numbered placeholder(s) (highest $1) but `params` passes 2 value(s)"},
		{"fewer params than placeholders", `SELECT id FROM orders WHERE a = $1 AND b = $2`, `"params":["1"]`, "highest $2) but `params` passes 1 value(s)"},
		{"no params for a placeholder", dbOKQuery, ``, "highest $1) but `params` passes 0 value(s)"},
		{"a gap in the numbering", `SELECT id FROM orders WHERE a = $2`, `"params":["1"]`, "must use exactly $1..$1"},
		{"a param nothing uses", `SELECT id FROM orders`, `"params":["1"]`, "but `params` passes 1 value(s)"},
		{"a non-string param", dbOKQuery, `"params":[42]`, "`params` element 1 is not a string"},
		{"params that is not an array", dbOKQuery, `"params":"1"`, "`params` must be a JSON array of strings"},
		{"a foreign placeholder in a param", dbOKQuery, `"params":["${TOKEN}"]`, "`params` element 1 carries `${TOKEN}`"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			errs := dbErrs(dbTrig(c.query, c.fields), dbOKExpect)
			if !find(errs, c.want) {
				t.Fatalf("want a refusal saying %q, got %v", c.want, errs)
			}
		})
	}
	// a placeholder inside a string literal is text, not a bind position
	if errs := dbErrs(dbTrig(`SELECT id FROM orders WHERE note = '$1 off'`, ``), dbOKExpect); len(errs) != 0 {
		t.Fatalf("a $1 inside a literal is not a placeholder, got %v", errs)
	}
}

func TestValidate_DBStepSavedParamAllowedJSONSavedParamRefused(t *testing.T) {
	two := `{"steps":[{"type":"http","name":"mk","method":"POST","url":"http://sut.invalid/o","save":{"oid":"id"}},` +
		`{"type":"db","name":"q","query":"SELECT status FROM orders WHERE id = $1","params":["${saved.oid}"]}]}`
	expect := "### Runnable\n- step mk: status=200\n- step q: body has row_count >= 1\n"
	if errs := dbErrs(two, expect); len(errs) != 0 {
		t.Fatalf("a text-saved value in a param is the intended use, got %v", errs)
	}
	asJSON := strings.Replace(two, `"save":{"oid":"id"}`, `"save":{"oid":{"json":"id"}}`, 1)
	errs := dbErrs(asJSON, expect)
	if !find(errs, "the db step's param $1 uses ${saved.oid}, which was saved with the {\"json\": …} form") {
		t.Fatalf("a JSON-saved variable in a param must be refused by name; got %v", errs)
	}
}

func TestValidate_DBStepBounds(t *testing.T) {
	cases := []struct{ name, fields, want string }{
		{"timeout not a duration", `"timeout":"soon"`, "`timeout` \"soon\" is not a positive Go duration"},
		{"timeout zero", `"timeout":"0s"`, "is not a positive Go duration"},
		{"timeout over 30s", `"timeout":"31s"`, "`timeout` of 31s is more than the 30s bound"},
		{"max_rows zero", `"max_rows":0`, "`max_rows` is 0, outside 1..1000"},
		{"max_rows over 1000", `"max_rows":1001`, "`max_rows` is 1001, outside 1..1000"},
		{"poll interval under 1s", `"poll":{"timeout":"30s","interval":"500ms"}`, "`poll.interval` \"500ms\", which is under 1s - a check must not hammer a database"},
		{"poll timeout over 10m", `"poll":{"timeout":"11m","interval":"5s"}`, "`poll.timeout` of 11m0s, which is more than the 10m bound"},
		{"poll timeout invalid", `"poll":{"timeout":"x","interval":"5s"}`, "`poll.timeout` \"x\", which is not a valid duration"},
		{"poll interval invalid", `"poll":{"timeout":"30s","interval":"x"}`, "`poll.interval` \"x\", which is not a valid duration"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			errs := dbErrs(dbTrig(dbOKQuery, `"params":["1"],`+c.fields), dbOKExpect)
			if !find(errs, c.want) {
				t.Fatalf("want a refusal saying %q, got %v", c.want, errs)
			}
		})
	}
}

func TestValidate_DBStepPollWithoutClaimRefused(t *testing.T) {
	errs := dbErrs(dbTrig(dbOKQuery, `"params":["1"],"poll":{"timeout":"30s","interval":"2s"}`), "### Runnable\n")
	if !find(errs, `step "q" has no claim`) {
		t.Fatalf("a polling db step with no claim must be refused; got %v", errs)
	}
}

func TestValidate_DBStepStrayFieldsRefusedByName(t *testing.T) {
	for _, k := range []string{`"method":"GET"`, `"url":"http://x"`, `"tool":"t"`, `"op":"publish"`, `"headers":{}`, `"body":{}`, `"args":{}`} {
		errs := dbErrs(dbTrig(dbOKQuery, `"params":["1"],`+k), dbOKExpect)
		name := strings.Trim(strings.SplitN(k, ":", 2)[0], `"`)
		if !find(errs, `this db step carries the field "`+name+`", which a db step does not accept`) {
			t.Errorf("the field %s on a db step must be refused by name; got %v", k, errs)
		}
	}
}

func TestValidate_DBStepOtherStepKindsClaimsRefusedByName(t *testing.T) {
	cases := []struct{ claim, want string }{
		{"status=200", "is a status claim - a db step has no HTTP status"},
		{"elapsed < 500ms", "`elapsed` is a claim for an http step only"},
		{"unreachable", "`unreachable` is a claim for an http step only"},
		{"broker accepts", "its claim \"broker accepts\" is a broker claim"},
		{"result.isError == false", "is not a recognised db-step assertion"},
	}
	for _, c := range cases {
		t.Run(c.claim, func(t *testing.T) {
			errs := dbErrs(dbTrig(dbOKQuery, `"params":["1"]`), "### Runnable\n- step q: body has row_count >= 1\n- step q: "+c.claim+"\n")
			if !find(errs, c.want) {
				t.Fatalf("want a refusal saying %q, got %v", c.want, errs)
			}
		})
	}
}

// Door A learned to refuse an unknown step type in; the SAME function words it at the executor.
func TestValidate_UnknownStepTypeRefusedAtAuthoring(t *testing.T) {
	trig := `{"steps":[{"type":"sql","name":"q","query":"SELECT 1 FROM t"}]}`
	errs := dbErrs(trig, "### Runnable\n")
	want := UnknownStepTypeProblem("sql")
	if want == "" || !find(errs, want) {
		t.Fatalf("an unknown step type must be refused with %q; got %v", want, errs)
	}
	if got := UnknownStepTypeProblem("db"); got != "" {
		t.Fatalf("db is a known step type, got %q", got)
	}
	for _, typ := range ChainStepTypes {
		if UnknownStepTypeProblem(typ) != "" {
			t.Fatalf("%s is listed in ChainStepTypes and must be known", typ)
		}
	}
	if !strings.Contains(want, "(one of mcp, ui, http, amqp, fault, db, logs, bus)") {
		t.Fatalf("the sentence must list the known types, got %q", want)
	}
}

// The shared SQL entry point: the read-only subset is a subset of the full rules, and VERIFY wording is unchanged.
func TestDBStepReadOnlySubsetOfFullRules(t *testing.T) {
	q := `WITH x AS (DELETE FROM t RETURNING id) SELECT id FROM x; SELECT 1`
	full, ro := DBStepQueryErrors(q), DBStepReadOnlyViolations(q)
	if len(ro) == 0 || len(ro) > len(full) {
		t.Fatalf("read-only %v must be a non-empty subset of full %v", ro, full)
	}
	in := map[string]bool{}
	for _, f := range full {
		in[f] = true
	}
	for _, r := range ro {
		if !in[r] {
			t.Fatalf("read-only violation %q is not among the full rules %v", r, full)
		}
	}
	if got := VerifySQLErrors("DELETE FROM orders", nil, true); len(got) == 0 || !strings.Contains(got[0], "VERIFY") {
		t.Fatalf("the VERIFY wording must be untouched, got %v", got)
	}
}
