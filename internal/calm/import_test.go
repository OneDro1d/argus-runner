package calm

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/toolcore"
)

// — `argus calm import`. The importer turns a FINOS CALM architecture into Argus check
// files, a traceability map and an UNMAPPED.md that lists everything it did NOT turn into a check.

// ── helpers ──────────────────────────────────────────────────────────────────────────────────────

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// arch builds a small architecture: actor a, mcp service m, http service t, system s.
func arch(rels string, nodeControls string) string {
	return `{
  "unique-id": "demo-arch",
  "name": "Demo",
  "nodes": [
    {"unique-id":"a","node-type":"actor","name":"Client"},
    {"unique-id":"m","node-type":"service","name":"MCP"` + nodeControls + `},
    {"unique-id":"t","node-type":"service","name":"API"},
    {"unique-id":"s","node-type":"system","name":"Cluster"}
  ],
  "relationships": [` + rels + `]
}`
}

const relAtoM = `{"unique-id":"a-to-m","protocol":"HTTPS","relationship-type":{"connects":{"source":{"node":"a"},"destination":{"node":"m"}}}}`
const relMtoT = `{"unique-id":"m-to-t","protocol":"HTTP","relationship-type":{"connects":{"source":{"node":"m"},"destination":{"node":"t"}}}}`
const relDeployed = `{"unique-id":"in-s","relationship-type":{"deployed-in":{"container":"s","nodes":["m","t"]}}}`

var mcpBind = Bind{URL: "http://m.example.svc.cluster.local:8080/mcp", Transport: "streamable-http"}
var tBind = Bind{URL: "http://t.example.svc.cluster.local:8080"}

func run(t *testing.T, files map[string]string, o Options) (*Result, error) {
	t.Helper()
	dir := writeTree(t, files)
	o.ArchitecturePath = filepath.Join(dir, "arch.json")
	return Import(o)
}

func mustImport(t *testing.T, files map[string]string, o Options) *Result {
	t.Helper()
	r, err := run(t, files, o)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	return r
}

func checkByKind(r *Result, kind string) []Check {
	var out []Check
	for _, c := range r.Checks {
		if c.Kind == kind {
			out = append(out, c)
		}
	}
	return out
}

func fileOf(t *testing.T, r *Result, c Check) string {
	t.Helper()
	b, ok := r.Files[c.File]
	if !ok {
		t.Fatalf("check %s names file %s which is not in the result (%v)", c.ID, c.File, fileNames(r))
	}
	return string(b)
}

func fileNames(r *Result) []string {
	var n []string
	for k := range r.Files {
		n = append(n, k)
	}
	sort.Strings(n)
	return n
}

func mustBeValid(t *testing.T, name, body string) {
	t.Helper()
	_, errs := toolcore.ValidateAll(body)
	if len(errs) != 0 {
		t.Errorf("%s does not pass the scenario validator author__validate_scenario uses: %v\n%s", name, errs, body)
	}
}

func contains(t *testing.T, haystack, needle, what string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Errorf("%s: want %q in:\n%s", what, needle, haystack)
	}
}

// ── connects ─────────────────────────────────────────────────────────────────────────────────────

func TestImport_ConnectsToAnMCPNodeBecomesAnMCPProbe(t *testing.T) {
	r := mustImport(t, map[string]string{"arch.json": arch(relAtoM, "")},
		Options{Binds: map[string]Bind{"m": mcpBind}, Validate: toolcore.ValidateAll})
	cs := checkByKind(r, KindConnects)
	if len(cs) != 1 {
		t.Fatalf("want exactly one connects check, got %+v", r.Checks)
	}
	body := fileOf(t, r, cs[0])
	mustBeValid(t, cs[0].File, body)
	contains(t, body, `"type": "mcp"`, "connects")
	contains(t, body, `"transport": "streamable-http"`, "connects")
	contains(t, body, `"server_url": "`+mcpBind.URL+`"`, "connects")
	contains(t, body, "- step probe: protocol error", "connects")
	contains(t, body, "calm:demo-arch:relationship:a-to-m", "References")
	// the relationship it proves, and the node it reaches (H6: a node a check exercises is NAMED by it)
	if len(cs[0].Covers) != 2 || cs[0].Covers[0] != "calm:demo-arch:relationship:a-to-m" || cs[0].Covers[1] != "calm:demo-arch:node:m" {
		t.Errorf("covers: %v", cs[0].Covers)
	}
}

