package chain

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/mcp"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// A step saves any JSON value AS JSON (`{"json": "<path>"}`), a later step places it, unquoted,
// into a JSON body or args. Every test is hermetic: httptest servers, no database.

func jsonSave(path string) scenario.SaveSpec { return scenario.SaveSpec{Path: path, JSON: true} }

// ── the evaluator ───────────────────────────────────────────────────────────────────────────────────

const jsonSaveDoc = `{
  "doc": {"id": 12345678901234567890, "price": 0.10, "tags": ["a", "b"], "nested": {"z": 1, "a": null}},
  "name": "say \"hi\" now",
  "ok": true, "no": false, "nothing": null, "exp": 1.5e3, "small": 5,
  "ids": [ 11111111111111111111, 2 ]
}`

func TestSaveFromHTTP_JSON_KeepsTheValueAsWritten(t *testing.T) {
	for _, c := range []struct{ name, path, want string }{
		{"object, whitespace removed, key order kept", "doc.nested", `{"z":1,"a":null}`},
		{"nested object with a long integer and a trailing-zero decimal", "doc",
			`{"id":12345678901234567890,"price":0.10,"tags":["a","b"],"nested":{"z":1,"a":null}}`},
		{"array", "ids", `[11111111111111111111,2]`},
		{"array element", "ids.0", `11111111111111111111`},
		{"string keeps its quotes and escapes", "name", `"say \"hi\" now"`},
		{"long integer, every digit", "doc.id", `12345678901234567890`},
		{"decimal with a trailing zero", "doc.price", `0.10`},
		{"exponent as written", "exp", `1.5e3`},
		{"true", "ok", `true`},
		{"false", "no", `false`},
		{"null", "nothing", `null`},
	} {
		got, fail := saveFromHTTP([]byte(jsonSaveDoc), "v", jsonSave(c.path))
		if fail != "" || got != c.want {
			t.Errorf("%s: got %q fail %q, want %q", c.name, got, fail, c.want)
		}
	}
	whole, fail := saveFromHTTP([]byte(" {\"a\": [1, 2.50]} \n"), "v", jsonSave("$"))
	if fail != "" || whole != `{"a":[1,2.50]}` {
		t.Errorf("$ is the whole response, compact: %q %q", whole, fail)
	}
}

func TestSaveFromHTTP_JSON_PathThatDoesNotResolveFailsTheStep(t *testing.T) {
	for _, path := range []string{"nope", "doc.nope", "ids.9", "ids.x", "name.deeper"} {
		got, fail := saveFromHTTP([]byte(jsonSaveDoc), "v", jsonSave(path))
		if got != "" || !strings.Contains(fail, "no value at path "+`"`+path+`"`) || !strings.Contains(fail, `"v"`) {
			t.Errorf("%s: got %q / %q", path, got, fail)
		}
	}
	if _, fail := saveFromHTTP([]byte(`not json`), "v", jsonSave("$")); fail == "" {
		t.Error("a response that is not JSON cannot be saved as JSON")
	}
	if _, fail := saveFromHTTP([]byte(`{"a":1} trailing`), "v", jsonSave("$")); fail == "" {
		t.Error("a document with trailing text is not one JSON value")
	}
}

func TestSaveFromHTTP_JSON_OverTheLimitFailsNamingTheVariableTheSizeAndTheLimit(t *testing.T) {
	big := `{"blob":"` + strings.Repeat("x", scenario.MaxJSONSavedBytes) + `"}`
	_, fail := saveFromHTTP([]byte(big), "bigvar", jsonSave("blob"))
	if !strings.Contains(fail, `"bigvar"`) || !strings.Contains(fail, "65538 bytes") || !strings.Contains(fail, "65536-byte limit") {
		t.Errorf("want the variable, the size and the limit; got %q", fail)
	}
	atLimit := `{"blob":"` + strings.Repeat("x", scenario.MaxJSONSavedBytes-2) + `"}`
	if v, fail := saveFromHTTP([]byte(atLimit), "v", jsonSave("blob")); fail != "" || len(v) != scenario.MaxJSONSavedBytes {
		t.Errorf("exactly the limit is allowed: %d / %q", len(v), fail)
	}
}

