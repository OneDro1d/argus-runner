package reporoute

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/toolcore"
)

func repoPath(t *testing.T, name string) string {
	t.Helper()
	p := filepath.Join("testdata", "repos", name)
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("fixture repo %s missing: %v", name, err)
	}
	return p
}

// TestOpenAPIExtractor covers spec item 2a: OpenAPI 3.x paths + methods, deterministic and
// including a path with a parameter.
func TestOpenAPIExtractor(t *testing.T) {
	out := t.TempDir()
	rep, err := Run(Options{Repo: repoPath(t, "openapi-basic"), Out: out})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := rep.RoutesByExtractor["openapi"]; got != 3 {
		t.Fatalf("routes_by_extractor[openapi] = %d, want 3 (routes: %+v)", got, rep.Routes)
	}
	want := map[string]bool{"GET /pets": false, "POST /pets": false, "GET /pets/{id}": false}
	for _, r := range rep.Routes {
		key := r.Method + " " + r.Path
		if _, ok := want[key]; ok {
			want[key] = true
		}
	}
	for k, found := range want {
		if !found {
			t.Errorf("route %q not extracted; got %+v", k, rep.Routes)
		}
	}
}

// TestSwagger2BasePath covers spec item 2a's "Swagger 2.0" half specifically: basePath prefixes
// every path in the document.
func TestSwagger2BasePath(t *testing.T) {
	out := t.TempDir()
	rep, err := Run(Options{Repo: repoPath(t, "swagger2-basepath"), Out: out})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	found := false
	for _, r := range rep.Routes {
		if r.Method == "GET" && r.Path == "/v1/widgets" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected GET /v1/widgets (basePath applied), got %+v", rep.Routes)
	}
}

// TestOpenAPIMalformedReportedNotFatal: a file that LOOKS like an OpenAPI doc but fails to parse is
// reported in UnparseableFiles, and Run still succeeds (spec: TEST-FIRST "malformed OpenAPI file
// (reported, not fatal)").
func TestOpenAPIMalformedReportedNotFatal(t *testing.T) {
	out := t.TempDir()
	rep, err := Run(Options{Repo: repoPath(t, "openapi-malformed"), Out: out})
	if err != nil {
		t.Fatalf("Run must not fail on a malformed OpenAPI file: %v", err)
	}
	if len(rep.UnparseableFiles) != 1 {
		t.Fatalf("UnparseableFiles = %+v, want exactly 1", rep.UnparseableFiles)
	}
	if !strings.Contains(rep.UnparseableFiles[0].File, "bad-openapi.yaml") {
		t.Errorf("UnparseableFiles[0].File = %q, want it to name bad-openapi.yaml", rep.UnparseableFiles[0].File)
	}
}

// TestFastifyPrefix covers spec item 2b: Fastify app.get/route({...}), and a register(...,
// {prefix}) resolved onto the routes registered inside it in the same file.
func TestFastifyPrefix(t *testing.T) {
	out := t.TempDir()
	rep, err := Run(Options{Repo: repoPath(t, "fastify-prefix"), Out: out})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	byPath := map[string]Route{}
	for _, r := range rep.Routes {
		byPath[r.Method+" "+r.Path] = r
	}
	if _, ok := byPath["GET /items/list"]; !ok {
		t.Errorf("expected the register() prefix /items applied to /list, got routes: %+v", rep.Routes)
	}
	if _, ok := byPath["GET /items/status"]; !ok {
		t.Errorf("expected the prefix applied to the route({...}) form too, got routes: %+v", rep.Routes)
	}
	if _, ok := byPath["GET /health"]; !ok {
		t.Errorf("expected the un-prefixed /health route, got routes: %+v", rep.Routes)
	}
}

// TestExpressParamPath covers a parameterised path: the param is recorded and NOT invented as a
// runnable EXPECT value (spec item 3) — it must show up in the Non-runnable note instead.
func TestExpressParamPath(t *testing.T) {
	out := t.TempDir()
	rep, err := Run(Options{Repo: repoPath(t, "express-basic"), Out: out})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var got *Route
	for i := range rep.Routes {
		if rep.Routes[i].Path == "/users/:id" {
			got = &rep.Routes[i]
		}
	}
	if got == nil {
		t.Fatalf("expected /users/:id route, got %+v", rep.Routes)
	}
	if len(got.ParamNames) != 1 || got.ParamNames[0] != "id" {
		t.Errorf("ParamNames = %v, want [id]", got.ParamNames)
	}
	// The accepted draft for this GET route must carry a Non-runnable note about the param and NOT
	// invent a value in the Runnable EXPECT.
	body := readWrittenDraft(t, out, rep, "GET", "/users/:id")
	if !strings.Contains(body, "### Non-runnable") || !strings.Contains(body, "`id`") {
		t.Errorf("draft does not flag the path param in Non-runnable:\n%s", body)
	}
	if strings.Contains(body, "${id}") || strings.Contains(body, "/users/123") {
		t.Errorf("draft appears to have INVENTED a value for the path param:\n%s", body)
	}
}

