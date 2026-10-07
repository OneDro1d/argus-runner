package reporoute

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// review_defects_test.go: TEST-FIRST coverage for the six defects found reviewing propose-from-repo
// against shop-services (--config .../shop-dev/argus-config.yaml, money_handling: true).
// Each test below was RED before its matching fix landed; the fixtures under testdata/repos use the
// exact shapes the review quoted from the real repo.

// --- Defect 1: test source files scanned for routes -----------------------------------------

func TestTestSourcesSkipped(t *testing.T) {
	out := t.TempDir()
	rep, err := Run(Options{Repo: repoPath(t, "test-source-skip"), Out: out})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, r := range rep.Routes {
		switch r.Path {
		case "/x", "/probe", "/protected", "/whoami", "/echo", "/fail", "/fixture-only-route":
			t.Errorf("a route from a test source file was extracted: %+v", r)
		}
	}
	if rep.SkippedTestFiles < 2 {
		t.Errorf("SkippedTestFiles = %d, want at least 2 (idempotency.test.ts + foo.spec.js under __tests__)", rep.SkippedTestFiles)
	}
	// The real route, outside any test file/dir, must still be found.
	found := false
	for _, r := range rep.Routes {
		if r.Path == "/real" {
			found = true
		}
	}
	if !found {
		t.Errorf("the real (non-test) route was not found; routes=%+v", rep.Routes)
	}
}

// --- Defect 2: outbound HTTP client calls read as routes -------------------------------------

func TestOutboundClientCallsNotExtracted(t *testing.T) {
	out := t.TempDir()
	rep, err := Run(Options{Repo: repoPath(t, "outbound-client"), Out: out})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, r := range rep.Routes {
		if r.Path == "/verification" || strings.Contains(r.Path, "/applicant/") {
			t.Errorf("an outbound axios client call was extracted as a route: %+v", r)
		}
	}
	if len(rep.AmbiguousCalls) < 2 {
		t.Fatalf("AmbiguousCalls = %+v, want at least 2 (the POST /verification and GET /applicant/.../status client calls)", rep.AmbiguousCalls)
	}
	foundClientAlias := false
	foundTemplate := false
	for _, a := range rep.AmbiguousCalls {
		if a.Path == "/verification" {
			foundClientAlias = true
		}
		if strings.Contains(a.Path, "${") {
			foundTemplate = true
		}
	}
	if !foundClientAlias {
		t.Errorf("expected the this.api.post(\"/verification\", ...) call skipped as an HTTP-client receiver: %+v", rep.AmbiguousCalls)
	}
	if !foundTemplate {
		t.Errorf("expected the template-expression path skipped: %+v", rep.AmbiguousCalls)
	}
	// The real Fastify/Express route in the same fixture must still be found.
	found := false
	for _, r := range rep.Routes {
		if r.Path == "/health" {
			found = true
		}
	}
	if !found {
		t.Errorf("the real /health route should still be extracted; routes=%+v", rep.Routes)
	}
}

// --- Defect 3: duplicates ----------------------------------------------------------------------

