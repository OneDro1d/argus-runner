package argus

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// / -15 (amended): how an environment value is substituted into a chain spec.
//   - ${INGESTION_URL} is a leading MARKER and wins over an environment variable of that name;
//   - a value is placed INSIDE the JSON text as data, so it can neither break nor extend the spec, and a
//     parse error never quotes a character of it.

// recorder is a real server that keeps the raw bytes and the path of every request it receives.
type recorder struct {
	mu     sync.Mutex
	bodies []string
	paths  []string
}

func (r *recorder) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.bodies = append(r.bodies, string(b))
		r.paths = append(r.paths, req.Method+" "+req.URL.RequestURI())
		r.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (r *recorder) hits() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.paths)
}

const okExpect = "### Runnable\n- step s: status=200\n"

// ── item 2: the marker wins over an environment variable of the same name ──────────────────────────

func TestChainHTTPStep_MarkerWinsOverAnEnvironmentVariableCalledIngestionURL(t *testing.T) {
	configured, other := &recorder{}, &recorder{}
	cfgSrv, otherSrv := configured.server(t), other.server(t)
	t.Setenv("INGESTION_URL", otherSrv.URL)

	trig := `{"steps":[{"type":"http","name":"s","method":"GET","url":"${INGESTION_URL}/api/v1/me"}]}`
	res := runChainScenario(httpTargetCfg(cfgSrv.URL), scenario.Parse(chainRunMD("", trig, okExpect)), "tr-marker-wins", "testkit/ui")
	if res.Status != "passed" {
		t.Fatalf("status %q %+v", res.Status, res.Failure)
	}
	if configured.hits() != 1 {
		t.Errorf("the CONFIGURED target received %d requests, want 1", configured.hits())
	}
	if other.hits() != 0 {
		t.Errorf("the server named by the environment variable received %d requests, want 0: %v", other.hits(), other.paths)
	}
}

// The marker stays valid only as the LEADING token of a chain http url; anywhere else it is refused, and an
// environment variable of that name must not make it acceptable.
func TestChainHTTPStep_MarkerAnywhereElseInTheURLIsRefusedEvenWithTheVariableSet(t *testing.T) {
	configured, other := &recorder{}, &recorder{}
	cfgSrv, otherSrv := configured.server(t), other.server(t)
	for _, env := range []string{"", otherSrv.URL} {
		t.Setenv("INGESTION_URL", env)
		trig := `{"steps":[{"type":"http","name":"s","method":"GET","url":"` + cfgSrv.URL + `/x?next=${INGESTION_URL}/y"}]}`
		res := runChainScenario(httpTargetCfg(cfgSrv.URL), scenario.Parse(chainRunMD("", trig, okExpect)), "tr-marker-mid", "testkit/ui")
		if res.Status != "failed" || res.Failure == nil || !strings.Contains(res.Failure.Observed, "${INGESTION_URL}") {
			t.Fatalf("env=%q: a marker that is not the leading token must be refused by name: %q %+v", env, res.Status, res.Failure)
		}
	}
	if configured.hits() != 0 || other.hits() != 0 {
		t.Errorf("nothing may be sent on a refusal: configured=%d other=%d", configured.hits(), other.hits())
	}
}

// ── item 3: a value is data, not JSON ──────────────────────────────────────────────────────────────

// awkward holds every character class the brief names: a quote, a backslash, a newline, a tab, non-ASCII.
const awkward = "pw\"with\\slash\nnew\tline é漢字✓ </&>"

func runBodyChain(t *testing.T, body string, envName, envValue string) (*recorder, string) {
	t.Helper()
	t.Setenv(envName, envValue)
	rec := &recorder{}
	srv := rec.server(t)
	trig := `{"steps":[{"type":"http","name":"s","method":"POST","url":"` + srv.URL + `/login","body":` + body + `}]}`
	res := runChainScenario(httpTargetCfg(srv.URL), scenario.Parse(chainRunMD("", trig, okExpect)), "tr-subst", "testkit/ui")
	status := res.Status
	if res.Failure != nil {
		status += " | " + res.Failure.Observed
	}
	return rec, status
}