// The string form keeps its meaning; a container or null now says which it was, and how to save it.
func TestSaveFromHTTP_StringForm_ContainerSentenceNamesTheJSONForm(t *testing.T) {
	for _, c := range []struct{ path, kind string }{
		{"doc", "an object"}, {"ids", "an array"}, {"nothing", "null"},
	} {
		got, fail := saveFromHTTP([]byte(jsonSaveDoc), "v", scenario.SaveSpec{Path: c.path})
		if got != "" || !strings.Contains(fail, "is "+c.kind) || !strings.Contains(fail, `{"json": "`+c.path+`"}`) {
			t.Errorf("%s: got %q / %q", c.path, got, fail)
		}
	}
	// a missing path keeps the sentence it always had
	_, fail := saveFromHTTP([]byte(jsonSaveDoc), "v", scenario.SaveSpec{Path: "nope"})
	if fail != `save failed: no value at path "nope" for variable "v" in this step's response body` {
		t.Errorf("the missing-path sentence changed: %q", fail)
	}
	// and what a scalar saves is untouched, a number included (it goes through float64 as it always did)
	for path, want := range map[string]string{"name": `say "hi" now`, "ok": "true", "small": "5", "doc.price": "0.1"} {
		if got, fail := saveFromHTTP([]byte(jsonSaveDoc), "v", scenario.SaveSpec{Path: path}); fail != "" || got != want {
			t.Errorf("the string form of %s: %q / %q, want %q", path, got, fail, want)
		}
	}
}

func TestSaveFromMCP_JSON_ReadsThroughTheSameDocumentAsTheStringForm(t *testing.T) {
	inner := `{"items":[{"id":18446744073709551615}],"total":3}`
	raw, _ := json.Marshal(map[string]any{"result": map[string]any{
		"content": []any{map[string]any{"type": "text", "text": inner}}, "isError": false}})
	for path, want := range map[string]string{
		"$": inner, "items": `[{"id":18446744073709551615}]`, "items.0.id": `18446744073709551615`, "total": `3`,
	} {
		if got, fail := saveFromMCP(raw, "v", jsonSave(path)); fail != "" || got != want {
			t.Errorf("%s: got %q / %q, want %q", path, got, fail, want)
		}
	}
	// no content text: the result object itself, as the string form falls back to it
	direct := json.RawMessage(`{"result":{"doc":{"n":1.50},"isError":false}}`)
	if got, fail := saveFromMCP(direct, "v", jsonSave("doc")); fail != "" || got != `{"n":1.50}` {
		t.Errorf("fallback to result: %q / %q", got, fail)
	}
	if _, fail := saveFromMCP(raw, "v", jsonSave("nope")); !strings.Contains(fail, "read through result.content[0].text, else result") {
		t.Errorf("a missing path keeps the mcp sentence: %q", fail)
	}
	// the string form of a container, over mcp
	if _, fail := saveFromMCP(raw, "v", scenario.SaveSpec{Path: "items"}); !strings.Contains(fail, "is an array") {
		t.Errorf("the string form of an array over mcp: %q", fail)
	}
}

// ── the binding ─────────────────────────────────────────────────────────────────────────────────────

// recorder is an http server that returns reply for GET /read and keeps the raw bytes of every POST /write.
type recorder struct {
	*httptest.Server
	mu     sync.Mutex
	bodies [][]byte
	hits   atomic.Int32
}

func newRecorder(t *testing.T, reply string) *recorder {
	t.Helper()
	r := &recorder{}
	r.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.hits.Add(1)
		if req.Method == http.MethodPost {
			b, _ := io.ReadAll(req.Body)
			r.mu.Lock()
			r.bodies = append(r.bodies, b)
			r.mu.Unlock()
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		_, _ = w.Write([]byte(reply))
	}))
	t.Cleanup(r.Close)
	return r
}

