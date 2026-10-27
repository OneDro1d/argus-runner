package router

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/OneDro1d/argus-runner/internal/mcpserver"
)

// AC-D70.2 (#699): four follow-ups from the AC-D70 review — (1) a forwarded call that gets no usable answer shows the
// agent the router's own sentence, never the upstream's address or error page, and the full text goes to the log, whose
// list-failure line is bounded; (2) a developer's own build (0.0.0-dev, 0.0.0-src+…) is the NEWEST in the version order;
// (3) the respelling touches whole tool names only; (4) a tool listed under both spellings keeps the one-underscore name.

// anAddress is any host:port, so a check cannot pass because the port it looked for changed.
var anAddress = regexp.MustCompile(`[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+:[0-9]+|localhost:[0-9]+`)

// requireNoAddress fails when an agent's answer holds an address or any of the given texts.
func requireNoAddress(t *testing.T, raw []byte, hostport string, absent ...string) {
	t.Helper()
	s := string(raw)
	for _, bad := range append([]string{"http://", "https://", hostport}, absent...) {
		if strings.Contains(s, bad) {
			t.Fatalf("the agent's answer holds %q:\n%s", bad, s)
		}
	}
	if m := anAddress.FindString(s); m != "" {
		t.Fatalf("the agent's answer holds an address %q:\n%s", m, s)
	}
}

// oneRunnerRig is a product folder over one stub executor, instance "a", with a session started.
func oneRunnerRig(t *testing.T, opts Options) (*rig, *stubExec, string) {
	t.Helper()
	e := newStubExec(t, "0.3.73", "runner-a", "author-a")
	r := newRig(t, opts, productFolder(pTok, map[string]Upstream{"a": {URL: e.URL(), Token: "runner-a"}}))
	r.initialize("s", pTok)
	return r, e, strings.TrimPrefix(e.ts.URL, "http://")
}

func noAnswerWant(target, kind string) string { return fmt.Sprintf(textNoAnswerFmt, target, kind) }

// ── (1) the agent's error holds no address and no error page; the log holds both ─────────────────────────────────────

func TestFollowups_ADownExecutorsErrorHoldsNoAddressAndTheLogHoldsIt(t *testing.T) {
	r, e, hostport := oneRunnerRig(t, Options{})
	e.ts.Close()
	raw := r.call("s", pTok, 30, "runner_get_report", map[string]any{"instance_id": "a"})
	requireNoAddress(t, raw, hostport)
	p := toolErrPayload(t, raw)
	if want := noAnswerWant("the executor of instance a", kindUnreachable); p["error"] != want || p["reached"] != false ||
		p["plane"] != "runner" || p["instance"] != "a" {
		t.Fatalf("got %v\nwant error %q, reached false, plane runner, instance a", p, want)
	}
	if logs := r.logged(); !strings.Contains(logs, hostport) || !strings.Contains(logs, "(instance a) got no usable answer") {
		t.Fatalf("the log does not hold the full text (with %s):\n%s", hostport, logs)
	}
}

// The executor goes away between initialize and tools/call: Go's error for the POST names the URL — the case the
// review found (client.go "tools/call: " + err.Error()).
func TestFollowups_AConnectionDroppedAtTheCallHoldsNoAddress(t *testing.T) {
	r, e, hostport := oneRunnerRig(t, Options{})
	e.setMode("calldrop")
	raw := r.call("s", pTok, 31, "runner_get_report", map[string]any{"instance_id": "a"})
	requireNoAddress(t, raw, hostport)
	if p := toolErrPayload(t, raw); p["error"] != noAnswerWant("the executor of instance a", kindUnreachable) {
		t.Fatalf("got %v", p)
	}
	if logs := r.logged(); !strings.Contains(logs, "tools/call") || !strings.Contains(logs, hostport) {
		t.Fatalf("the log does not hold Go's text for the call:\n%s", logs)
	}
}