func TestChainSpec_AValueWithJSONSpecialCharactersArrivesByteExact(t *testing.T) {
	rec, status := runBodyChain(t, `{"password":"${SUBST_AWKWARD}","keep":"x"}`, "SUBST_AWKWARD", awkward)
	if !strings.HasPrefix(status, "passed") {
		t.Fatalf("the step failed: %s", status)
	}
	if rec.hits() != 1 {
		t.Fatalf("hits = %d", rec.hits())
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(rec.bodies[0]), &got); err != nil {
		t.Fatalf("the server received invalid JSON: %v", err)
	}
	if got["password"] != awkward {
		t.Errorf("the value did not arrive byte-exact: got %q want %q", got["password"], awkward)
	}
	if got["keep"] != "x" {
		t.Errorf("a neighbouring field changed: %v", got)
	}
}

func TestChainSpec_ACraftedValueCannotAddAKey(t *testing.T) {
	crafted := `x","admin":true,"y":"`
	rec, status := runBodyChain(t, `{"password":"${SUBST_CRAFTED}"}`, "SUBST_CRAFTED", crafted)
	if !strings.HasPrefix(status, "passed") {
		t.Fatalf("the step failed: %s", status)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(rec.bodies[0]), &got); err != nil {
		t.Fatalf("the server received invalid JSON: %v", err)
	}
	var keys []string
	for k := range got {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if strings.Join(keys, ",") != "password" {
		t.Errorf("the server must receive exactly the keys the scenario wrote, got %v", keys)
	}
	if got["password"] != crafted {
		t.Errorf("the crafted value must arrive as a plain string: %q", got["password"])
	}
}

// A plain value (letters, digits) gives the request bytes it always did, including a placeholder written
// OUTSIDE a JSON string.
func TestChainSpec_APlainValueGivesByteIdenticalRequestBytes(t *testing.T) {
	t.Setenv("SUBST_WORD", "abc123")
	rec, status := runBodyChain(t, `{"count": ${SUBST_N}, "name":"${SUBST_WORD}"}`, "SUBST_N", "42")
	if !strings.HasPrefix(status, "passed") {
		t.Fatalf("the step failed: %s", status)
	}
	if want := `{"count": 42, "name":"abc123"}`; rec.bodies[0] != want {
		t.Errorf("request bytes changed:\n got %q\nwant %q", rec.bodies[0], want)
	}
}

// If the spec still cannot be parsed, the error names the problem and quotes no character of a value.
func TestChainSpec_AParseErrorNeverQuotesACharacterOfAValue(t *testing.T) {
	const canary = "7ÆØcanary"
	t.Setenv("SUBST_BAD_N", canary)
	payload := `{"steps":[{"type":"http","name":"s","method":"POST","url":"http://x.invalid/a","body":{"count": ${SUBST_BAD_N}}}]}`
	_, err := parseChainSpec(payload, "tr-1")
	if err == nil {
		t.Fatal("a number position holding text must not parse")
	}
	for _, frag := range []string{"Æ", "Ø", "Ã", "canary", "invalid character"} {
		if strings.Contains(err.Error(), frag) {
			t.Errorf("the parse error quotes part of a value (%q): %v", frag, err)
		}
	}
	if !strings.Contains(err.Error(), "not valid JSON") || !strings.Contains(err.Error(), "environment value") {
		t.Errorf("the error must name the problem: %v", err)
	}

	// and through the run: the reported failure carries no character of the value either
	rec := &recorder{}
	srv := rec.server(t)
	trig := strings.ReplaceAll(payload, "http://x.invalid", srv.URL)
	res := runChainScenario(httpTargetCfg(srv.URL), scenario.Parse(chainRunMD("", trig, okExpect)), "tr-bad", "testkit/ui")
	if res.Failure == nil || strings.Contains(res.Failure.Observed, "Æ") || strings.Contains(res.Failure.Observed, "canary") {
		t.Errorf("the failure must be present and must not carry the value: %+v", res.Failure)
	}
	if rec.hits() != 0 {
		t.Errorf("nothing may be sent when the spec cannot be parsed")
	}
}

// An authoring mistake that involves no environment value keeps the parser's own, more helpful, text.
func TestChainSpec_AnAuthoringTypoKeepsTheParsersOwnError(t *testing.T) {
	_, err := parseChainSpec(`{"steps":[{"type":"http",}]}`, "tr-1")
	if err == nil || !strings.Contains(err.Error(), "invalid character") {
		t.Errorf("a typo with no substitution involved must keep the parser's message, got %v", err)
	}
}