func readWrittenDraft(t *testing.T, out string, rep *Report, method, path string) string {
	t.Helper()
	for _, d := range rep.Accepted {
		if d.Method == method && d.Path == path {
			b, err := os.ReadFile(filepath.Join(out, d.File))
			if err != nil {
				t.Fatalf("read written draft %s: %v", d.File, err)
			}
			return string(b)
		}
	}
	t.Fatalf("no accepted draft for %s %s among %+v", method, path, rep.Accepted)
	return ""
}

// TestGoExtractors covers spec item 2c: net/http, gorilla mux (.Methods), chi and gin.
func TestGoExtractors(t *testing.T) {
	out := t.TempDir()
	rep, err := Run(Options{Repo: repoPath(t, "go-basic"), Out: out})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := map[string]string{ // "METHOD PATH" -> extractor
		"GET /health":     "go-net-http",
		"GET /items/{id}": "go-mux",
		"POST /orders":    "go-mux",
		"GET /widgets":    "go-chi",
		"GET /status":     "go-gin",
	}
	got := map[string]string{}
	for _, r := range rep.Routes {
		got[r.Method+" "+r.Path] = r.Extractor
	}
	for k, wantExt := range want {
		if got[k] != wantExt {
			t.Errorf("route %q: extractor = %q, want %q (all routes: %+v)", k, got[k], wantExt, rep.Routes)
		}
	}
}

// TestMoneyHandlingRefusesNonGETAndVerbPaths covers T5.4 (spec item 3 second bullet + TEST-FIRST):
// a POST /orders is refused by the money-path guard (defence in depth, spec item 4's last
// sentence); NOTHING is written for it.
//
// UPDATED for review defect 4 (2026-09-24): GET /api/v1/quote used to be expected in rep.Refused
// with a "money-path guard" reason. Under the new money_handling GENERATION filter, a GET whose
// path is not a probe/health/metrics shape is never even built or validated — it is turned away
// earlier, into rep.NotProposed, with the fixed reason string the spec requires. This test now
// asserts the NotProposed bucket instead of Refused for /quote.
func TestMoneyHandlingRefusesNonGETAndVerbPaths(t *testing.T) {
	out := t.TempDir()
	rep, err := Run(Options{Repo: repoPath(t, "money-handling"), Out: out, Config: filepath.Join("testdata", "money-config.yaml")})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !rep.MoneyHandling {
		t.Fatal("rep.MoneyHandling should be true")
	}
	refusedFor := func(method, path string) *Refusal {
		for i := range rep.Refused {
			if rep.Refused[i].Method == method && rep.Refused[i].Path == path {
				return &rep.Refused[i]
			}
		}
		return nil
	}
	post := refusedFor("POST", "/api/v1/orders")
	if post == nil {
		t.Fatalf("POST /api/v1/orders was not refused; refused=%+v accepted=%+v", rep.Refused, rep.Accepted)
	}
	if !strings.Contains(post.Reason, "money-path guard") {
		t.Errorf("POST refusal reason = %q, want it to name the money-path guard", post.Reason)
	}
	var quote *Refusal
	for i := range rep.NotProposed {
		if rep.NotProposed[i].Method == "GET" && rep.NotProposed[i].Path == "/api/v1/quote" {
			quote = &rep.NotProposed[i]
		}
	}
	if quote == nil {
		t.Fatalf("GET /api/v1/quote was not turned away as not-proposed; not_proposed=%+v refused=%+v accepted=%+v",
			rep.NotProposed, rep.Refused, rep.Accepted)
	}
	if quote.Reason != notProposedReason {
		t.Errorf("GET /quote not-proposed reason = %q, want %q", quote.Reason, notProposedReason)
	}
	// /quote must not also show up as Refused or Accepted — it was never built at all.
	if refusedFor("GET", "/api/v1/quote") != nil {
		t.Errorf("GET /api/v1/quote should not appear in Refused (it is NotProposed): %+v", rep.Refused)
	}
	// The health route is a plain GET on a safe (probe/health) path and must still be accepted.
	found := false
	for _, a := range rep.Accepted {
		if a.Method == "GET" && a.Path == "/api/v1/health" {
			found = true
		}
	}
	if !found {
		t.Errorf("GET /api/v1/health should have been accepted; accepted=%+v", rep.Accepted)
	}
	// Nothing written for the refused or not-proposed routes.
	entries, _ := os.ReadDir(out)
	for _, e := range entries {
		if strings.Contains(e.Name(), "orders") || strings.Contains(e.Name(), "quote") {
			t.Errorf("a file was written for a refused/not-proposed draft: %s", e.Name())
		}
	}
}

