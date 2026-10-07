package argus

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"html"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/obsquery"
)

// #419 (, reported by Talos, Bartek's agent) — saga-presence judges PARSED FIELDS OF
// ONE LINE, with the SUT's DECLARED names.
//
// It used to pass on `body.contains("control_action")` over the WHOLE Loki response, after finding
// the saga with substring line filters, and it hardcoded event_type/saga. A SUT that emitted other
// saga steps for the run passed as soon as any returned line mentioned control_action anywhere (an
// error message, a next_step field, an echoed request body); a SUT declaring other names (Social)
// could never pass.
//
// The verify-saga script is run EXACTLY as extracted from templates/saga-presence.jmx, through
// testdata/saga_harness.groovy, against a fake Loki. It needs a Groovy runtime on the classpath:
//
//	ARGUS_GROOVY_CP='/path/groovy-4.0.22.jar:/path/groovy-json-4.0.22.jar'   (or /opt/jmeter/lib/*)
//
// ⚠ Without one those tests SKIP — and a skip reads "ok" (see the loud skip text). The gate sets
// ARGUS_REQUIRE_GROOVY=1, which turns the skip into a failure.

const sagaCID = "tr-1-SAGA-001-abc"

func sagaScenario(t *testing.T) *config.Config {
	t.Helper()
	c := &config.Config{}
	c.Targets.HTTP = &config.HTTPTarget{BaseURL: "http://sut.invalid"}
	c.Targets.Database = &config.DBTarget{JDBCURL: "jdbc:postgresql://db.invalid:5432/t", Username: "u"}
	return c
}

func sagaPropsFor(t *testing.T, c *config.Config) map[string]string {
	t.Helper()
	s := headerScenario(t, "HTTP Ingestion", "saga-presence")
	p, err := DeriveProps(c, s, sagaCID)
	if err != nil {
		t.Fatalf("DeriveProps: %v", err)
	}
	return p
}

// ── always-run: the wiring (no Groovy needed) ───────────────────────────────────────────────

func TestSagaPresenceProps_DefaultNamesComeFromTheGoReader(t *testing.T) {
	p := sagaPropsFor(t, sagaScenario(t))
	want := (&obsquery.Loki{}).SagaJudgeFields()
	if p["loki.corr_field"] != want.CorrelationField || p["loki.corr_field"] != "correlation_id" {
		t.Errorf("loki.corr_field = %q", p["loki.corr_field"])
	}
	if p["loki.saga_field"] != "event_type" {
		t.Errorf("loki.saga_field = %q, want the reader default event_type", p["loki.saga_field"])
	}
	if p["loki.saga_values"] != `["saga"]` {
		t.Errorf("loki.saga_values = %q", p["loki.saga_values"])
	}
	var chain []string
	if err := json.Unmarshal([]byte(p["loki.step_fields"]), &chain); err != nil || len(chain) == 0 || chain[0] != "step_name" {
		t.Errorf("loki.step_fields = %q (%v), want the reader's step_name fallback chain", p["loki.step_fields"], err)
	}
	if p["loki.log_format"] != "json" {
		t.Errorf("loki.log_format = %q", p["loki.log_format"])
	}
}

func TestSagaPresenceProps_DeclaredNamesReachTheTemplate(t *testing.T) {
	c := sagaScenario(t)
	c.Observability.Loki.CorrelationField = "request_id"
	c.Observability.Loki.SagaEventField = "event"
	c.Observability.Loki.SagaEventValues = []string{"tool_dispatch", "ayrshare_dispatch"}
	c.Observability.Loki.SagaStepFields = map[string]string{"step_name": "msg"}
	c.Observability.Loki.LogFormat = "logfmt"
	p := sagaPropsFor(t, c)
	if p["loki.corr_field"] != "request_id" || p["loki.saga_field"] != "event" {
		t.Errorf("corr/saga field = %q/%q", p["loki.corr_field"], p["loki.saga_field"])
	}
	if p["loki.saga_values"] != `["tool_dispatch","ayrshare_dispatch"]` {
		t.Errorf("loki.saga_values = %q", p["loki.saga_values"])
	}
	if p["loki.step_fields"] != `["msg"]` {
		t.Errorf("loki.step_fields = %q", p["loki.step_fields"])
	}
	if p["loki.log_format"] != "logfmt" {
		t.Errorf("loki.log_format = %q", p["loki.log_format"])
	}
}