func TestImport_ConnectsToAnMCPNodeOverLegacySSEKeepsTheTransport(t *testing.T) {
	r := mustImport(t, map[string]string{"arch.json": arch(relAtoM, "")},
		Options{Binds: map[string]Bind{"m": {URL: "http://m.example:8080", Transport: "http-sse"}}, Validate: toolcore.ValidateAll})
	body := fileOf(t, r, checkByKind(r, KindConnects)[0])
	mustBeValid(t, "sse", body)
	contains(t, body, `"transport": "http-sse"`, "sse connects")
}

func TestImport_ConnectsToAnHTTPNodeBecomesAGet(t *testing.T) {
	rel := `{"unique-id":"a-to-t","relationship-type":{"connects":{"source":{"node":"a"},"destination":{"node":"t"}}}}`
	r := mustImport(t, map[string]string{"arch.json": arch(rel, "")},
		Options{Binds: map[string]Bind{"t": tBind}, Validate: toolcore.ValidateAll})
	body := fileOf(t, r, checkByKind(r, KindConnects)[0])
	mustBeValid(t, "http connects", body)
	contains(t, body, `"type": "http"`, "http connects")
	contains(t, body, `"method": "GET"`, "http connects")
	contains(t, body, `"url": "`+tBind.URL+`"`, "http connects")
	contains(t, body, "- step probe: status=200", "http connects")
}

func TestImport_ConnectsToAnUnboundNodeIsRefusedByName(t *testing.T) {
	_, err := run(t, map[string]string{"arch.json": arch(relAtoM, "")}, Options{Validate: toolcore.ValidateAll})
	if err == nil || !strings.Contains(err.Error(), `"m"`) || !strings.Contains(err.Error(), "--bind") {
		t.Fatalf("an unbound destination must be refused naming the node and the flag; got %v", err)
	}
}

// ── through, structural relationships ─────────────────────────────────────────────────────────────

func TestImport_ServiceToServiceConnectsIsListedAsNeedingAProbe(t *testing.T) {
	r := mustImport(t, map[string]string{"arch.json": arch(relAtoM+","+relMtoT, "")},
		Options{Binds: map[string]Bind{"m": mcpBind, "t": tBind}, Validate: toolcore.ValidateAll})
	u := unmappedFor(r, "calm:demo-arch:relationship:m-to-t")
	if u == nil || !strings.Contains(u.Reason, "needs a probe") {
		t.Fatalf("a service->service relationship must be listed as 'needs a probe'; got %+v", r.Unmapped)
	}
	if !strings.Contains(string(r.Files["UNMAPPED.md"]), "calm:demo-arch:relationship:m-to-t") {
		t.Errorf("UNMAPPED.md must name it:\n%s", r.Files["UNMAPPED.md"])
	}
}

func TestImport_DeployedInIsListedNotDropped(t *testing.T) {
	r := mustImport(t, map[string]string{"arch.json": arch(relAtoM+","+relDeployed, "")},
		Options{Binds: map[string]Bind{"m": mcpBind}, Validate: toolcore.ValidateAll})
	u := unmappedFor(r, "calm:demo-arch:relationship:in-s")
	if u == nil || !strings.Contains(u.Reason, "deployed-in") {
		t.Fatalf("deployed-in must be listed with its type; got %+v", r.Unmapped)
	}
}

func TestImport_EveryRelationshipIsEitherCoveredOrUnmapped(t *testing.T) {
	r := mustImport(t, map[string]string{"arch.json": arch(relAtoM+","+relMtoT+","+relDeployed, "")},
		Options{Binds: map[string]Bind{"m": mcpBind, "t": tBind}, Validate: toolcore.ValidateAll})
	for _, id := range []string{"a-to-m", "m-to-t", "in-s"} {
		ref := "calm:demo-arch:relationship:" + id
		if !covered(r, ref) && unmappedFor(r, ref) == nil {
			t.Errorf("%s is neither covered by a check nor listed in UNMAPPED — it was dropped silently", ref)
		}
	}
}