func (r *recorder) lastBody(t *testing.T) []byte {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.bodies) == 0 {
		t.Fatal("the SUT received no POST")
	}
	return r.bodies[len(r.bodies)-1]
}

// decodeExact decodes what was RECEIVED, numbers kept as text.
func decodeExact(t *testing.T, b []byte) map[string]any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("the SUT received a body that is not JSON: %v: %s", err, b)
	}
	return m
}

func readThenWrite(srv *recorder, bodyTmpl string, claims []mcp.BodyAssert) []Step {
	return []Step{
		HTTPStep("read", "GET", srv.URL+"/read", nil, "", 200, nil,
			map[string]scenario.SaveSpec{"doc": jsonSave("doc"), "ids": jsonSave("ids"), "name": {Path: "name"}},
			nil, false, nil, &scenario.MoneySpendLedger{}),
		HTTPStep("write", "POST", srv.URL+"/write", nil, bodyTmpl, 200, claims, nil, nil, false, nil, &scenario.MoneySpendLedger{}),
	}
}

func TestHTTPBody_JSONSavedObjectArrivesAsANestedObject(t *testing.T) {
	srv := newRecorder(t, jsonSaveDoc)
	// the unquoted-by-quoting form (how it must be written inside a TRIGGER payload that parses), a bare
	// reference, a list, and a string-form variable next to them that must still be escaped
	res := Run("c", readThenWrite(srv,
		`{"quoted":"${saved.doc}","bare":${saved.doc},"list":"${saved.ids}","who":"${saved.name}","n":1}`, nil))
	if res.Status != "passed" {
		t.Fatalf("chain: %s %+v %+v", res.Status, res.Failure, res.Steps)
	}
	got := decodeExact(t, srv.lastBody(t))
	for _, key := range []string{"quoted", "bare"} {
		doc, ok := got[key].(map[string]any)
		if !ok {
			t.Fatalf("%s must arrive as a nested object, got %T %v", key, got[key], got[key])
		}
		if id, _ := doc["id"].(json.Number); id.String() != "12345678901234567890" {
			t.Errorf("%s: the long integer arrived as %q", key, id)
		}
		if pr, _ := doc["price"].(json.Number); pr.String() != "0.10" {
			t.Errorf("%s: the decimal arrived as %q", key, pr)
		}
		if tags, _ := doc["tags"].([]any); len(tags) != 2 || tags[0] != "a" || tags[1] != "b" {
			t.Errorf("%s: tags %v", key, doc["tags"])
		}
		nested, _ := doc["nested"].(map[string]any)
		if nested["a"] != nil || nested["z"].(json.Number).String() != "1" {
			t.Errorf("%s: nested %v", key, doc["nested"])
		}
	}
	list, _ := got["list"].([]any)
	if len(list) != 2 || list[0].(json.Number).String() != "11111111111111111111" {
		t.Errorf("the list must arrive as a list of exact numbers, got %v", got["list"])
	}
	// the string-form variable is still a string, JSON-escaped
	if got["who"] != `say "hi" now` {
		t.Errorf("a string-form variable beside it must still be escaped text, got %#v", got["who"])
	}
	if !bytes.Contains(srv.lastBody(t), []byte(`"id":12345678901234567890`)) {
		t.Errorf("the bytes on the wire must carry the digits as written: %s", srv.lastBody(t))
	}
}

