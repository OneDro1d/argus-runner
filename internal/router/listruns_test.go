package router

// VR10-R2 / V28-016 — the local router must carry `author_list_runs` to the control plane, and an agent must be able to
// SEE its filter arguments, because a conforming agent sends only what it can see.
//
// Since AC-D69 the router declares nothing itself: the tool, its description and its schema — filters included — are
// the control plane's own, passed on unchanged. So this asserts that what the control plane declares is what the agent
// sees, and that the filters survive the hop.

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRouter_ListRunsSchemaDeclaresItsFilters(t *testing.T) {
	h := newHarness(t, Options{})
	h.initialize("s", testTok)
	routed, _ := h.listed("s", testTok)
	entry, ok := routed["author_list_runs"]
	if !ok {
		t.Fatalf("author_list_runs is not listed — the test agent's router cannot reach it")
	}
	var tl struct {
		InputSchema struct {
			Properties map[string]any `json:"properties"`
		} `json:"inputSchema"`
	}
	_ = json.Unmarshal(entry, &tl)
	for _, arg := range []string{"instance_id", "limit", "since"} { // what the stub control plane declares
		if _, ok := tl.InputSchema.Properties[arg]; !ok {
			t.Errorf("the router does not show %q — an agent that cannot see an argument never sends it", arg)
		}
	}
}

// The tool reaches the CLOUD plane with its filters intact, a test folder sees it, and a product folder can neither see
// it nor call it. The hat rule is the boundary; listing is the courtesy.
func TestRouter_ListRunsGoesToTheCloudPlaneForTheTestHatOnly(t *testing.T) {
	h := newHarness(t, Options{})
	h.initialize("t", testTok)
	h.initialize("p", prodTok)
	sees := func(names []string) bool {
		for _, n := range names {
			if n == "author_list_runs" || n == "author__list_runs" {
				return true
			}
		}
		return false
	}
	if _, names := h.listed("t", testTok); !sees(names) {
		t.Error("the TEST folder cannot see author_list_runs")
	}
	if _, names := h.listed("p", prodTok); sees(names) {
		t.Error("the PRODUCT folder can see author_list_runs — the author holdout is broken")
	}

	args := json.RawMessage(`{"instance_id":"suta","limit":"5","since":"2026-09-04T09:00:00Z"}`)
	h.call("t", testTok, 1, "author_list_runs", args)
	got := h.cp.lastCall()
	if got.Tool != "author_list_runs" {
		t.Errorf("forwarded as %q", got.Tool)
	}
	// The filters must survive the hop. A router that dropped them would return the newest 20 runs to a caller who
	// asked for 5 since a timestamp — a complete-looking answer to a different question.
	for _, want := range []string{"limit", "5", "since", "2026-09-04T09:00:00Z"} {
		if !strings.Contains(string(got.Args), want) {
			t.Errorf("the upstream never saw %q; it received: %s", want, got.Args)
		}
	}

	before := len(h.cp.calls)
	if raw := h.call("p", prodTok, 2, "author_list_runs", json.RawMessage(`{"instance_id":"suta"}`)); !strings.Contains(string(raw), `"code":-32001`) {
		t.Fatalf("a PRODUCT folder naming author_list_runs directly was NOT refused: %s", raw)
	}
	if len(h.cp.calls) != before {
		t.Fatal("the refused product call reached the control plane")
	}
}
