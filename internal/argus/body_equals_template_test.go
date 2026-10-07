package argus

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"html"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/mcp"
)

// — `body has perPage equals 1` PASSED while perPage was 10, on the JMeter path.
//
// The grammar defines `equals` (scenario.BodyEquals) and the Go judge evaluates it EXACTLY
// (mcp.bodyAssertMiss: `target != a.Value`), but templates/http-ingestion.jmx switched on op with
// `exists`, `matches` and an else that was `target.contains(v)` — so `equals` was silently judged as
// `contains`. These tests run the template's REAL assertion script under Groovy (extracted from the
// .jmx, nothing re-implemented) and, for `equals`, compare it case by case to the Go judge
// (mcp.BodyAssertsMiss) used as the ORACLE: the two planes must agree on every case.

type bodyAssertVerdict struct {
	Failure bool   `json:"failure"`
	Message string `json:"message"`
}

// httpIngestionBodyAssertScript extracts the "enforce body assertion (DF-04)" script from the template.
func httpIngestionBodyAssertScript(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("../../templates/http-ingestion.jmx")
	if err != nil {
		t.Fatalf("read template: %v", err)
	}
	type node struct {
		XMLName xml.Name
		Attrs   []xml.Attr `xml:",any,attr"`
		Nodes   []node     `xml:",any"`
		Text    string     `xml:",chardata"`
	}
	var root node
	if err := xml.Unmarshal(b, &root); err != nil {
		t.Fatalf("parse template: %v", err)
	}
	var found string
	var walk func(n node)
	walk = func(n node) {
		if n.XMLName.Local == "JSR223Assertion" {
			for _, a := range n.Attrs {
				if a.Name.Local == "testname" && strings.HasPrefix(a.Value, "enforce body assertion") {
					for _, c := range n.Nodes {
						for _, ca := range c.Attrs {
							if ca.Name.Local == "name" && ca.Value == "script" {
								found = html.UnescapeString(c.Text)
							}
						}
					}
				}
			}
		}
		for _, c := range n.Nodes {
			walk(c)
		}
	}
	walk(root)
	if found == "" {
		t.Fatal("no `enforce body assertion` JSR223 assertion found in templates/http-ingestion.jmx")
	}
	return found
}

// runBodyAssert runs ONE assertion (field, op, value) against body through the template's script.
func runBodyAssert(t *testing.T, field, op, value, body string) bodyAssertVerdict {
	t.Helper()
	cp := groovyClasspath(t)
	props := map[string]string{
		"expect.body.count":   "1",
		"expect.body.1.field": field,
		"expect.body.1.op":    op,
		"expect.body.1.value": value,
	}
	in, _ := json.Marshal(map[string]any{"script": httpIngestionBodyAssertScript(t), "props": props, "body": body, "code": "200"})
	harness, _ := filepath.Abs("testdata/body_assert_harness.groovy")
	cmd := exec.Command("java", "-cp", cp, "groovy.ui.GroovyMain", harness)
	cmd.Stdin = bytes.NewReader(in)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("groovy harness failed: %v\n%s\n%s", err, out.String(), errb.String())
	}
	last := ""
	for _, ln := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(strings.TrimSpace(ln), "{") {
			last = ln
		}
	}
	var v bodyAssertVerdict
	if err := json.Unmarshal([]byte(last), &v); err != nil {
		t.Fatalf("harness output unreadable: %v\n%s\n%s", err, out.String(), errb.String())
	}
	return v
}

// The live defect, pinned by name: perPage was 10, the scenario said equals 1, the run PASSED.
func TestHTTPIngestionTemplate_EqualsIsExact_LiveDefect(t *testing.T) {
	v := runBodyAssert(t, "perPage", "equals", "1", `{"perPage":10,"page":1}`)
	if !v.Failure {
		t.Fatal(" `body has perPage equals 1` PASSED against perPage=10 — equals was judged as contains")
	}
	if !strings.Contains(v.Message, "BODY-ASSERT-FAIL") {
		t.Errorf("failure must carry the BODY-ASSERT-FAIL marker the runner parses, got %q", v.Message)
	}
	if ok := runBodyAssert(t, "perPage", "equals", "10", `{"perPage":10,"page":1}`); ok.Failure {
		t.Errorf("equals 10 against perPage=10 must PASS, got failure %q", ok.Message)
	}
}