func TestFollowups_AnErrorPageIsNotShownToTheAgentButIsLogged(t *testing.T) {
	const marker = "MARKER-ACD702-ERROR-PAGE <html>secret proxy at 10.1.2.3:8443</html>"
	for _, c := range []struct{ mode, stage string }{{"status:500", "initialize"}, {"callstatus:502", "tools/call"}} {
		t.Run(c.mode, func(t *testing.T) {
			r, e, hostport := oneRunnerRig(t, Options{})
			e.setStatusBody(marker)
			e.setMode(c.mode)
			raw := r.call("s", pTok, 32, "runner_get_report", map[string]any{"instance_id": "a"})
			requireNoAddress(t, raw, hostport, "MARKER-ACD702", "secret proxy")
			code := strings.TrimPrefix(c.mode[strings.Index(c.mode, ":"):], ":")
			if p := toolErrPayload(t, raw); p["error"] != noAnswerWant("the executor of instance a", fmt.Sprintf(kindHTTPFmt, atoi(t, code))) || p["reached"] != false {
				t.Fatalf("got %v", p)
			}
			logs := r.logged()
			if !strings.Contains(logs, "MARKER-ACD702-ERROR-PAGE") || !strings.Contains(logs, c.stage+": HTTP "+code) {
				t.Fatalf("the log does not hold the error page and its stage:\n%s", logs)
			}
		})
	}
}

func TestFollowups_ACallPastItsBoundSaysItDidNotAnswerInTime(t *testing.T) {
	r, e, hostport := oneRunnerRig(t, Options{RunnerCallTimeout: 300 * time.Millisecond})
	e.setMode("hang")
	start := time.Now()
	raw := r.call("s", pTok, 33, "runner_get_report", map[string]any{"instance_id": "a"})
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("the call took %s with a 300 ms bound", took)
	}
	requireNoAddress(t, raw, hostport)
	if p := toolErrPayload(t, raw); p["error"] != noAnswerWant("the executor of instance a", kindTimedOut) || p["reached"] != false {
		t.Fatalf("got %v", p)
	}
	// The log holds the client's full text: Go's error for the initialize that ran out of time, with the address.
	if logs := r.logged(); !strings.Contains(logs, "(instance a) got no usable answer: initialize: Post") ||
		!strings.Contains(logs, hostport) || !strings.Contains(logs, "deadline exceeded") {
		t.Fatalf("the log does not hold the full text:\n%s", logs)
	}
}

// The headers come and the body never does: still "did not answer in time" — the read path, not only the send path.
func TestFollowups_AnAnswerWhoseBodyNeverComesIsATimeout(t *testing.T) {
	r, e, hostport := oneRunnerRig(t, Options{RunnerCallTimeout: 300 * time.Millisecond})
	e.setMode("bodyhang")
	raw := r.call("s", pTok, 38, "runner_get_report", map[string]any{"instance_id": "a"})
	requireNoAddress(t, raw, hostport)
	if p := toolErrPayload(t, raw); p["error"] != noAnswerWant("the executor of instance a", kindTimedOut) || p["reached"] != false {
		t.Fatalf("got %v", p)
	}
	if logs := r.logged(); !strings.Contains(logs, "got no usable answer: tools/call read:") {
		t.Fatalf("the log does not hold the read failure:\n%s", logs)
	}
}