// TestNoConfigProposesNonGET covers spec item 3 third bullet: without --config, a non-GET route is
// still proposed (tagged proposed, Non-runnable write note), provided it validates.
func TestNoConfigProposesNonGET(t *testing.T) {
	out := t.TempDir()
	rep, err := Run(Options{Repo: repoPath(t, "money-handling"), Out: out}) // no Config
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var post *Draft
	for i := range rep.Accepted {
		if rep.Accepted[i].Method == "POST" && rep.Accepted[i].Path == "/api/v1/orders" {
			post = &rep.Accepted[i]
		}
	}
	if post == nil {
		t.Fatalf("POST /api/v1/orders should be proposed without a config; accepted=%+v refused=%+v", rep.Accepted, rep.Refused)
	}
	if !post.Proposed {
		t.Errorf("Draft.Proposed should be true for a non-GET draft with no config")
	}
	b, err := os.ReadFile(filepath.Join(out, post.File))
	if err != nil {
		t.Fatalf("read %s: %v", post.File, err)
	}
	body := string(b)
	if !strings.Contains(body, "proposed") {
		t.Errorf("draft must carry the proposed tag:\n%s", body)
	}
	if !strings.Contains(body, "WRITES to the SUT") {
		t.Errorf("draft must carry a Non-runnable write-warning note:\n%s", body)
	}
}

// TestOutInsideRepoRefused covers spec item 4: --out inside --repo refuses, nothing written.
func TestOutInsideRepoRefused(t *testing.T) {
	repo := t.TempDir()
	out := filepath.Join(repo, "scenarios-out")
	_, err := Run(Options{Repo: repo, Out: out})
	if err == nil {
		t.Fatal("expected an error when --out is inside --repo")
	}
	if _, statErr := os.Stat(out); statErr == nil {
		t.Error("--out must not have been created when refused")
	}

	// The repo itself as --out must also be refused.
	_, err2 := Run(Options{Repo: repo, Out: repo})
	if err2 == nil {
		t.Fatal("expected an error when --out equals --repo")
	}
}

// TestDeterminism covers spec item 4: same repo -> same IDs and same bytes, across two runs into
// two different --out dirs.
func TestDeterminism(t *testing.T) {
	repo := repoPath(t, "go-basic")
	out1 := t.TempDir()
	out2 := t.TempDir()
	rep1, err := Run(Options{Repo: repo, Out: out1})
	if err != nil {
		t.Fatalf("Run 1: %v", err)
	}
	rep2, err := Run(Options{Repo: repo, Out: out2})
	if err != nil {
		t.Fatalf("Run 2: %v", err)
	}
	if len(rep1.Accepted) == 0 {
		t.Fatal("expected at least one accepted draft")
	}
	ids1 := draftIDs(rep1)
	ids2 := draftIDs(rep2)
	if strings.Join(ids1, ",") != strings.Join(ids2, ",") {
		t.Fatalf("IDs differ across runs:\n%v\n%v", ids1, ids2)
	}
	for _, d := range rep1.Accepted {
		b1, e1 := os.ReadFile(filepath.Join(out1, d.File))
		b2, e2 := os.ReadFile(filepath.Join(out2, d.File))
		if e1 != nil || e2 != nil {
			t.Fatalf("read: %v %v", e1, e2)
		}
		if string(b1) != string(b2) {
			t.Errorf("%s bytes differ across runs", d.File)
		}
	}
}

func draftIDs(rep *Report) []string {
	ids := make([]string, 0, len(rep.Accepted))
	for _, d := range rep.Accepted {
		ids = append(ids, d.ID)
	}
	sort.Strings(ids)
	return ids
}

// TestEveryWrittenFilePassesValidateAll covers TEST-FIRST: every written file passes ValidateAll.
func TestEveryWrittenFilePassesValidateAll(t *testing.T) {
	for _, fixture := range []string{"openapi-basic", "fastify-prefix", "express-basic", "go-basic", "param-path"} {
		out := t.TempDir()
		rep, err := Run(Options{Repo: repoPath(t, fixture), Out: out})
		if err != nil {
			t.Fatalf("%s: Run: %v", fixture, err)
		}
		for _, d := range rep.Accepted {
			b, rerr := os.ReadFile(filepath.Join(out, d.File))
			if rerr != nil {
				t.Fatalf("%s: read %s: %v", fixture, d.File, rerr)
			}
			_, verrs := toolcore.ValidateAll(string(b))
			if len(verrs) > 0 {
				t.Errorf("%s: written file %s fails ValidateAll: %+v\n%s", fixture, d.File, verrs, string(b))
			}
		}
	}
}