func TestHTTPBody_JSONSavedScalarsArriveAsTheirJSONTypes(t *testing.T) {
	srv := newRecorder(t, `{"s":"txt","n":7.50,"b":true,"z":null}`)
	steps := []Step{
		HTTPStep("read", "GET", srv.URL+"/read", nil, "", 200, nil, map[string]scenario.SaveSpec{
			"s": jsonSave("s"), "n": jsonSave("n"), "b": jsonSave("b"), "z": jsonSave("z")}, nil, false, nil, &scenario.MoneySpendLedger{}),
		HTTPStep("write", "POST", srv.URL+"/write", nil, `{"s":"${saved.s}","n":"${saved.n}","b":"${saved.b}","z":"${saved.z}"}`,
			200, nil, nil, nil, false, nil, &scenario.MoneySpendLedger{}),
	}
	if res := Run("c", steps); res.Status != "passed" {
		t.Fatalf("chain: %s %+v", res.Status, res.Failure)
	}
	if got := string(srv.lastBody(t)); got != `{"s":"txt","n":7.50,"b":true,"z":null}` {
		t.Errorf("a string keeps its quotes, a number its digits, true and null their type: %s", got)
	}
}

func TestHTTPBody_AJSONSavedValueThatBreaksTheBodyIsNotFired(t *testing.T) {
	srv := newRecorder(t, jsonSaveDoc)
	// the reference sits INSIDE a longer string: the bound text is no longer JSON
	res := Run("c", readThenWrite(srv, `{"note":"see ${saved.doc} for details"}`, nil))
	if res.Status == "passed" {
		t.Fatalf("a body that stopped being JSON must not pass: %+v", res)
	}
	w := res.Steps[1]
	if w.Status != report.StepNotMeasured || !strings.Contains(w.Observed, "${saved.doc}") || !strings.Contains(w.Observed, "not valid JSON") {
		t.Errorf("want not-measured naming the variable; got %q / %q", w.Status, w.Observed)
	}
	if n := srv.hits.Load(); n != 1 { // the read only
		t.Errorf("the write must not be fired; the SUT saw %d requests", n)
	}
}

func TestHTTPBody_ATextBodyIsNotHeldToJSON(t *testing.T) {
	srv := newRecorder(t, jsonSaveDoc)
	res := Run("c", readThenWrite(srv, `plain text with ${saved.name} in it`, nil))
	if res.Status != "passed" || string(srv.lastBody(t)) != `plain text with say \"hi\" now in it` {
		t.Errorf("a text body keeps today's escaping and is not judged as JSON: %s %q", res.Status, srv.lastBody(t))
	}
}

func TestHTTPStep_JSONSavedVariableIsRefusedAtRunTimeOutsideABody(t *testing.T) {
	srv := newRecorder(t, jsonSaveDoc)
	read := readThenWrite(srv, "", nil)[0]
	for name, step := range map[string]Step{
		"url":    HTTPStep("u", "GET", srv.URL+"/read?d=${saved.doc}", nil, "", 200, nil, nil, nil, false, nil, &scenario.MoneySpendLedger{}),
		"header": HTTPStep("h", "GET", srv.URL+"/read", map[string]string{"X-Doc": "${saved.doc}"}, "", 200, nil, nil, nil, false, nil, &scenario.MoneySpendLedger{}),
		"claim": HTTPStep("c", "GET", srv.URL+"/read", nil, "", 200,
			[]mcp.BodyAssert{{Field: "name", Op: mcp.BodyContainsOp, Value: "${saved.doc}"}}, nil, nil, false, nil, &scenario.MoneySpendLedger{}),
	} {
		res := Run("c", []Step{read, step})
		w := res.Steps[1]
		if w.Status != report.StepNotMeasured || !strings.Contains(w.Observed, "only be placed in a body or args") ||
			!strings.Contains(w.Observed, "${saved.doc}") {
			t.Errorf("%s: want a not-measured step naming the variable; got %q / %q", name, w.Status, w.Observed)
		}
	}
	if n := srv.hits.Load(); n != 3 { // three reads, no refused step fired
		t.Errorf("a refused step must not be fired; the SUT saw %d requests (want 3 reads)", n)
	}
}

// ── mcp args ────────────────────────────────────────────────────────────────────────────────────────