// passBack serves both planes: the author plane changes the same way.
func TestFollowups_TheAuthorPlaneHoldsNoAddressEither(t *testing.T) {
	t.Run("not reachable", func(t *testing.T) {
		h := newHarness(t, Options{})
		h.initialize("s", testTok)
		hostport := strings.TrimPrefix(h.cp.ts.URL, "http://")
		h.cp.ts.Close()
		raw := h.call("s", testTok, 34, "author_list_runs", map[string]any{"instance_id": "suta"})
		requireNoAddress(t, raw, hostport)
		if p := toolErrPayload(t, raw); p["error"] != noAnswerWant("the control plane (instance suta)", kindUnreachable) ||
			p["plane"] != "author" || p["reached"] != false || p["instance"] != "suta" {
			t.Fatalf("got %v", p)
		}
		if logs := strings.Join(h.logs, "\n"); !strings.Contains(logs, hostport) || !strings.Contains(logs, "router: a call to the author plane") {
			t.Fatalf("the log does not hold the full text:\n%s", logs)
		}
	})
	t.Run("HTTP 503 with a page", func(t *testing.T) {
		h := newHarness(t, Options{})
		h.initialize("s", testTok)
		h.cp.setMode("status:503")
		raw := h.call("s", testTok, 35, "author_list_runs", nil)
		requireNoAddress(t, raw, strings.TrimPrefix(h.cp.ts.URL, "http://"), "upstream proxy page")
		if p := toolErrPayload(t, raw); p["error"] != noAnswerWant("the control plane", fmt.Sprintf(kindHTTPFmt, 503)) {
			t.Fatalf("got %v", p)
		}
		if logs := strings.Join(h.logs, "\n"); !strings.Contains(logs, "upstream proxy page") {
			t.Fatalf("the log does not hold the page:\n%s", logs)
		}
	})
}

// No token in the agent's text or in the log line, whatever the failure.
func TestFollowups_NoTokenInTheTextOrTheLog(t *testing.T) {
	for _, mode := range []string{"", "calldrop", "status:500", "callstatus:500"} {
		t.Run("mode "+mode, func(t *testing.T) {
			r, e, _ := oneRunnerRig(t, Options{})
			if mode == "" {
				e.ts.Close()
			} else {
				e.setMode(mode)
			}
			raw := r.call("s", pTok, 36, "runner_get_report", map[string]any{"instance_id": "a"})
			for _, tok := range []string{"runner-a", "author-a", pTok} {
				if bytes.Contains(raw, []byte(tok)) || strings.Contains(r.logged(), tok) {
					t.Fatalf("token %q in the answer or the log:\n%s\n%s", tok, raw, r.logged())
				}
			}
		})
	}
}

// A tool-list failure quoting a long JSON-RPC message: the log line is bounded.
func TestFollowups_ALongListFailureIsCappedInTheLog(t *testing.T) {
	e := newStubExec(t, "0.3.73", "runner-a", "author-a")
	e.setRPCMessage(strings.Repeat("é", 50_000) + "THE-END-OF-THE-MESSAGE")
	e.setMode("rpcerr")
	r := newRig(t, Options{}, productFolder(pTok, map[string]Upstream{"a": {URL: e.URL(), Token: "runner-a"}}))
	if res := r.initialize("s", pTok); res["instructions"] != fmt.Sprintf(textExecErrorFmt, "a", "JSON-RPC error -32603") {
		t.Fatalf("instructions: %v", res["instructions"])
	}
	var line string
	for _, l := range strings.Split(r.logged(), "\n") {
		if strings.Contains(l, "could not be read") {
			line = l
		}
	}
	if line == "" || len(line) > 1500 || strings.Contains(line, "THE-END-OF-THE-MESSAGE") || !strings.Contains(line, "bytes in all") || !utf8.ValidString(line) {
		t.Fatalf("the list failure is not logged capped (%d bytes): %.300q", len(line), line)
	}
}