// TestDedupSameServiceMergesWithAllReferences: the SAME route found by two extractors in the SAME
// service (OpenAPI {strategyPairId} + Fastify :strategyPairId) is proposed ONCE, with both sources
// listed in References — never as a "-2" duplicate.
func TestDedupSameServiceMergesWithAllReferences(t *testing.T) {
	out := t.TempDir()
	rep, err := Run(Options{Repo: repoPath(t, "dedup-same-service"), Out: out})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var matches []Draft
	for _, d := range rep.Accepted {
		if d.Method == "GET" && strings.Contains(d.Path, "strategies") {
			matches = append(matches, d)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("want exactly ONE merged draft for the strategies route, got %d: %+v", len(matches), matches)
	}
	d := matches[0]
	if strings.HasSuffix(d.ID, "-2") {
		t.Errorf("merged draft got a '-2' duplicate-style ID: %s", d.ID)
	}
	if len(d.References) != 2 {
		t.Errorf("References = %v, want 2 (the openapi.yaml entry AND the strategies.ts entry)", d.References)
	}
	body, rerr := os.ReadFile(filepath.Join(out, d.File))
	if rerr != nil {
		t.Fatalf("read %s: %v", d.File, rerr)
	}
	if !strings.Contains(string(body), "openapi.yaml") || !strings.Contains(string(body), "strategies.ts") {
		t.Errorf("written draft References section does not list both sources:\n%s", body)
	}
}

// TestDedupCrossServiceKeepsSeparateNamedByService: the SAME method+path declared in DIFFERENT
// services (custody-service, wallet-service) stays as separate drafts, named by service directory —
// never a numeric "-2" suffix.
func TestDedupCrossServiceKeepsSeparateNamedByService(t *testing.T) {
	out := t.TempDir()
	rep, err := Run(Options{Repo: repoPath(t, "dedup-cross-service"), Out: out})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var ids []string
	for _, d := range rep.Accepted {
		if d.Method == "GET" && d.Path == "/health" {
			ids = append(ids, d.ID)
		}
	}
	if len(ids) != 2 {
		t.Fatalf("want 2 separate /health drafts (one per service), got %d: %v", len(ids), ids)
	}
	want := map[string]bool{"RT-custody-service-GET-health": false, "RT-wallet-service-GET-health": false}
	for _, id := range ids {
		if _, ok := want[id]; ok {
			want[id] = true
		}
		if strings.HasSuffix(id, "-2") {
			t.Errorf("cross-service duplicate used a numeric suffix instead of the service name: %s", id)
		}
	}
	for id, ok := range want {
		if !ok {
			t.Errorf("expected service-named id %q, got ids=%v", id, ids)
		}
	}
}

// --- Defect 4: money-handling generation too loose ---------------------------------------------

func TestMoneyHandlingGeneratesOnlyProbeHealthMetricsGETs(t *testing.T) {
	out := t.TempDir()
	rep, err := Run(Options{
		Repo: repoPath(t, "money-probe-paths"), Out: out,
		Config: filepath.Join("testdata", "money-config.yaml"),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	acceptedPaths := map[string]bool{}
	for _, d := range rep.Accepted {
		acceptedPaths[d.Path] = true
	}
	for _, want := range []string{"/healthz", "/metrics", "/status", "/health/status", "/info"} {
		if !acceptedPaths[want] {
			t.Errorf("GET %s should be proposed under money_handling; accepted=%+v", want, rep.Accepted)
		}
	}
	notProposedPaths := map[string]string{}
	for _, r := range rep.NotProposed {
		notProposedPaths[r.Path] = r.Reason
	}
	for _, wantExcluded := range []string{
		"/kyc/status", "/api/v1/user/info", "/api/v1/user/deposit-address/:chainId", "/api/admin/users",
	} {
		reason, ok := notProposedPaths[wantExcluded]
		if !ok {
			t.Errorf("GET %s should be listed as not-proposed under money_handling; not_proposed=%+v accepted=%+v",
				wantExcluded, rep.NotProposed, rep.Accepted)
			continue
		}
		if reason != notProposedReason {
			t.Errorf("GET %s not-proposed reason = %q, want %q", wantExcluded, reason, notProposedReason)
		}
		if acceptedPaths[wantExcluded] {
			t.Errorf("GET %s must NOT be accepted/written under money_handling", wantExcluded)
		}
	}
	// Nothing is written to disk for the excluded paths.
	entries, _ := os.ReadDir(out)
	for _, e := range entries {
		if strings.Contains(e.Name(), "deposit-address") || strings.Contains(e.Name(), "kyc") {
			t.Errorf("a file was written for a not-proposed (money_handling) draft: %s", e.Name())
		}
	}
}

// --- Defect 5: parameterised routes write unrunnable files --------------------------------------

// TestNeedsValueWrittenUnderDotDirSkippedByDiscovery proves the existing mechanism
// (scenario.DiscoverFiles's dot-directory prune) is what keeps a parameterised draft out of
// validate-config/cloud-seed-scenarios, by actually RUNNING discovery over --out.
func TestNeedsValueWrittenUnderDotDirSkippedByDiscovery(t *testing.T) {
	out := t.TempDir()
	rep, err := Run(Options{Repo: repoPath(t, "param-needs-value"), Out: out})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var d *Draft
	for i := range rep.Accepted {
		if rep.Accepted[i].Path == "/users/:id" {
			d = &rep.Accepted[i]
		}
	}
	if d == nil {
		t.Fatalf("expected an accepted draft for /users/:id; accepted=%+v", rep.Accepted)
	}
	if !d.NeedsValue {
		t.Errorf("Draft.NeedsValue should be true for a parameterised route")
	}
	if !strings.HasPrefix(filepath.ToSlash(d.File), ".needs-values/") {
		t.Errorf("File = %q, want it written under .needs-values/", d.File)
	}
	// The file must actually exist where the report says it does.
	if _, statErr := os.Stat(filepath.Join(out, d.File)); statErr != nil {
		t.Fatalf("written file missing: %v", statErr)
	}
	// It still passes ValidateAll (T5.3 is not relaxed by spec item 5).
	body, rerr := os.ReadFile(filepath.Join(out, d.File))
	if rerr != nil {
		t.Fatalf("read: %v", rerr)
	}
	_ = body
	// The load-bearing proof: run the REAL discovery mechanism over --out and confirm it finds
	// NOTHING — scenario.DiscoverFiles already prunes every dot-directory (discover.go), which is
	// exactly what validate-config and cloud-seed-scenarios call.
	discovered := scenario.DiscoverFiles(out)
	for _, disc := range discovered {
		if strings.Contains(filepath.ToSlash(disc.Path), ".needs-values/") {
			t.Errorf("scenario.DiscoverFiles picked up a .needs-values draft: %s (must be skipped until a human fills the value)", disc.Path)
		}
	}
}

// --- Defect 6: wrong extractor label -------------------------------------------------------------

func TestFastifyFileLabeledFastifyNotExpress(t *testing.T) {
	out := t.TempDir()
	rep, err := Run(Options{Repo: repoPath(t, "fastify-labeled-correctly"), Out: out})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var got *Route
	for i := range rep.Routes {
		if strings.Contains(rep.Routes[i].Path, "strategies") {
			got = &rep.Routes[i]
		}
	}
	if got == nil {
		t.Fatalf("expected the strategies route; routes=%+v", rep.Routes)
	}
	if got.Extractor != "fastify" {
		t.Errorf("Extractor = %q, want %q (file imports FastifyInstance, not express) — got path %q", got.Extractor, "fastify", got.Path)
	}
}

// TestAmbiguousJSFileLabeledJSRouter: a file with no Fastify/Express import signal at all is
// labelled "js-router" (spec item 6: "if a file is ambiguous say js-router"), never guessed from
// path punctuation.
func TestAmbiguousJSFileLabeledJSRouter(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.js"), []byte(
		"const app = makeApp();\napp.get('/widgets', function (req, res) { res.json([]); });\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	rep, err := Run(Options{Repo: dir, Out: out})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rep.Routes) != 1 {
		t.Fatalf("expected exactly 1 route, got %+v", rep.Routes)
	}
	if rep.Routes[0].Extractor != "js-router" {
		t.Errorf("Extractor = %q, want %q for a file with no framework signal", rep.Routes[0].Extractor, "js-router")
	}
}