// ── must-not-connect ──────────────────────────────────────────────────────────────────────────────

func TestImport_MustNotConnectIsAPositiveStepThenAnUnreachableClaim(t *testing.T) {
	r := mustImport(t, map[string]string{"arch.json": arch(relAtoM, "")},
		Options{Binds: map[string]Bind{"m": mcpBind, "t": tBind}, Validate: toolcore.ValidateAll})
	cs := checkByKind(r, KindMustNotConnect)
	if len(cs) != 1 {
		t.Fatalf("a has no relationship to t: want one must-not-connect check, got %+v", r.Checks)
	}
	body := fileOf(t, r, cs[0])
	mustBeValid(t, cs[0].File, body)
	alive := strings.Index(body, `"name": "alive"`)
	blocked := strings.Index(body, `"name": "blocked"`)
	if alive < 0 || blocked < 0 || alive > blocked {
		t.Fatalf("the positive step must come BEFORE the unreachable one:\n%s", body)
	}
	contains(t, body, `"url": "`+tBind.URL+`"`, "blocked step url")
	contains(t, body, "- step alive: protocol error", "positive step claim")
	contains(t, body, "- step blocked: unreachable", "negative claim")
	contains(t, body, "calm:demo-arch:relationship:a-to-m", "positive relationship is referenced")
	contains(t, body, "calm:demo-arch:node:t", "the node whose absence is claimed is referenced")
}

func TestImport_NoMustNotConnectWithoutAPositiveStep(t *testing.T) {
	// the actor has no relationship at all: an unreachable claim would have nothing proving the system is up.
	r := mustImport(t, map[string]string{"arch.json": arch(relMtoT, "")},
		Options{Binds: map[string]Bind{"t": tBind}, Validate: toolcore.ValidateAll})
	if len(checkByKind(r, KindMustNotConnect)) != 0 {
		t.Fatalf("no positive step is available, so no unreachable check may be generated: %+v", r.Checks)
	}
	u := unmappedFor(r, "calm:demo-arch:node:t")
	if u == nil || !strings.Contains(u.Reason, "positive") {
		t.Fatalf("the skipped must-not-connect must be listed with its reason; got %+v", r.Unmapped)
	}
}

func TestImport_UnboundNodeGetsNoMustNotConnectButIsListed(t *testing.T) {
	r := mustImport(t, map[string]string{"arch.json": arch(relAtoM, "")},
		Options{Binds: map[string]Bind{"m": mcpBind}, Validate: toolcore.ValidateAll})
	if len(checkByKind(r, KindMustNotConnect)) != 0 {
		t.Fatalf("t is not bound: nothing to probe: %+v", r.Checks)
	}
	if u := unmappedFor(r, "calm:demo-arch:node:t"); u == nil || !strings.Contains(u.Reason, "not bound") {
		t.Fatalf("t must be listed as not bound; got %+v", r.Unmapped)
	}
}

// ── controls ──────────────────────────────────────────────────────────────────────────────────────

const guardrailControl = `,"controls":{"mcp-guardrail":{"description":"deny symbols","requirements":[{"requirement-url":"controls/req.json","config-url":"controls/cfg.json"}]}}`

var guardFiles = func(archBody string) map[string]string {
	return map[string]string{
		"arch.json":         archBody,
		"controls/req.json": `{"title":"req"}`,
		"controls/cfg.json": `{"control-id":"mcp-001","denied-symbols":["VOD","GME","AMC"],"enforcement-point":"m"}`,
	}
}

func guardOpts() Options {
	return Options{
		Binds:       map[string]Bind{"m": mcpBind},
		ControlArgs: map[string]string{"mcp-guardrail.tool": "get_trades", "mcp-guardrail.arg": "symbol"},
		Validate:    toolcore.ValidateAll,
	}
}

