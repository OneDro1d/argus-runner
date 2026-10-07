package router

import (
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/role"
)

// The routing table is the security core of the local router (VR-R4/R5/R7/R13, and VR-P7 above them
// all). Everything else in the package is transport.
//
// WHAT THE ROUTER IS FOR. A .mcp.json key maps to ONE url, but every instance's runner is a separate
// endpoint on its own port (measured: compose 8765/8767/8768; k3d+aks 50765/64617/53905). N SUTs in
// one folder therefore need N entries — unless something routes by instance_id.
//
// WHAT MAKES IT DANGEROUS. Measured on disk, the SAME instance from two folders:
//
//	TEST agent -> localhost:8765/sse   Bearer author-425e158c…
//	PROD agent -> localhost:8765/sse   Bearer runner-fd9e7bdc…
//
// Same endpoint, DIFFERENT tokens — that is the dark-factory holdout enforced by token scope. A
// router that held "the instance's token" and injected it would silently start feeding EXPECT values
// to the product agent.

const (
	prodTok  = "odtr_product_folder_token_aaaaaaaaaaaaaaaa"
	testTok  = "odtr_test_folder_token_bbbbbbbbbbbbbbbbbb"
	otherTok = "odtr_other_folder_token_cccccccccccccccc"
)

func testTable(t *testing.T) *Table {
	t.Helper()
	tbl := New()
	// the PRODUCT folder: runner-scoped upstreams only, and no cloud credential exists for it
	if err := tbl.AddFolder(Folder{
		Path: "C:/work/product-agent", Hat: role.Product, Token: prodTok,
		Upstreams: map[string]Upstream{
			"suta": {URL: "http://127.0.0.1:8765", Token: "runner-suta"},
			"sutb": {URL: "http://127.0.0.1:8767", Token: "runner-sutb"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	// the TEST folder: author-scoped upstreams for the same instances, PLUS the cloud plane
	if err := tbl.AddFolder(Folder{
		Path: "C:/work/test-agent", Hat: role.Test, Token: testTok,
		Upstreams: map[string]Upstream{
			"suta": {URL: "http://127.0.0.1:8765", Token: "author-suta"},
			"sutb": {URL: "http://127.0.0.1:8767", Token: "author-sutb"},
		},
		Cloud: &CloudRef{URL: "https://argus-dev.onedroid.ai/mcp", User: "u"}, record: &CloudRecord{URL: "https://argus-dev.onedroid.ai/mcp", User: "u", Token: "odts_user_global"},
	}); err != nil {
		t.Fatal(err)
	}
	// a DIFFERENT folder on the same machine, holding a different SUT entirely
	if err := tbl.AddFolder(Folder{
		Path: "C:/elsewhere/test-agent", Hat: role.Test, Token: otherTok,
		Upstreams: map[string]Upstream{"sutz": {URL: "http://127.0.0.1:8768", Token: "author-sutz"}},
		Cloud:     &CloudRef{URL: "https://argus-dev.onedroid.ai/mcp", User: "u"}, record: &CloudRecord{URL: "https://argus-dev.onedroid.ai/mcp", User: "u", Token: "odts_user_global"},
	}); err != nil {
		t.Fatal(err)
	}
	return tbl
}

// ── VR-R7 / TS-R1: THE HOLDOUT TEST — the single most important test in the build ───────────────

func TestVisibleTools_ProductFolderNeverSeesAuthorTools(t *testing.T) {
	tbl := testTable(t)
	f, err := tbl.Resolve(prodTok)
	if err != nil {
		t.Fatal(err)
	}
	tools := f.VisibleTools()
	if len(tools) == 0 {
		t.Fatal("a product folder must still see the runner tools")
	}
	for _, name := range tools {
		if strings.HasPrefix(name, "author__") {
			t.Errorf("author tool %q is VISIBLE to the product hat — the holdout is broken", name)
		}
	}
	// and the test folder DOES see them, or the router is useless
	tf, _ := tbl.Resolve(testTok)
	var sawAuthor bool
	for _, name := range tf.VisibleTools() {
		if strings.HasPrefix(name, "author__") {
			sawAuthor = true
		}
	}
	if !sawAuthor {
		t.Error("the test hat must see the author tools")
	}
}

// Absence from tools/list is not enough on its own: a client that already knows the name must also be
// refused when it CALLS it. Listing is a courtesy; the refusal is the boundary.
func TestRoute_ProductFolderCallingAnAuthorToolIsRefused(t *testing.T) {
	tbl := testTable(t)
	f, _ := tbl.Resolve(prodTok)
	_, err := f.Route("author__read_scenario", "suta")
	if err == nil {
		t.Fatal("a product folder called an author tool and was NOT refused")
	}
	if !strings.Contains(err.Error(), "not available") && !strings.Contains(err.Error(), "runner") {
		t.Errorf("the refusal should explain the scope; got %v", err)
	}
}

// VR-R4's "structurally incapable". This is the rule that survives a routing BUG: a product folder
// must not merely decline to use an author credential — it must not HAVE one. Then a mistake in the
// routing logic produces a failed lookup, not a leak.
func TestAddFolder_AProductFolderCannotHoldACloudCredential(t *testing.T) {
	tbl := New()
	err := tbl.AddFolder(Folder{
		Path: "C:/work/product-agent", Hat: role.Product, Token: prodTok,
		Upstreams: map[string]Upstream{"suta": {URL: "http://127.0.0.1:8765", Token: "runner-suta"}},
		Cloud:     &CloudRef{URL: "https://argus-dev.onedroid.ai/mcp", User: "u"}, record: &CloudRecord{URL: "https://argus-dev.onedroid.ai/mcp", User: "u", Token: "odts_user_global"},
	})
	if err == nil {
		t.Fatal("a PRODUCT folder was allowed to hold an author-plane credential — VR-R4 is not structural")
	}
	if !strings.Contains(err.Error(), "product") {
		t.Errorf("the refusal must name the hat; got %v", err)
	}
}

// The product folder's own record must contain no author-plane credential at all, however you reach
// for it. Belt and braces with the test above, because this is the one that would be checked by
// someone auditing the stored state rather than the constructor.
func TestFolder_ProductHasNoCloudCredential(t *testing.T) {
	tbl := testTable(t)
	f, _ := tbl.Resolve(prodTok)
	if f.CloudCredential() != "" {
		t.Error("a product folder resolved to a cloud credential")
	}
	tf, _ := tbl.Resolve(testTok)
	if tf.CloudCredential() == "" {
		t.Error("the test folder must hold the cloud credential — it is the author plane's only route")
	}
}

// ── VR-R5 / TS-R2: per-folder scoping ───────────────────────────────────────────────────────────

func TestRoute_CannotReachAnInstanceFromAnotherFolder(t *testing.T) {
	tbl := testTable(t)
	f, _ := tbl.Resolve(testTok) // holds suta + sutb, NOT sutz
	_, err := f.Route("runner__run", "sutz")
	if err == nil {
		t.Fatal("folder A reached an instance onboarded into folder B")
	}
	if !strings.Contains(err.Error(), "sutz") || !strings.Contains(err.Error(), "folder") {
		t.Errorf("the refusal must name the instance and say it is not in this folder; got %v", err)
	}
}

// Not discoverable either: an out-of-folder instance must not appear in anything the folder can
// enumerate, or the refusal above just tells an attacker what to go looking for.
func TestFolder_InstancesAreScopedToTheFolder(t *testing.T) {
	tbl := testTable(t)
	f, _ := tbl.Resolve(testTok)
	got := f.Instances()
	if len(got) != 2 {
		t.Fatalf("Instances() = %v, want exactly this folder's two", got)
	}
	for _, id := range got {
		if id == "sutz" {
			t.Error("another folder's instance is discoverable")
		}
	}
}

// ── VR-R13 / TS-R5: instance_id names exactly ONE instance ──────────────────────────────────────

// The plausible agent error is a joined string, and the plausible FAILURE is a half-run reported as a
// success. A loud refusal is the whole point.
func TestRoute_JoinedInstanceIDsAreRefusedLoudly(t *testing.T) {
	tbl := testTable(t)
	f, _ := tbl.Resolve(testTok)
	for _, bad := range []string{"suta,sutb", "suta sutb", "suta;sutb", "suta, sutb"} {
		_, err := f.Route("runner__run", bad)
		if err == nil {
			t.Errorf("Route(%q) was accepted — a half-run would be reported as success", bad)
			continue
		}
		if !strings.Contains(err.Error(), "exactly one") {
			t.Errorf("Route(%q): the refusal must say one instance per call; got %v", bad, err)
		}
	}
}

func TestRoute_EmptyInstanceIDIsRefused(t *testing.T) {
	tbl := testTable(t)
	f, _ := tbl.Resolve(testTok)
	if _, err := f.Route("runner__run", ""); err == nil {
		t.Fatal("an empty instance_id was accepted")
	}
}

// ── the routing itself ──────────────────────────────────────────────────────────────────────────

// VR-R4, positively stated: the SAME instance resolves to DIFFERENT upstream tokens depending on the
// hat of the folder the call came from. That difference IS the holdout.
func TestRoute_SameInstanceDifferentHatDifferentToken(t *testing.T) {
	tbl := testTable(t)
	pf, _ := tbl.Resolve(prodTok)
	tf, _ := tbl.Resolve(testTok)

	pt, err := pf.Route("runner__get_report", "suta")
	if err != nil {
		t.Fatal(err)
	}
	tt, err := tf.Route("runner__get_report", "suta")
	if err != nil {
		t.Fatal(err)
	}
	if pt.URL != tt.URL {
		t.Errorf("the two hats should reach the SAME endpoint; got %q vs %q", pt.URL, tt.URL)
	}
	if pt.Token == tt.Token {
		t.Fatal("the two hats resolved to the SAME upstream token — the product agent would receive unredacted reports")
	}
	if pt.Token != "runner-suta" || tt.Token != "author-suta" {
		t.Errorf("product=%q test=%q; want the per-hat tokens", pt.Token, tt.Token)
	}
}

// An author tool from a test folder goes to the CLOUD, with the user-global token — not to the
// instance's endpoint. runner__* stays local because it must reach the SUT (VR-C4).
func TestRoute_AuthorToolGoesToTheCloudPlane(t *testing.T) {
	tbl := testTable(t)
	f, _ := tbl.Resolve(testTok)
	tgt, err := f.Route("author__list_scenarios", "suta")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(tgt.URL, "/mcp") || !strings.Contains(tgt.URL, "onedroid.ai") {
		t.Errorf("author tool routed to %q, want the control plane", tgt.URL)
	}
	if tgt.Token != "odts_user_global" {
		t.Errorf("author tool carried %q, want the user-global author token", tgt.Token)
	}
	// …and the scope check still ran first: an out-of-folder instance is refused BEFORE the cloud
	// is contacted, so the CP never sees a call the folder had no business making.
	if _, err := f.Route("author__list_scenarios", "sutz"); err == nil {
		t.Error("an author call for an out-of-folder instance reached the cloud")
	}
}

func TestResolve_UnknownTokenIsRefused(t *testing.T) {
	tbl := testTable(t)
	if _, err := tbl.Resolve("odtr_not_a_real_token"); err == nil {
		t.Fatal("an unknown router token was accepted")
	}
	if _, err := tbl.Resolve(""); err == nil {
		t.Fatal("an empty router token was accepted")
	}
}

// Two folders cannot share a router token: the token IS the folder's identity, and a collision would
// make "which folder is this?" unanswerable — which is the question every scope check depends on.
func TestAddFolder_DuplicateTokenIsRefused(t *testing.T) {
	tbl := testTable(t)
	err := tbl.AddFolder(Folder{
		Path: "C:/somewhere/else", Hat: role.Test, Token: testTok,
		Upstreams: map[string]Upstream{"sutq": {URL: "http://127.0.0.1:9999", Token: "author-sutq"}},
	})
	if err == nil {
		t.Fatal("two folders were allowed to share a router token")
	}
}

// An unknown tool is refused before any routing decision — the router forwards a fixed, known set,
// never "whatever the caller asked for".
func TestRoute_UnknownToolIsRefused(t *testing.T) {
	tbl := testTable(t)
	f, _ := tbl.Resolve(testTok)
	if _, err := f.Route("runner__delete_everything", "suta"); err == nil {
		t.Fatal("an unknown tool name was routed")
	}
}
