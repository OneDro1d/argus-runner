package router

// VR10-R2 / V28-016 — the local router must carry `author__list_runs` to the control plane, and it
// must declare the two arguments that are new to the author plane.
//
// The router's schema is OPEN by design (the upstream owns its own contract, and a second copy here
// would be a second contract to drift). Openness is why an UNDECLARED argument is not refused here —
// and exactly why one must be DECLARED: a conforming agent sends only what it can see, so an argument
// missing from this list is an argument the test agent never gets to use.

import (
	"strings"
	"testing"
)

func TestRouter_ListRunsSchemaDeclaresItsFilters(t *testing.T) {
	found := false
	for _, name := range AuthorTools {
		if name == "author__list_runs" {
			found = true
		}
	}
	if !found {
		t.Fatalf("author__list_runs is not in AuthorTools — the test agent's router cannot reach it: %v", AuthorTools)
	}
	props, _ := schemaFor("author__list_runs")["properties"].(map[string]any)
	for _, arg := range []string{"instance_id", "limit", "since", "layer", "tag"} {
		if _, ok := props[arg]; !ok {
			t.Errorf("the router's schema does not declare %q — an agent that cannot see an argument never sends it", arg)
		}
	}
}

// The tool reaches the CLOUD plane with its filters intact, a test folder sees it, and a product
// folder can neither see it nor call it. The hat rule is the boundary; listing is the courtesy.
func TestRouter_ListRunsGoesToTheCloudPlaneForTheTestHatOnly(t *testing.T) {
	tbl := testTable(t)
	fwd := &fakeFwd{}
	srv := serverFor(t, tbl, fwd)

	testPrin, err := Authenticator(tbl).Authenticate(testTok)
	if err != nil {
		t.Fatal(err)
	}
	prodPrin, err := Authenticator(tbl).Authenticate(prodTok)
	if err != nil {
		t.Fatal(err)
	}

	sees := func(names []string) bool {
		for _, n := range names {
			if n == "author__list_runs" {
				return true
			}
		}
		return false
	}
	if !sees(toolNames(srv, testPrin)) {
		t.Error("the TEST folder cannot see author__list_runs")
	}
	if sees(toolNames(srv, prodPrin)) {
		t.Error("the PRODUCT folder can see author__list_runs — the author holdout is broken")
	}

	args := []byte(`{"instance_id":"suta","limit":"5","since":"2026-09-04T09:00:00Z"}`)
	if o := proxy(tbl, fwd, "author__list_runs", args, testPrin); o.IsError {
		t.Fatalf("the test folder was refused: %+v", o)
	}
	if fwd.lastTarget.Plane != "author" {
		t.Errorf("author__list_runs was routed to the %q plane, want the author (control) plane", fwd.lastTarget.Plane)
	}
	if fwd.lastTool != "author__list_runs" {
		t.Errorf("forwarded as %q", fwd.lastTool)
	}
	// The filters must survive the hop. A router that dropped them would return the newest 20 runs to
	// a caller who asked for 5 since a timestamp — a complete-looking answer to a different question.
	for _, want := range []string{"limit", "5", "since", "2026-09-04T09:00:00Z"} {
		if !strings.Contains(string(fwd.lastArgs), want) {
			t.Errorf("the upstream never saw %q; it received: %s", want, fwd.lastArgs)
		}
	}

	if o := checkOnly(tbl, "author__list_runs", []byte(`{"instance_id":"suta"}`), prodPrin); o == nil {
		t.Fatal("a PRODUCT folder naming author__list_runs directly was NOT refused")
	}
}