func TestImport_GuardrailMakesOneRefusedCheckPerDeniedSymbolAndOneAllowed(t *testing.T) {
	r := mustImport(t, guardFiles(arch(relAtoM, guardrailControl)), guardOpts())
	refused := checkByKind(r, KindGuardrailRefused)
	allowed := checkByKind(r, KindGuardrailAllowed)
	if len(refused) != 3 || len(allowed) != 1 {
		t.Fatalf("want 3 refused + 1 allowed, got %d + %d: %+v", len(refused), len(allowed), r.Checks)
	}
	for _, c := range refused {
		body := fileOf(t, r, c)
		mustBeValid(t, c.File, body)
		contains(t, body, `"tool": "get_trades"`, c.ID)
		contains(t, body, "- step denied: result.isError == true", c.ID)
		contains(t, body, "calm:demo-arch:control:m/mcp-guardrail", c.ID)
		// a wrong tool name must not read as a refusal: the allowed call comes first and must succeed.
		if strings.Index(body, `"name": "allowed"`) > strings.Index(body, `"name": "denied"`) {
			t.Errorf("%s: the allowed call must precede the denied one:\n%s", c.ID, body)
		}
		contains(t, body, "- step allowed: result.isError == false", c.ID)
	}
	for _, sym := range []string{"VOD", "GME", "AMC"} {
		found := false
		for _, c := range refused {
			if strings.Contains(fileOf(t, r, c), `"symbol": "`+sym+`"`) {
				found = true
			}
		}
		if !found {
			t.Errorf("no refused check for %s", sym)
		}
	}
	ab := fileOf(t, r, allowed[0])
	mustBeValid(t, allowed[0].File, ab)
	contains(t, ab, "- step allowed: result.isError == false", "allowed check")
	if strings.Contains(ab, `"VOD"`) || strings.Contains(ab, `"GME"`) || strings.Contains(ab, `"AMC"`) {
		t.Errorf("the allowed check must use a symbol NOT in the denied list:\n%s", ab)
	}
}

func TestImport_GuardrailRefusalPlaneCanBeProtocol(t *testing.T) {
	o := guardOpts()
	o.ControlArgs["mcp-guardrail.refusal"] = "protocol"
	r := mustImport(t, guardFiles(arch(relAtoM, guardrailControl)), o)
	body := fileOf(t, r, checkByKind(r, KindGuardrailRefused)[0])
	mustBeValid(t, "protocol refusal", body)
	contains(t, body, "- step denied: protocol error", "protocol refusal")
}

func TestImport_GuardrailAllowedSymbolCannotBeADeniedOne(t *testing.T) {
	o := guardOpts()
	o.ControlArgs["mcp-guardrail.allowed"] = "GME"
	if _, err := run(t, guardFiles(arch(relAtoM, guardrailControl)), o); err == nil || !strings.Contains(err.Error(), "GME") {
		t.Fatalf("an allowed symbol that is denied must be refused by name; got %v", err)
	}
}

func TestImport_GuardrailWithoutToolAndArgIsListedNeverGuessed(t *testing.T) {
	o := guardOpts()
	o.ControlArgs = nil
	r := mustImport(t, guardFiles(arch(relAtoM, guardrailControl)), o)
	if len(checkByKind(r, KindGuardrailRefused))+len(checkByKind(r, KindGuardrailAllowed)) != 0 {
		t.Fatalf("the importer must never guess a tool name: %+v", r.Checks)
	}
	u := unmappedFor(r, "calm:demo-arch:control:m/mcp-guardrail")
	if u == nil || !strings.Contains(u.Reason, "mcp-guardrail.tool") || !strings.Contains(u.Reason, "mcp-guardrail.arg") {
		t.Fatalf("the reason must say which --control-arg is missing; got %+v", r.Unmapped)
	}
}

func TestImport_UnknownControlIsListedWithItsRequirementURL(t *testing.T) {
	ctl := `,"controls":{"data-residency":{"description":"x","requirements":[{"requirement-url":"controls/res.json","config-url":"controls/res.config.json"}]}}`
	r := mustImport(t, map[string]string{"arch.json": arch(relAtoM, ctl)},
		Options{Binds: map[string]Bind{"m": mcpBind}, Validate: toolcore.ValidateAll})
	u := unmappedFor(r, "calm:demo-arch:control:m/data-residency")
	if u == nil || !strings.Contains(u.Reason, "no mapper") || !strings.Contains(u.Detail, "controls/res.json") {
		t.Fatalf("an unknown control must be listed with its requirement-url; got %+v", r.Unmapped)
	}
	if !strings.Contains(string(r.Files["UNMAPPED.md"]), "controls/res.json") {
		t.Errorf("UNMAPPED.md must carry the requirement-url:\n%s", r.Files["UNMAPPED.md"])
	}
}

