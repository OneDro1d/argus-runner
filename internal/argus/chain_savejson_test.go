package argus

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// `save` can keep a JSON value as JSON. Run the way an executor runs a scenario, so the
// TRIGGER payload has to parse, the save has to reach the step and the body has to carry the value.

func jsonSaveServer(t *testing.T) (*httptest.Server, func() []byte) {
	t.Helper()
	var mu sync.Mutex
	var posted []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			posted = b
			mu.Unlock()
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		_, _ = w.Write([]byte(`{"resource": {"id": 12345678901234567890, "price": 0.10, "tags": ["a","b"]}, "ids": [7, 8]}`))
	}))
	t.Cleanup(srv.Close)
	return srv, func() []byte { mu.Lock(); defer mu.Unlock(); return posted }
}

func TestChainRun_JSONSavedObjectIsPlacedUnquotedInALaterBody(t *testing.T) {
	srv, posted := jsonSaveServer(t)
	trig := `{"steps":[
	  {"type":"http","name":"read","method":"GET","url":"${BASE}/r","save":{"res":{"json":"resource"},"ids":{"json":"ids"}}},
	  {"type":"http","name":"write","method":"POST","url":"${BASE}/w","body":{"update":"${saved.res}","ids":"${saved.ids}","n":1}}
	]}`
	s := scenario.Parse(chainRunMD(srv.URL, trig, "### Runnable\n- step read: status=200\n- step write: status=200\n"))
	if _, errs := scenario.Validate(chainRunMD(srv.URL, trig, "### Runnable\n- step read: status=200\n- step write: status=200\n")); len(errs) != 0 {
		t.Fatalf("the scenario must validate: %v", errs)
	}
	r := runChainScenario(&config.Config{}, s, "tr-jsonsave", "testkit/ui")
	if r.Status != "passed" {
		t.Fatalf("want passed: %s / %+v / %+v", r.Status, r.Failure, r.Steps)
	}
	dec := json.NewDecoder(bytes.NewReader(posted()))
	dec.UseNumber()
	var got map[string]any
	if err := dec.Decode(&got); err != nil {
		t.Fatalf("the SUT received something that is not JSON: %v: %s", err, posted())
	}
	upd, ok := got["update"].(map[string]any)
	if !ok {
		t.Fatalf("update must arrive as a nested object: %s", posted())
	}
	if id := upd["id"].(json.Number).String(); id != "12345678901234567890" {
		t.Errorf("the long integer arrived as %q", id)
	}
	if pr := upd["price"].(json.Number).String(); pr != "0.10" {
		t.Errorf("the decimal arrived as %q", pr)
	}
	if ids, _ := got["ids"].([]any); len(ids) != 2 {
		t.Errorf("ids must arrive as a list: %s", posted())
	}
}

// The executor's second door: a file that reached a run without the validator is refused by name, before any
// step runs, with the SAME sentence the validator gives (both call scenario.SavedJSONProblems).
func TestChainRun_JSONSavedVariablePlacementIsRefusedByNameBeforeAnyStepRuns(t *testing.T) {
	for _, c := range []struct{ name, trig, expect, where string }{
		{"url",
			`{"steps":[{"type":"http","name":"read","method":"GET","url":"${BASE}/r","save":{"res":{"json":"resource"}}},
			 {"type":"http","name":"use","method":"GET","url":"${BASE}/r?d=${saved.res}"}]}`,
			"### Runnable\n- step read: status=200\n- step use: status=200\n", "the url"},
		{"header",
			`{"steps":[{"type":"http","name":"read","method":"GET","url":"${BASE}/r","save":{"res":{"json":"resource"}}},
			 {"type":"http","name":"use","method":"GET","url":"${BASE}/r","headers":{"X-R":"${saved.res}"}}]}`,
			"### Runnable\n- step read: status=200\n- step use: status=200\n", `the header "X-R"`},
		{"claim",
			`{"steps":[{"type":"http","name":"read","method":"GET","url":"${BASE}/r","save":{"res":{"json":"resource"}}},
			 {"type":"http","name":"use","method":"GET","url":"${BASE}/r"}]}`,
			"### Runnable\n- step read: status=200\n- step use: body contains ${saved.res}\n", "a claim (body contains ${saved.res})"},
	} {
		var hits int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			hits++
			_, _ = w.Write([]byte(`{"resource":{"id":1}}`))
		}))
		md := chainRunMD(srv.URL, c.trig, c.expect)
		r := runChainScenario(&config.Config{}, scenario.Parse(md), "tr-jsonsave-pre", "testkit/ui")
		srv.Close()
		want := scenario.SavedJSONPlacementSentence(c.where, "res")
		if r.Status == "passed" || r.Failure == nil || !strings.Contains(r.Failure.Observed, want) {
			t.Errorf("%s: want a refusal carrying %q; got %s / %+v", c.name, want, r.Status, r.Failure)
		}
		if hits != 0 {
			t.Errorf("%s: the refusal must come before any step runs; the SUT saw %d request(s)", c.name, hits)
		}
		if _, errs := scenario.Validate(md); !strings.Contains(strings.Join(errStrings(errs), "\n"), want) {
			t.Errorf("%s: the validator must give the same sentence; got %v", c.name, errs)
		}
	}
}

func TestChainRun_ASavedVariableHasOneKind(t *testing.T) {
	srv, _ := jsonSaveServer(t)
	trig := `{"steps":[
	  {"type":"http","name":"a","method":"GET","url":"${BASE}/r","save":{"v":"ids.0"}},
	  {"type":"http","name":"b","method":"GET","url":"${BASE}/r","save":{"v":{"json":"ids"}}}
	]}`
	md := chainRunMD(srv.URL, trig, "### Runnable\n- step a: status=200\n- step b: status=200\n")
	r := runChainScenario(&config.Config{}, scenario.Parse(md), "tr-jsonsave-kind", "testkit/ui")
	if r.Status == "passed" || r.Failure == nil || !strings.Contains(r.Failure.Observed, `variable "v" is saved as text here and with the {"json": …} form in step "b"`) {
		t.Errorf("the executor must refuse a variable of two kinds by name: %s / %+v", r.Status, r.Failure)
	}
}

func errStrings(errs []scenario.Error) []string {
	out := make([]string, 0, len(errs))
	for _, e := range errs {
		out = append(out, e.Message)
	}
	return out
}