// Parity with the Go judge, case by case. The oracle is mcp.BodyAssertsMiss — the same function the
// chain `http` step and the MCP plane use — so a divergence here is the two planes disagreeing.
func TestHTTPIngestionTemplate_EqualsMatchesTheGoJudge(t *testing.T) {
	cases := []struct {
		name, field, value, body string
	}{
		{"number: a prefix of the value is not equal", "n", "1", `{"n":10}`},
		{"number: a suffix is not equal", "n", "0", `{"n":10}`},
		{"number: exact", "n", "10", `{"n":10}`},
		{"number: 1.0 renders as 1 (float64)", "n", "1", `{"n":1.0}`},
		{"number: value 1.0 is not the text 1", "n", "1.0", `{"n":1}`},
		{"number: fraction exact", "n", "1.5", `{"n":1.5}`},
		{"number: trailing zero in the body is dropped", "n", "1.5", `{"n":1.50}`},
		{"number: negative", "n", "-3", `{"n":-3}`},
		// beyond 2^53 the Go judge reads float64 and so does the template. (Beyond int64 Groovy 3's
		// JsonSlurper overflows before we see the value - a parser limit recorded in.)
		{"number: an integer past 2^53 goes through float64", "n", "9007199254740992", `{"n":9007199254740993}`},
		{"number: 1e20 stays plain", "n", "100000000000000000000", `{"n":1e20}`},
		{"number: 1e21 switches to exponent form", "n", "1e+21", `{"n":1e21}`},
		{"number: 1.5e-7 switches to exponent form", "n", "1.5e-7", `{"n":0.00000015}`},
		{"number: 0.000001 stays plain", "n", "0.000001", `{"n":0.000001}`},
		{"number: exponent form is normalised", "n", "100", `{"n":1e2}`},
		{"number: zero", "n", "0", `{"n":0}`},
		{"string: exact", "s", "abc", `{"s":"abc"}`},
		{"string: substring is not equal", "s", "ab", `{"s":"abc"}`},
		{"string: extra text either side is a miss", "s", "abc", `{"s":" abc "}`},
		{"string: empty", "s", "", `{"s":""}`},
		{"bool: true", "b", "true", `{"b":true}`},
		{"bool: wrong", "b", "false", `{"b":true}`},
		{"nested path", "a.b.c", "x", `{"a":{"b":{"c":"x"}}}`},
		{"array index", "items.1", "two", `{"items":["one","two"]}`},
		{"field absent is a miss", "nope", "1", `{"n":1}`},
		{"JSON null is a miss", "n", "null", `{"n":null}`},
		{"not JSON is a miss (R4)", "n", "1", `plain text`},
		{"object: keys are compared sorted", "o", `{"a":2,"b":1}`, `{"o":{"b":1,"a":2}}`},
		{"object: Go escapes < in a nested string", "o", `{"s":"a` + "\\" + `u003cb"}`, `{"o":{"s":"a<b"}}`},
		{"array: compact", "l", `[1,2]`, `{"l":[1, 2]}`},
		{"whole body: exact", "", "hello world", `hello world`},
		{"whole body: a substring is not equal", "", "hello", `hello world`},
		{"whole body: extra text is a miss", "", "hello world", `hello world!`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wantMiss := mcp.BodyAssertsMiss(c.body, []mcp.BodyAssert{{Field: c.field, Op: mcp.BodyEqualsOp, Value: c.value}})
			got := runBodyAssert(t, c.field, "equals", c.value, c.body)
			if got.Failure != wantMiss {
				t.Errorf("template failure=%v (%q) but the Go judge's miss=%v for equals %q on %s", got.Failure, got.Message, wantMiss, c.value, c.body)
			}
		})
	}
}

// An op the template does not know is a loud, NAMED failure — never a quiet fall back to `contains`.
func TestHTTPIngestionTemplate_UnknownOpFailsLoudly(t *testing.T) {
	for _, op := range []string{"gt", "frobnicate"} {
		v := runBodyAssert(t, "n", op, "1", `{"n":10}`)
		if !v.Failure {
			t.Errorf("op %q passed — an unsupported op must FAIL the sample, not be judged as contains", op)
			continue
		}
		if !strings.Contains(strings.ToLower(v.Message), "unsupported") || !strings.Contains(v.Message, op) {
			t.Errorf("op %q: the failure must name the unsupported op, got %q", op, v.Message)
		}
	}
}

// The ops the template already supported are unchanged.
func TestHTTPIngestionTemplate_ExistingOpsUnchanged(t *testing.T) {
	for _, c := range []struct {
		field, op, value, body string
		wantFail               bool
	}{
		{"s", "contains", "bc", `{"s":"abc"}`, false},
		{"s", "contains", "zz", `{"s":"abc"}`, true},
		{"", "contains", "ell", `hello`, false},
		{"s", "matches", "^a.c$", `{"s":"abc"}`, false},
		{"s", "matches", "^b", `{"s":"abc"}`, true},
		{"s", "exists", "", `{"s":"abc"}`, false},
		{"t", "exists", "", `{"s":"abc"}`, true},
	} {
		got := runBodyAssert(t, c.field, c.op, c.value, c.body)
		if got.Failure != c.wantFail {
			t.Errorf("%s %q on %s: failure=%v (%q), want %v", c.op, c.value, c.body, got.Failure, got.Message, c.wantFail)
		}
	}
}