func TestImport_GuardrailOnAnUnboundNodeIsRefusedByName(t *testing.T) {
	_, err := run(t, guardFiles(arch(relAtoM, guardrailControl)),
		Options{Binds: map[string]Bind{"t": tBind}, ControlArgs: guardOpts().ControlArgs, Validate: toolcore.ValidateAll})
	if err == nil || !strings.Contains(err.Error(), `"m"`) {
		t.Fatalf("a control on an unbound node must be refused naming it; got %v", err)
	}
}

func TestImport_GuardrailOnANonMCPBindingIsListed(t *testing.T) {
	o := guardOpts()
	o.Binds = map[string]Bind{"m": {URL: "http://m.example:8080"}}
	r := mustImport(t, guardFiles(arch(relAtoM, guardrailControl)), o)
	u := unmappedFor(r, "calm:demo-arch:control:m/mcp-guardrail")
	if u == nil || !strings.Contains(u.Reason, "MCP") {
		t.Fatalf("a guardrail on a node bound without a transport is not an MCP check: %+v", r.Unmapped)
	}
}

func TestImport_GuardrailConfigProblemsAreListedNotFatal(t *testing.T) {
	files := guardFiles(arch(relAtoM, guardrailControl))
	files["controls/cfg.json"] = `{"denied-symbols":"not-a-list"}`
	r := mustImport(t, files, guardOpts())
	if u := unmappedFor(r, "calm:demo-arch:control:m/mcp-guardrail"); u == nil || !strings.Contains(u.Reason, "denied-symbols") {
		t.Fatalf("got %+v", r.Unmapped)
	}
	delete(files, "controls/cfg.json")
	r = mustImport(t, files, guardOpts())
	if u := unmappedFor(r, "calm:demo-arch:control:m/mcp-guardrail"); u == nil || !strings.Contains(u.Reason, "config-url") {
		t.Fatalf("a missing config file must be listed; got %+v", r.Unmapped)
	}
}

// ── flows and the rest ────────────────────────────────────────────────────────────────────────────

func TestImport_FlowsAreListedWithTheirTransitions(t *testing.T) {
	a := strings.Replace(arch(relAtoM, ""), `"relationships"`, `"flows":[{"unique-id":"buy","name":"Buy","description":"d","transitions":[{"relationship-unique-id":"a-to-m","sequence-number":1,"description":"ask"}]}],"relationships"`, 1)
	r := mustImport(t, map[string]string{"arch.json": a}, Options{Binds: map[string]Bind{"m": mcpBind}, Validate: toolcore.ValidateAll})
	u := unmappedFor(r, "calm:demo-arch:flow:buy")
	if u == nil || !strings.Contains(u.Detail, "a-to-m") {
		t.Fatalf("a flow must be listed with its transitions; got %+v", r.Unmapped)
	}
}

// ── actor and flags ───────────────────────────────────────────────────────────────────────────────

func TestImport_TwoActorsNeedStandAs(t *testing.T) {
	a := strings.Replace(arch(relAtoM, ""), `{"unique-id":"s"`, `{"unique-id":"b","node-type":"actor","name":"B"},{"unique-id":"s"`, 1)
	_, err := run(t, map[string]string{"arch.json": a}, Options{Binds: map[string]Bind{"m": mcpBind}, Validate: toolcore.ValidateAll})
	if err == nil || !strings.Contains(err.Error(), "--stand-as") || !strings.Contains(err.Error(), "a") || !strings.Contains(err.Error(), "b") {
		t.Fatalf("an ambiguous actor must be refused listing the candidates; got %v", err)
	}
	r := mustImport(t, map[string]string{"arch.json": a}, Options{Binds: map[string]Bind{"m": mcpBind}, StandAs: "a", Validate: toolcore.ValidateAll})
	if len(checkByKind(r, KindConnects)) != 1 {
		t.Fatalf("with --stand-as a the a->m relationship is a connects check: %+v", r.Checks)
	}
}

