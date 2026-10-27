package argus

// A chain http step can send a TEXT body. Run the way an executor runs a scenario: the
// TRIGGER payload is parsed, the placeholders are filled, the step is built and a real net/http client sends
// to a real httptest server, which keeps the exact bytes and Content-Type it RECEIVED.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

type tbReceived struct {
	mu          sync.Mutex
	body        string
	contentType string
	contentLen  int64
	hits        int
}

func (r *tbReceived) server(t *testing.T, reply string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		w.Header().Set("Content-Type", "application/json")
		if req.URL.Path == "/read" {
			_, _ = w.Write([]byte(reply))
			return
		}
		r.mu.Lock()
		r.body, r.contentType, r.contentLen, r.hits = string(b), req.Header.Get("Content-Type"), req.ContentLength, r.hits+1
		r.mu.Unlock()
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func tbStep(headers, body string) string {
	s := `{"type":"http","name":"w","method":"POST","url":"${BASE}/w"`
	if headers != "" {
		s += `,"headers":` + headers
	}
	if body != "" {
		s += `,"body":` + body
	}
	return s + `}`
}

// The matrix: {string, object, null body} x {no header, application/json, a +json type, text/plain,
// text/plain with a parameter, form-encoded} — and the case variants the rule says it ignores.
func TestChainTextBody_Matrix_ExactBytesAndContentTypeReceived(t *testing.T) {
	headers := []struct{ name, json, ct string }{
		{"no header", "", ""},
		{"application/json", `{"Content-Type":"application/json"}`, "application/json"},
		{"application/merge-patch+json", `{"Content-Type":"application/merge-patch+json"}`, "application/merge-patch+json"},
		{"text/plain", `{"Content-Type":"text/plain"}`, "text/plain"},
		{"text/plain; charset=utf-8", `{"Content-Type":"text/plain; charset=utf-8"}`, "text/plain; charset=utf-8"},
		{"application/x-www-form-urlencoded", `{"Content-Type":"application/x-www-form-urlencoded"}`, "application/x-www-form-urlencoded"},
		// case and parameters are ignored when deciding; the header is sent as written
		{"TEXT/PLAIN", `{"content-type":"TEXT/PLAIN"}`, "TEXT/PLAIN"},
		{"Application/JSON; charset=utf-8", `{"Content-Type":"Application/JSON; charset=utf-8"}`, "Application/JSON; charset=utf-8"},
		{"application/vnd.api+JSON", `{"Content-Type":"application/vnd.api+JSON"}`, "application/vnd.api+JSON"},
	}
	isJSON := map[string]bool{"no header": true, "application/json": true, "application/merge-patch+json": true,
		"Application/JSON; charset=utf-8": true, "application/vnd.api+JSON": true}
	bodies := []struct {
		name, json string
		textWant   string // what the SUT receives when the type is NOT JSON
		jsonWant   string // what it receives when the type is JSON or absent
	}{
		{"string", `"a=1&b=two words"`, `a=1&b=two words`, `"a=1&b=two words"`},
		{"object", `{"a":1}`, `{"a":1}`, `{"a":1}`},
		{"null", `null`, ``, ``},
	}
	for _, b := range bodies {
		for _, h := range headers {
			t.Run(b.name+"/"+h.name, func(t *testing.T) {
				var rcv tbReceived
				srv := rcv.server(t, "")
				trig := `{"steps":[` + tbStep(h.json, b.json) + `]}`
				md := chainRunMD(srv.URL, trig, "### Runnable\n- step w: status=200\n")
				// NOTHING NEW IS REFUSED: the authoring door accepts every row exactly as before
				if _, errs := scenario.Validate(md); len(errs) != 0 {
					t.Fatalf("scenario.Validate refused the scenario: %v", errs)
				}
				res := runChainScenario(&config.Config{}, scenario.Parse(md), "tr-tb", "testkit/ui")
				if res.Status != "passed" {
					t.Fatalf("status %q: %+v / %+v", res.Status, res.Failure, res.Steps)
				}
				want, wantCT := b.jsonWant, h.ct
				if !isJSON[h.name] {
					want = b.textWant
				}
				if b.name != "null" && h.name == "no header" {
					wantCT = "application/json" // the default, when a body is present and no header is set
				}
				if rcv.body != want {
					t.Errorf("the SUT received %q, want %q", rcv.body, want)
				}
				if rcv.contentType != wantCT {
					t.Errorf("the SUT received Content-Type %q, want %q", rcv.contentType, wantCT)
				}
				if rcv.contentLen != int64(len(want)) {
					t.Errorf("Content-Length %d, want %d", rcv.contentLen, len(want))
				}
			})
		}
	}
}

// Only the step's OWN header counts: a Content-Type that comes from the scenario's TRIGGER headers is the
// default for every step, and leaves a string body in today's form.
func TestChainTextBody_OnlyTheStepsOwnHeaderCounts(t *testing.T) {
	var rcv tbReceived
	srv := rcv.server(t, "")
	md := strings.Replace(chainRunMD(srv.URL, `{"steps":[`+tbStep("", `"hello"`)+`]}`, "### Runnable\n- step w: status=200\n"),
		"POST `chain`", "POST `chain`\nContent-Type: text/plain", 1)
	res := runChainScenario(&config.Config{}, scenario.Parse(md), "tr-tb-own", "testkit/ui")
	if res.Status != "passed" {
		t.Fatalf("status %q: %+v", res.Status, res.Failure)
	}
	if rcv.contentType != "text/plain" { // control: the TRIGGER header really did reach the request
		t.Fatalf("setup: the TRIGGER-level Content-Type did not reach the request (got %q)", rcv.contentType)
	}
	if rcv.body != `"hello"` {
		t.Errorf("a TRIGGER-level Content-Type must not turn the body into text: received %q (content-type %q)", rcv.body, rcv.contentType)
	}
}

// Placeholders in a text body: ${cid} / ${correlation_id} / an environment value are spliced as RAW text (a
// `"` and a `\` stay one `"` and one `\`), ${saved.<var>} in its string form likewise, and a JSON-saved
// variable as its JSON text.
func TestChainTextBody_PlaceholdersAreSplicedRaw(t *testing.T) {
	t.Setenv("TB_ENV_VAL", `e"n\v`)
	var rcv tbReceived
	// the value of `s` holds a quote and a backslash; `o` is saved with the json form
	srv := rcv.server(t, `{"s":"x\"y\\z","o":{"k":"v\"w"}}`)
	trig := `{"steps":[
	  {"type":"http","name":"r","method":"GET","url":"${BASE}/read","save":{"s":"s","o":{"json":"o"}}},
	  ` + tbStep(`{"Content-Type":"text/plain"}`, `"cid=${cid} cor=${correlation_id} env=${TB_ENV_VAL} s=${saved.s} o=${saved.o}"`) + `]}`
	md := chainRunMD(srv.URL, trig, "### Runnable\n- step r: status=200\n- step w: status=200\n")
	if _, errs := scenario.Validate(md); len(errs) != 0 {
		t.Fatalf("scenario.Validate refused the scenario: %v", errs)
	}
	res := runChainScenario(&config.Config{}, scenario.Parse(md), "tr-tb-ph", "testkit/ui")
	if res.Status != "passed" {
		t.Fatalf("status %q: %+v / %+v", res.Status, res.Failure, res.Steps)
	}
	want := `cid=tr-tb-ph cor=tr-tb-ph env=e"n\v s=x"y\z o={"k":"v\"w"}`
	if rcv.body != want {
		t.Errorf("the SUT received\n  %q\nwant\n  %q", rcv.body, want)
	}
	if rcv.contentType != "text/plain" {
		t.Errorf("Content-Type %q", rcv.contentType)
	}
}

// The same placeholders under a JSON type keep today's escaping (a string body is a JSON string there).
func TestChainTextBody_JSONFormKeepsItsEscaping(t *testing.T) {
	t.Setenv("TB_ENV_VAL", `e"n\v`)
	var rcv tbReceived
	srv := rcv.server(t, `{"s":"x\"y\\z"}`)
	trig := `{"steps":[
	  {"type":"http","name":"r","method":"GET","url":"${BASE}/read","save":{"s":"s"}},
	  ` + tbStep(`{"Content-Type":"application/json"}`, `"env=${TB_ENV_VAL} s=${saved.s}"`) + `]}`
	md := chainRunMD(srv.URL, trig, "### Runnable\n- step r: status=200\n- step w: status=200\n")
	res := runChainScenario(&config.Config{}, scenario.Parse(md), "tr-tb-json", "testkit/ui")
	if res.Status != "passed" {
		t.Fatalf("status %q: %+v", res.Status, res.Failure)
	}
	want := `"env=e\"n\\v s=x\"y\\z"`
	if rcv.body != want {
		t.Errorf("the SUT received %q, want %q", rcv.body, want)
	}
}

// An unsaved ${saved.x} in a text body is refused by name, and nothing is sent.
func TestChainTextBody_UnsavedVariableIsRefusedByNameAndNothingIsSent(t *testing.T) {
	var rcv tbReceived
	srv := rcv.server(t, "")
	trig := `{"steps":[` + tbStep(`{"Content-Type":"text/plain"}`, `"id=${saved.nothing}"`) + `]}`
	res := runChainScenario(&config.Config{}, scenario.Parse(chainRunMD(srv.URL, trig, "### Runnable\n- step w: status=200\n")), "tr-tb-un", "testkit/ui")
	if res.Status == "passed" || rcv.hits != 0 {
		t.Fatalf("status %q hits %d: an unresolved reference must not be sent", res.Status, rcv.hits)
	}
}

// Compare mode records the RESPONSE of an http step, never the request body: a text body adds nothing to
// the recorded output, and no request text reaches it.
func TestChainTextBody_TheRecordedOutputIsTheResponseNotTheRequest(t *testing.T) {
	var rcv tbReceived
	srv := rcv.server(t, "")
	trig := `{"steps":[` + tbStep(`{"Content-Type":"text/plain"}`, `"REQUESTTEXT-12345"`) + `]}`
	res := runChainScenario(&config.Config{}, scenario.Parse(chainRunMD(srv.URL, trig, "### Runnable\n- step w: status=200\n")), "tr-tb-rec", "testkit/ui")
	for _, st := range res.Steps {
		if strings.Contains(st.Observed, "REQUESTTEXT") {
			t.Errorf("the request text reached the step's observed text: %q", st.Observed)
		}
	}
}