func TestSagaPresenceTemplate_NoWholeResponseSubstringVerdict(t *testing.T) {
	script := sagaVerifyScript(t)
	if strings.Contains(script, "body.contains(") {
		t.Error("#419: the verdict must come from parsed fields of ONE line, never a substring test over the whole Loki response")
	}
	for _, name := range []string{"loki.corr_field", "loki.saga_field", "loki.saga_values", "loki.step_fields"} {
		if !strings.Contains(script, `"`+name+`"`) {
			t.Errorf("#419: the template must read the SUT's declared name from prop %s", name)
		}
	}
}

// ── Groovy-run: the verdict ─────────────────────────────────────────────────────────────────

type verdict struct {
	Successful bool   `json:"successful"`
	Message    string `json:"message"`
	Data       string `json:"data"`
}

func sagaVerifyScript(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("../../templates/saga-presence.jmx")
	if err != nil {
		t.Fatalf("read template: %v", err)
	}
	// the sampler sits several hashTrees down; a recursive scan is robust to nesting
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
		if n.XMLName.Local == "JSR223Sampler" {
			isVerify := false
			for _, a := range n.Attrs {
				if a.Name.Local == "testname" && strings.HasSuffix(a.Value, "-verify-saga") {
					isVerify = true
				}
			}
			if isVerify {
				for _, c := range n.Nodes {
					for _, a := range c.Attrs {
						if a.Name.Local == "name" && a.Value == "script" {
							found = html.UnescapeString(c.Text)
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
		t.Fatal("no verify-saga JSR223 script found in templates/saga-presence.jmx")
	}
	return found
}

func groovyClasspath(t *testing.T) string {
	t.Helper()
	cp := os.Getenv("ARGUS_GROOVY_CP")
	if cp == "" {
		if _, err := os.Stat("/opt/jmeter/lib"); err == nil {
			cp = "/opt/jmeter/lib/*"
		}
	}
	if cp == "" {
		msg := "NOT RUN: no Groovy runtime (set ARGUS_GROOVY_CP) — the saga-presence VERDICT was NOT exercised; this skip is not a pass"
		if os.Getenv("ARGUS_REQUIRE_GROOVY") == "1" {
			t.Fatal(msg)
		}
		t.Skip(msg)
	}
	return cp
}

// runVerify serves `lines` as one Loki stream, runs the template's verify-saga script against it and
// returns the verdict. lines == nil with unreachable=true points the script at a closed port.
func runVerify(t *testing.T, props map[string]string, lines []string, unreachable bool) verdict {
	t.Helper()
	cp := groovyClasspath(t)
	url := "http://127.0.0.1:1"
	if !unreachable {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			values := [][]string{}
			for i, l := range lines {
				values = append(values, []string{"170000000000000000" + string(rune('0'+i)), l})
			}
			body, _ := json.Marshal(map[string]any{"status": "success", "data": map[string]any{
				"resultType": "streams",
				"result":     []any{map[string]any{"stream": map[string]string{"service": "order-api"}, "values": values}},
			}})
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(body)
		}))
		defer srv.Close()
		url = srv.URL
	}
	pp := map[string]string{
		"loki.url": url, "correlation.id": sagaCID, "loki.expected_saga": "control_action",
		"loki.poll_attempts": "1", "loki.settle_ms": "0", "loki.poll_interval_ms": "0",
	}
	for k, v := range props {
		if strings.HasPrefix(k, "loki.") && k != "loki.url" {
			pp[k] = v
		}
	}
	in, _ := json.Marshal(map[string]any{"script": sagaVerifyScript(t), "props": pp})
	harness, _ := filepath.Abs("testdata/saga_harness.groovy")
	cmd := exec.Command("java", "-cp", cp, "groovy.ui.GroovyMain", harness)
	cmd.Stdin = bytes.NewReader(in)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("groovy harness failed: %v\n%s\n%s", err, out.String(), errb.String())
	}
	var v verdict
	lastJSON := ""
	for _, ln := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(strings.TrimSpace(ln), "{") {
			lastJSON = ln
		}
	}
	if err := json.Unmarshal([]byte(lastJSON), &v); err != nil {
		t.Fatalf("harness output unreadable: %v\n%s\n%s", err, out.String(), errb.String())
	}
	return v
}

func line(m map[string]any) string { b, _ := json.Marshal(m); return string(b) }