// rawArgsMCP is an MCP server that answers every tool call with reply and keeps the RAW arguments bytes.
func rawArgsMCP(t *testing.T, reply string) (*httptest.Server, func() []byte) {
	t.Helper()
	var mu sync.Mutex
	var last []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Arguments json.RawMessage `json:"arguments"`
			} `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "s1")
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"protocolVersion": "2024-11-05"}})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/call":
			mu.Lock()
			last = append([]byte(nil), req.Params.Arguments...)
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{
				"content": []any{map[string]any{"type": "text", "text": reply}}, "isError": false}})
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func() []byte { mu.Lock(); defer mu.Unlock(); return last }
}

func TestMCPArgs_JSONSavedObjectArrivesAsANestedObject(t *testing.T) {
	writeSrv, _ := rawArgsMCP(t, jsonSaveDoc)
	readSrv, sent := rawArgsMCP(t, "ok")
	wcl := &mcp.Client{ServerURL: writeSrv.URL, Transport: mcp.Streamable}
	rcl := &mcp.Client{ServerURL: readSrv.URL, Transport: mcp.Streamable}
	res := Run("tr-x", []Step{
		MCPStep("read", wcl, "t_read", `{}`, mcp.Expect{ErrorPlane: mcp.PlaneNone},
			map[string]scenario.SaveSpec{"doc": jsonSave("doc"), "name": {Path: "name"}}),
		MCPStep("write", rcl, "t_write", `{"resource":"${saved.doc}","who":"${saved.name}"}`,
			mcp.Expect{ErrorPlane: mcp.PlaneNone}, nil),
	})
	if res.Status != "passed" {
		t.Fatalf("chain: %s %+v %+v", res.Status, res.Failure, res.Steps)
	}
	got := decodeExact(t, sent())
	doc, ok := got["resource"].(map[string]any)
	if !ok {
		t.Fatalf("the args must carry a nested object, got %T: %s", got["resource"], sent())
	}
	if id, _ := doc["id"].(json.Number); id.String() != "12345678901234567890" {
		t.Errorf("the long integer arrived as %q", id)
	}
	if pr, _ := doc["price"].(json.Number); pr.String() != "0.10" {
		t.Errorf("the decimal arrived as %q", pr)
	}
	if got["who"] != `say "hi" now` {
		t.Errorf("a string-form variable beside it is still escaped text: %#v", got["who"])
	}
}

func TestMCPArgs_AJSONSavedValueThatBreaksTheArgsIsNotCalled(t *testing.T) {
	srv, sent := rawArgsMCP(t, jsonSaveDoc)
	cl := &mcp.Client{ServerURL: srv.URL, Transport: mcp.Streamable}
	res := Run("tr-x", []Step{
		MCPStep("read", cl, "t_read", `{}`, mcp.Expect{ErrorPlane: mcp.PlaneNone}, map[string]scenario.SaveSpec{"doc": jsonSave("doc")}),
		MCPStep("write", cl, "t_write", `{"resource":"id-${saved.doc}"}`, mcp.Expect{ErrorPlane: mcp.PlaneNone}, nil),
	})
	w := res.Steps[1]
	if w.Status != report.StepNotMeasured || !strings.Contains(w.Observed, "${saved.doc}") {
		t.Errorf("want not-measured naming the variable; got %q / %q", w.Status, w.Observed)
	}
	if string(sent()) != "{}" { // the read's own call is the last one that reached the SUT
		t.Errorf("the write must not be called; last args seen: %s", sent())
	}
}

// ── masking ─────────────────────────────────────────────────────────────────────────────────────────

// A JSON-saved value is not a credential in the usual sense, but it is a value the builder is not shown: wherever
// a saved value is masked in what an author reads, the exact saved text goes too.
func TestMasking_JSONSavedValueIsShownAsItsPlaceholderInAFailedClaim(t *testing.T) {
	// the SUT echoes the saved object back, as an error body might
	srv := newRecorder(t, `{"doc":{"a":"secret-object-value-1","b":[1,2,3]},"echo":"x"}`)
	steps := []Step{
		HTTPStep("read", "GET", srv.URL+"/read", nil, "", 200, nil, map[string]scenario.SaveSpec{"doc": jsonSave("doc")},
			nil, false, nil, &scenario.MoneySpendLedger{}),
		// a claim that fails; the observed text for an unscoped claim is the whole answer
		HTTPStep("again", "GET", srv.URL+"/read", nil, "", 200,
			[]mcp.BodyAssert{{Op: mcp.BodyContainsOp, Value: "never-in-the-answer"}}, nil, nil, false, nil, &scenario.MoneySpendLedger{}),
	}
	res := Run("c", steps)
	fc := failedClaimsOf(t, res.Steps[1])
	if len(fc) != 1 {
		t.Fatalf("want one failed claim, got %+v (%+v)", fc, res.Steps[1])
	}
	if strings.Contains(fc[0].Observed, "secret-object-value-1") || !strings.Contains(fc[0].Observed, "${saved.doc}") {
		t.Errorf("the saved object must be shown as its placeholder: %q", fc[0].Observed)
	}
	assertNoValue(t, res, "secret-object-value-1")
}

func TestMasking_JSONSavedValueIsShownAsItsPlaceholderInARecordedOutput(t *testing.T) {
	srv := newRecorder(t, `{"doc":{"a":"secret-object-value-2","b":[1,2,3]},"echo":"x"}`)
	oc := &OutputCapture{Spec: bodySpec()}
	read := HTTPStep("read", "GET", srv.URL+"/read", nil, "", 200, nil, map[string]scenario.SaveSpec{"doc": jsonSave("doc")},
		nil, false, nil, &scenario.MoneySpendLedger{})
	again := HTTPStepWithOutput("again", "GET", srv.URL+"/read", nil, "", 200, nil, nil, nil, false, nil, &scenario.MoneySpendLedger{}, oc)
	res := Run("c", []Step{read, again})
	if res.Status != "passed" || res.Steps[1].Output == nil {
		t.Fatalf("chain: %s %+v", res.Status, res.Steps)
	}
	file := string(res.Steps[1].Output.Stored.File())
	if strings.Contains(file, "secret-object-value-2") || !strings.Contains(file, "${saved.doc}") {
		t.Errorf("the recorded output must show the placeholder, never the saved object: %s", file)
	}
	// and the step that SAVED it does not record it either
	oc2 := &OutputCapture{Spec: bodySpec()}
	own := HTTPStepWithOutput("own", "GET", srv.URL+"/read", nil, "", 200, nil,
		map[string]scenario.SaveSpec{"doc": jsonSave("doc")}, nil, false, nil, &scenario.MoneySpendLedger{}, oc2)
	r := own.Run("c", map[string]string{})
	if f := string(r.Output.Stored.File()); strings.Contains(f, "secret-object-value-2") {
		t.Errorf("the saving step's own record carries the saved object: %s", f)
	}
}

// The SUT does not have to echo the saved text byte for byte: another layout, another key order, a number
// written another way. The value is masked as a SUBTREE, so those are masked too; what is not equal is not.
func TestMasking_JSONSavedValueIsMaskedWhateverLayoutTheSUTEchoesItIn(t *testing.T) {
	vars := map[string]string{
		"doc":                   `{"id":12345678901234567890,"tags":["a","b"],"who":"secret-object-value-4"}`,
		jsonKindPrefix + "doc":  "1",
		"tok":                   `"secret-string-value-5"`,
		jsonKindPrefix + "tok":  "1",
		"flag":                  `true`,
		jsonKindPrefix + "flag": "1",
	}
	echo := `{
	  "error": "rejected",
	  "request": {"who": "secret-object-value-4", "tags": ["a", "b"], "id": 12345678901234567890},
	  "other": {"who": "someone-else", "tags": ["a", "b"]},
	  "token": "secret-string-value-5", "ok": true
	}`
	got := redactEverySaved(echo, vars, "")
	if strings.Contains(got, "secret-object-value-4") || strings.Contains(got, "secret-string-value-5") {
		t.Errorf("a saved value survived in a different layout: %s", got)
	}
	if !strings.Contains(got, "${saved.doc}") || !strings.Contains(got, "${saved.tok}") {
		t.Errorf("the placeholders are missing: %s", got)
	}
	if !strings.Contains(got, "someone-else") || !strings.Contains(got, `"ok":true`) {
		t.Errorf("a value that is not equal to a saved one, and a short saved scalar, must be left alone: %s", got)
	}
	// numbers compare as numbers: a judge that renders the saved object with float64 digits is still masked
	lossy := `{"id":1.2345678901234567e+19,"tags":["a","b"],"who":"secret-object-value-4"}`
	if got := redactEverySaved(lossy, vars, ""); strings.Contains(got, "secret-object-value-4") {
		t.Errorf("a float64 rendering of the saved object must be masked too: %s", got)
	}
}

func TestMasking_TheSavedValueIsNeverSerialisedIntoAReport(t *testing.T) {
	srv := newRecorder(t, `{"doc":{"a":"secret-object-value-3"}}`)
	read := HTTPStep("read", "GET", srv.URL+"/read", nil, "", 200, nil, map[string]scenario.SaveSpec{"doc": jsonSave("doc")},
		nil, false, nil, &scenario.MoneySpendLedger{})
	res := Run("c", []Step{read})
	assertNoValue(t, res, "secret-object-value-3")
	if res.Steps[0].Captured["doc"] == "" {
		t.Fatal("the step must have captured it (for the next step)")
	}
	b, _ := json.Marshal(res.Steps[0])
	if strings.Contains(string(b), "captured") {
		t.Errorf("Captured must stay json:\"-\": %s", b)
	}
}

// ── the money guard ─────────────────────────────────────────────────────────────────────────────────

// The guard reads the body that is REALLY sent (buildHTTPRequest's resolvedBody, with the saved value spliced in),
// so an amount_field that arrives inside a spliced object is judged like any other.
func TestMoneyGuard_JudgesTheSplicedBody(t *testing.T) {
	allow := scenario.MoneyWriteAllowlist{{Method: "POST", Path: "/write", Spends: true,
		AmountField: "order.source_amount", MaxAmount: 25, MaxPerRun: 5}}

	for _, c := range []struct {
		name, reply string
		sent        bool
	}{
		{"over max_amount inside the spliced object", `{"doc":{"source_amount":999,"x":1}}`, false},
		{"under max_amount inside the spliced object", `{"doc":{"source_amount":10,"x":1}}`, true},
		{"no amount_field inside the spliced object", `{"doc":{"x":1}}`, false},
	} {
		srv := newRecorder(t, c.reply)
		read := HTTPStep("read", "GET", srv.URL+"/read", nil, "", 200, nil, map[string]scenario.SaveSpec{"doc": jsonSave("doc")},
			nil, false, nil, &scenario.MoneySpendLedger{})
		spend := HTTPStep("spend", "POST", srv.URL+"/write", nil, `{"order":"${saved.doc}"}`, 200, nil, nil, nil, false, allow, &scenario.MoneySpendLedger{})
		// the steps are run one by one, as moneywrites_test.go does: chain.Run would relabel a refusal
		// (status error) as "SUT unreachable", and the refusal's own text is what is pinned here
		vars := map[string]string{}
		got := read.Run("c", vars)
		for k, v := range got.Captured {
			vars[k] = v
		}
		res := spend.Run("c", vars)
		posted := srv.hits.Load() == 2
		if posted != c.sent {
			t.Errorf("%s: sent=%v, want %v (%+v)", c.name, posted, c.sent, res)
		}
		if !c.sent && (res.Status != report.StatusError || !strings.HasPrefix(res.Observed, MoneyWriteRefusal)) {
			t.Errorf("%s: want a money refusal, got %q / %q", c.name, res.Status, res.Observed)
		}
		if c.sent && res.Status != "passed" {
			t.Errorf("%s: want passed, got %q / %q", c.name, res.Status, res.Observed)
		}
	}
}