func TestImport_StandAsMustNameANode(t *testing.T) {
	_, err := run(t, map[string]string{"arch.json": arch(relAtoM, "")}, Options{StandAs: "zzz", Binds: map[string]Bind{"m": mcpBind}, Validate: toolcore.ValidateAll})
	if err == nil || !strings.Contains(err.Error(), `"zzz"`) {
		t.Fatalf("got %v", err)
	}
}

func TestImport_BindProblemsAreRefused(t *testing.T) {
	cases := []struct {
		name string
		bind map[string]Bind
		want string
	}{
		{"unknown node", map[string]Bind{"nope": tBind}, `"nope"`},
		{"credentials in the url", map[string]Bind{"m": {URL: "http://u:p@m.example/mcp", Transport: "streamable-http"}}, "credential"},
		{"credential-shaped query", map[string]Bind{"m": {URL: "http://m.example/mcp?api_key=x", Transport: "streamable-http"}}, "credential"},
		{"not http", map[string]Bind{"m": {URL: "ftp://m.example/mcp", Transport: "streamable-http"}}, "http"},
		{"bad transport", map[string]Bind{"m": {URL: "http://m.example/mcp", Transport: "grpc"}}, "grpc"},
	}
	for _, c := range cases {
		_, err := run(t, map[string]string{"arch.json": arch(relAtoM, "")}, Options{Binds: c.bind, Validate: toolcore.ValidateAll})
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: want a refusal containing %q; got %v", c.name, c.want, err)
		}
	}
}

func TestImport_UnknownControlArgKeyIsRefused(t *testing.T) {
	o := guardOpts()
	o.ControlArgs["mcp-guardrail.tols"] = "x"
	if _, err := run(t, guardFiles(arch(relAtoM, guardrailControl)), o); err == nil || !strings.Contains(err.Error(), "mcp-guardrail.tols") {
		t.Fatalf("a typo'd --control-arg key must be refused by name; got %v", err)
	}
}

func TestParseBindFlag(t *testing.T) {
	good := map[string]struct{ node, url, tr string }{
		"m=http://h:8080/mcp,streamable-http": {"m", "http://h:8080/mcp", "streamable-http"},
		"m=http://h:8080,http-sse":            {"m", "http://h:8080", "http-sse"},
		"t=http://h:8080":                     {"t", "http://h:8080", ""},
	}
	for in, w := range good {
		n, b, err := ParseBind(in)
		if err != nil || n != w.node || b.URL != w.url || b.Transport != w.tr {
			t.Errorf("%q: got %q %+v %v", in, n, b, err)
		}
	}
	for _, in := range []string{"", "m", "=http://h", "m=", "m=http://h,streamble-http"} {
		if _, _, err := ParseBind(in); err == nil {
			t.Errorf("%q must be refused", in)
		}
	}
}

// ── output shape ──────────────────────────────────────────────────────────────────────────────────