func TestSagaPresenceVerdict(t *testing.T) {
	def := sagaPropsFor(t, sagaScenario(t))

	social := sagaScenario(t)
	social.Observability.Loki.CorrelationField = "request_id"
	social.Observability.Loki.SagaEventField = "event"
	social.Observability.Loki.SagaEventValues = []string{"tool_dispatch", "ayrshare_dispatch"}
	social.Observability.Loki.SagaStepFields = map[string]string{"step_name": "msg"}
	socialP := sagaPropsFor(t, social)

	logfmt := sagaScenario(t)
	logfmt.Observability.Loki.LogFormat = "logfmt"
	logfmtP := sagaPropsFor(t, logfmt)

	real := line(map[string]any{"event_type": "saga", "correlation_id": sagaCID, "step_name": "control_action", "step_status": "ok"})
	decoyOtherStep := line(map[string]any{"event_type": "saga", "correlation_id": sagaCID, "step_name": "order_created", "next_step": "control_action"})

	cases := []struct {
		name  string
		props map[string]string
		lines []string
		want  bool
	}{
		{"a real control_action saga line passes", def, []string{real}, true},
		{"DECOY: same cid, saga line, step is order_created, control_action only in next_step -> FAIL", def, []string{decoyOtherStep}, false},
		{"DECOY: control_action only inside an error message of another step -> FAIL", def,
			[]string{line(map[string]any{"event_type": "saga", "correlation_id": sagaCID, "step_name": "order_created", "error": "retry of control_action failed"})}, false},
		{"DECOY: control_action step line that is NOT a saga line (event_type=request) -> FAIL", def,
			[]string{line(map[string]any{"event_type": "request", "correlation_id": sagaCID, "step_name": "control_action"})}, false},
		{"DECOY: control_action saga line of ANOTHER run that merely quotes our cid in a field -> FAIL", def,
			[]string{line(map[string]any{"event_type": "saga", "correlation_id": "tr-9-OTHER", "parent": sagaCID, "step_name": "control_action"})}, false},
		{"cid must match exactly, not as a prefix -> FAIL", def,
			[]string{line(map[string]any{"event_type": "saga", "correlation_id": sagaCID + "-2", "step_name": "control_action"})}, false},
		{"decoy first, real line later in the same response -> PASS", def, []string{decoyOtherStep, real}, true},
		{"unparseable lines are skipped, not a crash and not a pass -> FAIL", def,
			[]string{"{not json control_action " + sagaCID, "plain prose event_type saga control_action " + sagaCID}, false},
		{"an unparseable line next to a real one -> PASS", def, []string{"garbage {", real}, true},
		{"a JSON line that is not an object is skipped -> FAIL", def, []string{`"control_action"`, `42`}, false},
		{"NON-DEFAULT names: Social's real line passes with its declared names", socialP,
			[]string{line(map[string]any{"event": "tool_dispatch", "request_id": sagaCID, "msg": "control_action"})}, true},
		{"NON-DEFAULT names: a second declared marker value passes", socialP,
			[]string{line(map[string]any{"event": "ayrshare_dispatch", "request_id": sagaCID, "msg": "control_action"})}, true},
		{"NON-DEFAULT names: an undeclared marker value does not", socialP,
			[]string{line(map[string]any{"event": "http_request", "request_id": sagaCID, "msg": "control_action"})}, false},
		{"NON-DEFAULT names: Social's line is NOT accepted under the default names", def,
			[]string{line(map[string]any{"event": "tool_dispatch", "request_id": sagaCID, "msg": "control_action"})}, false},
		{"logfmt SUT: a stdlib-logger line with a leading prefix passes", logfmtP,
			[]string{"2026/07/16 19:28:43 INFO telemetry.event event_type=saga correlation_id=" + sagaCID + ` step_name="control_action" service=api`}, true},
		{"logfmt SUT: decoy with control_action in another field -> FAIL", logfmtP,
			[]string{"2026/07/16 19:28:43 INFO telemetry.event event_type=saga correlation_id=" + sagaCID + ` step_name=order_created next_step=control_action`}, false},
		{"a logfmt line is NOT parsed when the SUT declares json -> FAIL", def,
			[]string{"event_type=saga correlation_id=" + sagaCID + " step_name=control_action"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := runVerify(t, tc.props, tc.lines, false)
			if v.Successful != tc.want {
				t.Fatalf("successful=%v want %v (message %q)", v.Successful, tc.want, v.Message)
			}
			if !tc.want {
				if strings.Contains(v.Message, "order_created") || strings.Contains(v.Data, "order_created") {
					t.Errorf("a failure message must be reality-only, it echoed a line's content: %q / %q", v.Message, v.Data)
				}
				if !strings.Contains(v.Message, "not found in Loki for correlation_id "+sagaCID) {
					t.Errorf("failure wording changed: %q", v.Message)
				}
			}
		})
	}

	t.Run("Loki unreachable fails CLOSED", func(t *testing.T) {
		v := runVerify(t, def, nil, true)
		if v.Successful || !strings.Contains(v.Message, "Loki unreachable") || !strings.Contains(v.Message, "failing closed") {
			t.Fatalf("got successful=%v message=%q", v.Successful, v.Message)
		}
	})
}