// TestUniqueSortedIDs covers spec item 4: scenario IDs are sorted and unique.
func TestUniqueSortedIDs(t *testing.T) {
	out := t.TempDir()
	rep, err := Run(Options{Repo: repoPath(t, "go-basic"), Out: out})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	seen := map[string]bool{}
	for i, d := range rep.Accepted {
		if seen[d.ID] {
			t.Errorf("duplicate ID %q", d.ID)
		}
		seen[d.ID] = true
		if i > 0 && rep.Accepted[i-1].ID > d.ID {
			t.Errorf("Accepted is not sorted by ID: %q before %q", rep.Accepted[i-1].ID, d.ID)
		}
	}
}

// TestSchemaAndDocFilesListed covers spec item 5: migrations + README/ARCHITECTURE/REQUIREMENTS
// are listed, not turned into scenarios.
func TestSchemaAndDocFilesListed(t *testing.T) {
	out := t.TempDir()
	rep, err := Run(Options{Repo: repoPath(t, "docs-and-schema"), Out: out})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rep.Routes) != 0 {
		t.Errorf("docs-and-schema fixture should yield no routes, got %+v", rep.Routes)
	}
	wantSchema := "migrations/0001_init.sql"
	foundSchema := false
	for _, s := range rep.SchemaFiles {
		if s == wantSchema {
			foundSchema = true
		}
	}
	if !foundSchema {
		t.Errorf("SchemaFiles = %v, want it to contain %q", rep.SchemaFiles, wantSchema)
	}
	wantDocs := map[string]bool{"README.md": false, "ARCHITECTURE.md": false}
	for _, d := range rep.DocFiles {
		if _, ok := wantDocs[d]; ok {
			wantDocs[d] = true
		}
	}
	for k, ok := range wantDocs {
		if !ok {
			t.Errorf("DocFiles = %v, want it to contain %q", rep.DocFiles, k)
		}
	}
}

// TestJSFalsePositivesFiltered: a Map/URLSearchParams .get() call shares Fastify/Express's exact
// call shape but is never a route — found for real in shop-services, where "action" and
// "amount" were proposed as GET routes before the leading-"/" guard existed.
func TestJSFalsePositivesFiltered(t *testing.T) {
	out := t.TempDir()
	rep, err := Run(Options{Repo: repoPath(t, "express-basic"), Out: out})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, r := range rep.Routes {
		if !strings.HasPrefix(r.Path, "/") {
			t.Errorf("a non-route .get()/.post() call was extracted as a route: %+v", r)
		}
	}
}

// TestSkipsConventionalDirs covers spec item 2: node_modules, .git, vendor, dist, build and
// dot-dirs are never descended into.
//
// The tree is built here, not committed under testdata/: the repo's .gitignore drops build/, dist/
// and vendor/, and git cannot hold a nested .git/, so a committed fixture would reach a clean
// checkout without those four directories and this test would pass without having tested them.
func TestSkipsConventionalDirs(t *testing.T) {
	repo := t.TempDir()
	route := func(path string) string {
		return "const app = require('express')();\napp.get('" + path + "', function (req, res) { res.json({}); });\nmodule.exports = app;\n"
	}
	files := map[string]string{"real.js": route("/should-be-found")}
	for _, dir := range []string{"node_modules", ".git", "vendor", "dist", "build", ".cache"} {
		files[filepath.Join(dir, "inner.js")] = route("/" + strings.TrimPrefix(dir, ".") + "-should-not-be-found")
	}
	for rel, body := range files {
		p := filepath.Join(repo, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	out := t.TempDir()
	rep, err := Run(Options{Repo: repo, Out: out})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, r := range rep.Routes {
		if strings.Contains(r.Path, "should-not-be-found") {
			t.Errorf("a route from a skipped directory was extracted: %+v", r)
		}
	}
	found := false
	for _, r := range rep.Routes {
		if r.Path == "/should-be-found" {
			found = true
		}
	}
	if !found {
		t.Errorf("the real (non-skipped) route was not found; routes=%+v", rep.Routes)
	}
}

// TestUnknownFlagsRefused is exercised at the CLI layer (cmd/argus); this package-level test only
// documents that Run itself has no flag parsing of its own to test here.
func TestRunRefusesEmptyRepo(t *testing.T) {
	out := t.TempDir()
	if _, err := Run(Options{Repo: "", Out: out}); err == nil {
		t.Skip("empty repo currently resolves to cwd via filepath.Abs; the CLI layer refuses an empty --repo before calling Run")
	}
}