func TestImport_OutputIsDeterministicAndEveryCheckIsInTheMap(t *testing.T) {
	files := guardFiles(arch(relAtoM+","+relMtoT+","+relDeployed, guardrailControl))
	o := func() Options {
		return Options{Binds: map[string]Bind{"m": mcpBind, "t": tBind}, ControlArgs: guardOpts().ControlArgs, Validate: toolcore.ValidateAll}
	}
	r1 := mustImport(t, files, o())
	r2 := mustImport(t, files, o())
	if strings.Join(fileNames(r1), ",") != strings.Join(fileNames(r2), ",") {
		t.Fatal("file lists differ between runs")
	}
	for n := range r1.Files {
		if string(r1.Files[n]) != string(r2.Files[n]) {
			t.Errorf("%s differs between two runs of the same input", n)
		}
	}
	var m struct {
		Checks []struct {
			ID     string   `json:"id"`
			File   string   `json:"file"`
			Kind   string   `json:"kind"`
			Covers []string `json:"covers"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(r1.Files["calm-import.json"], &m); err != nil {
		t.Fatalf("calm-import.json: %v", err)
	}
	if len(m.Checks) != len(r1.Checks) {
		t.Fatalf("the map lists %d checks, the importer made %d", len(m.Checks), len(r1.Checks))
	}
	for _, c := range m.Checks {
		if len(c.Covers) == 0 {
			t.Errorf("%s covers no CALM id", c.ID)
		}
		if _, ok := r1.Files[c.File]; !ok {
			t.Errorf("%s names a file that was not written: %s", c.ID, c.File)
		}
		body := string(r1.Files[c.File])
		for _, id := range c.Covers {
			if !strings.Contains(body, id) {
				t.Errorf("%s: its References must name %s", c.File, id)
			}
		}
		mustBeValid(t, c.File, body)
	}
}

func TestImport_IDsAreUniqueAndLegal(t *testing.T) {
	files := guardFiles(arch(relAtoM, guardrailControl))
	files["controls/cfg.json"] = `{"denied-symbols":["A.B","A-B","a b","` + strings.Repeat("X", 90) + `"]}`
	r := mustImport(t, files, guardOpts())
	seen := map[string]bool{}
	for _, c := range r.Checks {
		if seen[c.ID] {
			t.Errorf("duplicate id %s", c.ID)
		}
		seen[c.ID] = true
		if len(c.ID) > 64 {
			t.Errorf("id too long: %s", c.ID)
		}
		mustBeValid(t, c.File, fileOf(t, r, c))
	}
}

func TestWrite_WritesEveryFile(t *testing.T) {
	r := mustImport(t, map[string]string{"arch.json": arch(relAtoM, "")},
		Options{Binds: map[string]Bind{"m": mcpBind}, Validate: toolcore.ValidateAll})
	out := filepath.Join(t.TempDir(), "o")
	if err := r.Write(out); err != nil {
		t.Fatal(err)
	}
	for n := range r.Files {
		if _, err := os.Stat(filepath.Join(out, n)); err != nil {
			t.Errorf("%s not written: %v", n, err)
		}
	}
	if err := r.Write(out); err == nil {
		t.Error("writing over an existing check file must be refused: the importer must not clobber an author's edits")
	}
}

// ── golden: the real QCon scenario 2 architecture ─────────────────────────────────────────────────

func qconOptions() Options {
	return Options{
		Binds: map[string]Bind{
			"mcp-server": {URL: "http://trades-mcp-server.calm-demo.svc.cluster.local/mcp", Transport: "streamable-http"},
			"trades-api": {URL: "http://trades.calm-demo.svc.cluster.local"},
		},
		ControlArgs: map[string]string{
			"mcp-guardrail.tool":             "getTrades",
			"mcp-guardrail.args":             `{"filter":"instrument eq '${symbol}'","nextLink":"","size":5}`,
			"mcp-guardrail.refusal":          "text",
			"mcp-guardrail.refused-contains": "is restricted",
			"mcp-guardrail.allowed":          "LSE:AAPL",
		},
		Validate: toolcore.ValidateAll,
	}
}

// ── helpers over Result ───────────────────────────────────────────────────────────────────────────

// A node that points at a nested architecture is not followed by v1, and says so.
func TestImport_DetailedArchitectureIsListedNotFollowed(t *testing.T) {
	a := strings.Replace(arch(relAtoM, ""), `{"unique-id":"t","node-type":"service","name":"API"}`,
		`{"unique-id":"t","node-type":"service","name":"API","details":{"detailed-architecture":"t.architecture.json"}}`, 1)
	r := mustImport(t, map[string]string{"arch.json": a}, Options{Binds: map[string]Bind{"m": mcpBind}, Validate: toolcore.ValidateAll})
	u := unmappedFor(r, "calm:demo-arch:node:t/detailed-architecture")
	if u == nil || !strings.Contains(u.Detail, "t.architecture.json") {
		t.Fatalf("a nested architecture must be listed with its file; got %+v", r.Unmapped)
	}
}

func unmappedFor(r *Result, ref string) *Unmapped {
	for i := range r.Unmapped {
		if r.Unmapped[i].Ref == ref {
			return &r.Unmapped[i]
		}
	}
	return nil
}

func covered(r *Result, ref string) bool {
	for _, c := range r.Checks {
		for _, id := range c.Covers {
			if id == ref {
				return true
			}
		}
	}
	return false
}