// The call's own log line is bounded too: Go's error quotes the whole URL, and a URL can be long.
func TestFollowups_TheCallsLogLineIsCapped(t *testing.T) {
	long := closedURL() + "/" + strings.Repeat("p", 3000)
	e := newStubExec(t, "0.3.73", "runner-a", "author-a")
	r := newRig(t, Options{}, productFolder(pTok, map[string]Upstream{"a": {URL: e.URL(), Token: "runner-a"}}))
	r.initialize("s", pTok)
	r.replace(productFolder(pTok, map[string]Upstream{"a": {URL: long, Token: "runner-a"}}))
	raw := r.call("s", pTok, 37, "runner_get_report", map[string]any{"instance_id": "a"})
	if p := toolErrPayload(t, raw); p["error"] != noAnswerWant("the executor of instance a", kindUnreachable) {
		t.Fatalf("got %v", p)
	}
	var line string
	for _, l := range strings.Split(r.logged(), "\n") {
		if strings.Contains(l, "got no usable answer") {
			line = l
		}
	}
	if line == "" || len(line) > 1500 || !strings.Contains(line, "bytes in all") {
		t.Fatalf("the call's log line is not capped (%d bytes): %.200q", len(line), line)
	}
}

func atoi(t *testing.T, s string) int {
	t.Helper()
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
		t.Fatal(err)
	}
	return n
}

// ── (2) a developer's own build is the newest ─────────────────────────────────────────────────────────────────────

func TestFollowups_ADevBuildsDefinitionsAreShownNextToARelease(t *testing.T) {
	for _, dev := range []string{"0.0.0-dev", "0.0.0-src+0123456789ab", "0.0.0-src+0123456789ab.dirty"} {
		for _, devID := range []string{"a", "b"} { // the dev build on the lower and on the higher instance id
			t.Run(dev+"_on_"+devID, func(t *testing.T) {
				relID := map[string]string{"a": "b", "b": "a"}[devID]
				d := newStubExec(t, dev, "runner-d", "author-d")
				d.serve(dev, "runner-d", "author-d", []mcpserver.Tool{execTool("runner_x", "the dev build's x", "devonly_arg"), execTool("runner_devonly", "dev only")})
				rel := newStubExec(t, "0.3.73", "runner-r", "author-r")
				rel.serve("0.3.73", "runner-r", "author-r", []mcpserver.Tool{execTool("runner_x", "the release's x"), execTool("runner_relonly", "release only")})
				r := newRig(t, Options{}, productFolder(pTok, map[string]Upstream{
					devID: {URL: d.URL(), Token: "runner-d"}, relID: {URL: rel.URL(), Token: "runner-r"}}))
				r.initialize("s", pTok)
				routed, _ := r.listed("s", pTok)
				dList, _ := executorList(t, d, "runner-d")
				if !bytes.Equal(routed["runner_x"], dList["runner_x"]) {
					t.Fatalf("runner_x is not the dev build's definition:\n%s", routed["runner_x"])
				}
				// Every call is still checked against the TARGET's own offer: the release is not sent a tool only
				// the dev build offers, and the release's own tool still works on it.
				if raw := r.call("s", pTok, 40, "runner_relonly", map[string]any{"instance_id": relID}); bytes.Contains(raw, []byte(`"isError":true`)) {
					t.Fatalf("the release's own tool was refused: %s", raw)
				}
				before := len(rel.toolCalls())
				got := toolErrorText(t, r.call("s", pTok, 41, "runner_devonly", map[string]any{"instance_id": relID}))
				if !strings.HasPrefix(got, "runner_devonly is not offered by the executor of instance "+relID) || len(rel.toolCalls()) != before {
					t.Fatalf("a dev-only tool on the release: %q (+%d calls)", got, len(rel.toolCalls())-before)
				}
			})
		}
	}
}

func TestFollowups_M25AndAnEmptyVersionStillLose(t *testing.T) {
	for _, c := range []struct{ old, other string }{{"M2.5", "0.3.1"}, {"M2.5", "0.0.0-dev"}, {"", "0.3.1"}, {"", "0.0.0-src+ab"}, {"m3-iii27", "0.3.1"}} {
		t.Run(c.old+"_vs_"+c.other, func(t *testing.T) {
			if !olderVersion(c.old, c.other) || olderVersion(c.other, c.old) {
				t.Fatalf("%q must be older than %q", c.old, c.other)
			}
		})
	}
}

func TestFollowups_TheVersionOrder(t *testing.T) {
	order := [][]string{ // rank by rank, oldest first; versions in one rank are equal
		{"", "M2.5", "m3-iii27"},
		{"0.3.1"},
		{"0.3.72-dev+abc"},
		{"0.3.72", "v0.3.72"},
		{"0.3.73"},
		{"0.0.0-dev", "0.0.0-src+abc", "0.0.0-src+abc.dirty"},
	}
	for i, ri := range order {
		for j, rj := range order {
			for _, a := range ri {
				for _, b := range rj {
					if got, want := olderVersion(a, b), i < j; got != want {
						t.Errorf("olderVersion(%q, %q) = %t, want %t", a, b, got, want)
					}
				}
			}
		}
	}
}

// ── (3) whole tool names only ─────────────────────────────────────────────────────────────────────────────────────

func TestFollowups_RespellTouchesWholeToolNamesOnly(t *testing.T) {
	for in, want := range map[string]string{
		"runner__run":                         "runner_run",
		"call runner__run, then author__x.":   "call runner_run, then author_x.",
		"(runner__get_report)":                "(runner_get_report)",
		"`author__list_runs`":                 "`author_list_runs`",
		"xrunner__y":                          "xrunner__y",
		"my_runner__x":                        "my_runner__x",
		"2runner__x":                          "2runner__x",
		"runner___x":                          "runner___x",
		"runner__":                            "runner__",
		"runner__Run":                         "runner__Run",
		"runner__run runner__get_report":      "runner_run runner_get_report",
		"line one\nrunner__run":               "line one\nrunner_run",
		"Xauthor__y and author__y":            "Xauthor__y and author_y",
		"runner_run stays, runner_x__y stays": "runner_run stays, runner_x__y stays",
	} {
		if got := respellText(in); got != want {
			t.Errorf("respellText(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFollowups_RespellThroughTheList(t *testing.T) {
	e := newStubExec(t, "0.3.73", "runner-a", "author-a")
	e.setRawList(`{"tools":[{"name":"runner__run","description":"xrunner__y, runner___x and runner__run","inputSchema":{"type":"object","properties":{"instance_id":{"type":"string","description":"see author__list_runs, not xauthor__z","default":"runner__run"}}}}]}`)
	r := newRig(t, Options{}, productFolder(pTok, map[string]Upstream{"a": {URL: e.URL(), Token: "runner-a"}}))
	r.initialize("s", pTok)
	routed, _ := r.listed("s", pTok)
	var d struct {
		Description string `json:"description"`
		InputSchema struct {
			Properties struct {
				InstanceID struct {
					Description string `json:"description"`
					Default     string `json:"default"`
				} `json:"instance_id"`
			} `json:"properties"`
		} `json:"inputSchema"`
	}
	if err := json.Unmarshal(routed["runner_run"], &d); err != nil {
		t.Fatal(err)
	}
	if d.Description != "xrunner__y, runner___x and runner_run" || d.InputSchema.Properties.InstanceID.Description != "see author_list_runs, not xauthor__z" ||
		d.InputSchema.Properties.InstanceID.Default != "runner__run" {
		t.Fatalf("got %+v", d)
	}
}

// ── (4) both spellings listed: one runner_run, the one-underscore entry ───────────────────────────────────────────────

func TestFollowups_BothSpellingsKeepTheOneUnderscoreNameInEitherOrder(t *testing.T) {
	two := `{"name":"runner__run","description":"the two-underscore entry","inputSchema":{"type":"object"}}`
	one := `{"name":"runner_run","description":"the one-underscore entry","inputSchema":{"type":"object"}}`
	other := `{"name":"runner_get_report","description":"r","inputSchema":{"type":"object"}}`
	for name, list := range map[string]string{
		"two then one": "[" + two + "," + other + "," + one + "]",
		"one then two": "[" + one + "," + other + "," + two + "]",
	} {
		t.Run(name, func(t *testing.T) {
			e := newStubExec(t, "0.3.73", "runner-a", "author-a")
			e.setRawList(`{"tools":` + list + `}`)
			r := newRig(t, Options{}, productFolder(pTok, map[string]Upstream{"a": {URL: e.URL(), Token: "runner-a"}}))
			r.initialize("s", pTok)
			routed, names := r.listed("s", pTok)
			if got := strings.Join(names, ","); got != "runner_run,runner_get_report" {
				t.Fatalf("listed %q, want one runner_run in its first position", got)
			}
			var d struct{ Description string }
			_ = json.Unmarshal(routed["runner_run"], &d)
			if d.Description != "the one-underscore entry" {
				t.Fatalf("runner_run shows %q", d.Description)
			}
			if raw := r.call("s", pTok, 50, "runner_run", map[string]any{"instance_id": "a"}); e.lastCall().Tool != "runner_run" {
				t.Fatalf("the call went out as %q: %s", e.lastCall().Tool, raw)
			}
			if logs := r.logged(); !strings.Contains(logs, `"runner__run"`) || !strings.Contains(logs, `"runner_run" is kept`) {
				t.Fatalf("the duplicate is not logged with what was kept:\n%s", logs)
			}
		})
	}
}

// Two entries of the SAME spelling: the first is kept, as before AC-D70.2.
func TestFollowups_TheSameSpellingTwiceKeepsTheFirst(t *testing.T) {
	for _, name := range []string{"runner_run", "runner__run"} {
		t.Run(name, func(t *testing.T) {
			first := `{"name":"` + name + `","description":"the first","inputSchema":{"type":"object"}}`
			second := `{"name":"` + name + `","description":"the second","inputSchema":{"type":"object"}}`
			e := newStubExec(t, "0.3.73", "runner-a", "author-a")
			e.setRawList(`{"tools":[` + first + `,` + second + `]}`)
			r := newRig(t, Options{}, productFolder(pTok, map[string]Upstream{"a": {URL: e.URL(), Token: "runner-a"}}))
			r.initialize("s", pTok)
			routed, names := r.listed("s", pTok)
			var d struct{ Description string }
			_ = json.Unmarshal(routed["runner_run"], &d)
			if strings.Join(names, ",") != "runner_run" || d.Description != "the first" {
				t.Fatalf("listed %v, runner_run shows %q — want one runner_run, the first entry", names, d.Description)
			}
		})
	}
}

// No tool disappears: a malformed one-underscore entry leaves the well-formed two-underscore one in place.
func TestFollowups_AMalformedOneUnderscoreEntryDoesNotHideTheOtherSpelling(t *testing.T) {
	two := `{"name":"runner__run","description":"the two-underscore entry","inputSchema":{"type":"object"}}`
	bad := `{"name":"runner_run","description":"malformed","inputSchema":"not a schema"}`
	for name, list := range map[string]string{"two then bad": "[" + two + "," + bad + "]", "bad then two": "[" + bad + "," + two + "]"} {
		t.Run(name, func(t *testing.T) {
			e := newStubExec(t, "0.3.73", "runner-a", "author-a")
			e.setRawList(`{"tools":` + list + `}`)
			r := newRig(t, Options{}, productFolder(pTok, map[string]Upstream{"a": {URL: e.URL(), Token: "runner-a"}}))
			r.initialize("s", pTok)
			routed, names := r.listed("s", pTok)
			var d struct{ Description string }
			_ = json.Unmarshal(routed["runner_run"], &d)
			if strings.Join(names, ",") != "runner_run" || d.Description != "the two-underscore entry" {
				t.Fatalf("listed %v, runner_run shows %q", names, d.Description)
			}
			if r.call("s", pTok, 51, "runner_run", map[string]any{"instance_id": "a"}); e.lastCall().Tool != "runner__run" {
				t.Fatalf("the call went out as %q, want the well-formed entry's name runner__run", e.lastCall().Tool)
			}
		})
	}
}
