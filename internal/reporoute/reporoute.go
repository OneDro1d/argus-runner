// Package reporoute implements T5.1+T5.3 (MVP2-SPRINT.md, E5): read-only ingestion of a real
// application repository into DRAFT Argus scenarios. It walks a repo tree, extracts HTTP routes
// from OpenAPI/Swagger documents and from common Fastify/Express/Go route-registration shapes,
// builds an HTTP-Ingestion-layer scenario draft per route, and accepts it only if it passes the
// SAME two gates a hand-authored scenario must pass: toolcore.ValidateAll (T5.3) and — when the
// SUT config declares money_handling — scenario.MoneyGuardViolations (T5.4). A draft that fails
// either gate is refused, named, and never written.
//
// The package touches the target repo READ-ONLY: it never writes, executes, or installs anything
// there. All writing happens under --out, which is refused outright when it resolves inside --repo
// (Run's very first check), so this tool can never contaminate the tree it is reading.
package reporoute

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/scenario"
	"github.com/OneDro1d/argus-runner/internal/toolcore"
)

// skipDirs names the directories Walk never descends into (spec item 2, T5.1).
var skipDirs = map[string]bool{
	"node_modules": true,
	".git":         true,
	"vendor":       true,
	"dist":         true,
	"build":        true,
}

// Route is one HTTP route extracted from the repo, with its provenance.
type Route struct {
	Method     string   `json:"method"`
	Path       string   `json:"path"`
	File       string   `json:"file"` // relative to the repo root
	Line       int      `json:"line"`
	Extractor  string   `json:"extractor"`
	ParamNames []string `json:"param_names,omitempty"`
	// PrefixUnresolved records that a Fastify register() prefix could not be statically resolved
	// for this route (spec item 2b, best-effort).
	PrefixUnresolved bool `json:"prefix_unresolved,omitempty"`
}

// Draft is one accepted scenario draft, written to --out.
type Draft struct {
	ID         string   `json:"id"`
	File       string   `json:"file"` // path written under --out, relative to it (may be under .needs-values/)
	Method     string   `json:"method"`
	Path       string   `json:"path"`
	Source     string   `json:"source"`                // first route.File:route.Line (see References for all of them)
	References []string `json:"references,omitempty"`  // every source file:line (extractor) that declared this route (spec item 3)
	Proposed   bool     `json:"proposed_write"`        // true = a non-GET draft proposed without a config
	NeedsValue bool     `json:"needs_value,omitempty"` // true = a path parameter is unfilled; written under .needs-values/ so discovery skips it (spec item 5)
}

// Refusal is one draft that was built but not written.
type Refusal struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Source string `json:"source"`
	Reason string `json:"reason"`
}

// UnresolvedPrefix records a register() call whose prefix option could not be statically matched
// to the routes it wraps (spec item 2b: "record when a prefix could not be resolved").
type UnresolvedPrefix struct {
	File   string `json:"file"`
	Line   int    `json:"line"`
	Prefix string `json:"prefix"`
	Reason string `json:"reason"`
}

// UnparseableFile is a file the extractors recognised as their kind (by extension, or by a
// top-level openapi:/swagger: key) but could not parse.
type UnparseableFile struct {
	File   string `json:"file"`
	Reason string `json:"reason"`
}

// AmbiguousCall is one JS/TS `<recv>.<method>(path, ...)` call the extractor found but, best-effort
// and conservative, declined to treat as a route registration (spec item 2).
type AmbiguousCall struct {
	File   string `json:"file"`
	Line   int    `json:"line"`
	Method string `json:"method"`
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// Report is what Run returns: everything the summary and --json output need (spec item 5).
type Report struct {
	Repo               string             `json:"repo"`
	RoutesByExtractor  map[string]int     `json:"routes_by_extractor"`
	Routes             []Route            `json:"routes_found"`
	Accepted           []Draft            `json:"drafts_accepted"`
	Refused            []Refusal          `json:"drafts_refused"`
	NotProposed        []Refusal          `json:"not_proposed,omitempty"` // money_handling: probe/health/metrics-only generation (spec item 4)
	UnresolvedPrefixes []UnresolvedPrefix `json:"unresolved_prefixes,omitempty"`
	UnparseableFiles   []UnparseableFile  `json:"unparseable_files,omitempty"`
	SchemaFiles        []string           `json:"schema_files,omitempty"`
	DocFiles           []string           `json:"doc_files,omitempty"`
	MoneyHandling      bool               `json:"money_handling"`
	SkippedTestFiles   int                `json:"skipped_test_files"`                // spec item 1
	AmbiguousCalls     []AmbiguousCall    `json:"ambiguous_calls_skipped,omitempty"` // spec item 2
}

// Options configures Run.
type Options struct {
	Repo   string // required: the repository to read
	Out    string // required: the directory scenarios are written into
	Config string // optional: an argus-config.yaml; when money_handling is true the money guard applies
}

// ErrOutInsideRepo is returned when --out resolves inside --repo (spec item 4).
var ErrOutInsideRepo = fmt.Errorf("--out must not be inside --repo (this tool refuses to write into the repo it scans)")

// Run walks opt.Repo, extracts routes, builds+gates a draft scenario per route, writes every
// accepted draft under opt.Out (created if absent), and returns the full report. It writes NOTHING
// to opt.Repo and makes no network call.
func Run(opt Options) (*Report, error) {
	repoAbs, err := filepath.Abs(opt.Repo)
	if err != nil {
		return nil, fmt.Errorf("resolve --repo: %w", err)
	}
	outAbs, err := filepath.Abs(opt.Out)
	if err != nil {
		return nil, fmt.Errorf("resolve --out: %w", err)
	}
	if outIsInsideRepo(repoAbs, outAbs) {
		return nil, ErrOutInsideRepo
	}

	rep := &Report{
		Repo:              repoAbs,
		RoutesByExtractor: map[string]int{},
	}

	var moneyHandling bool
	if opt.Config != "" {
		c, cerr := config.ParseUnresolved(opt.Config)
		if cerr != nil {
			return nil, fmt.Errorf("--config: %w", cerr)
		}
		moneyHandling = c.MoneyHandling
	}
	rep.MoneyHandling = moneyHandling

	if err := walk(repoAbs, rep); err != nil {
		return nil, fmt.Errorf("walk --repo: %w", err)
	}
	sort.Slice(rep.Routes, func(i, j int) bool { return routeLess(rep.Routes[i], rep.Routes[j]) })
	for _, r := range rep.Routes {
		rep.RoutesByExtractor[r.Extractor]++
	}

	// spec item 3: dedupe the SAME logical route found by two extractors in the SAME service into
	// ONE group (References lists every source); the SAME method+path in DIFFERENT services stays
	// as separate groups, told apart by serviceKey.
	groups := groupRoutes(rep.Routes)
	drafts := buildDrafts(groups, moneyHandling, rep)

	// Deterministic IDs: assign in the SAME sorted order groups were built in (spec item 4:
	// "same repo -> same IDs and same bytes"), then de-duplicate any remaining collision with a
	// stable counter (belt-and-braces on top of the service-key disambiguation above).
	seen := map[string]int{}
	type toWrite struct {
		id, relPath, body string
	}
	var write []toWrite
	for _, d := range drafts {
		if d.err != "" {
			rep.Refused = append(rep.Refused, Refusal{Method: d.group.method, Path: d.group.path, Source: source(d.group.sources[0]), Reason: d.err})
			continue
		}
		id := baseIDForGroup(d.group)
		seen[id]++
		if n := seen[id]; n > 1 {
			id = fmt.Sprintf("%s-%d", id, n)
		}
		var refs []string
		for _, s := range d.group.sources {
			refs = append(refs, source(s))
		}
		needsValue := len(d.group.paramNames) > 0
		relPath := id + ".md"
		if needsValue {
			// spec item 5: a draft with an unfilled path parameter must not be picked up by
			// scenario.DiscoverFiles (validate-config / cloud-seed-scenarios) until a human fills
			// it in — DiscoverFiles already prunes every dot-directory, so writing it under
			// .needs-values/ is that existing mechanism, proven by TestNeedsValueSkippedByDiscovery.
			relPath = filepath.Join(".needs-values", id+".md")
		}
		rep.Accepted = append(rep.Accepted, Draft{
			ID: id, File: relPath, Method: d.group.method, Path: d.group.path,
			Source: refs[0], References: refs, Proposed: d.proposed, NeedsValue: needsValue,
		})
		write = append(write, toWrite{id: id, relPath: relPath, body: withID(d.body, id)})
	}
	sort.Slice(rep.Accepted, func(i, j int) bool { return rep.Accepted[i].ID < rep.Accepted[j].ID })
	sort.Slice(write, func(i, j int) bool { return write[i].id < write[j].id })
	sort.Slice(rep.Refused, func(i, j int) bool {
		if rep.Refused[i].Source != rep.Refused[j].Source {
			return rep.Refused[i].Source < rep.Refused[j].Source
		}
		return rep.Refused[i].Reason < rep.Refused[j].Reason
	})
	sort.Slice(rep.NotProposed, func(i, j int) bool {
		if rep.NotProposed[i].Source != rep.NotProposed[j].Source {
			return rep.NotProposed[i].Source < rep.NotProposed[j].Source
		}
		return rep.NotProposed[i].Reason < rep.NotProposed[j].Reason
	})
	sort.Slice(rep.AmbiguousCalls, func(i, j int) bool {
		if rep.AmbiguousCalls[i].File != rep.AmbiguousCalls[j].File {
			return rep.AmbiguousCalls[i].File < rep.AmbiguousCalls[j].File
		}
		return rep.AmbiguousCalls[i].Line < rep.AmbiguousCalls[j].Line
	})

	if len(write) > 0 {
		if err := os.MkdirAll(outAbs, 0o755); err != nil {
			return nil, fmt.Errorf("create --out: %w", err)
		}
	}
	needsValueDir := filepath.Join(outAbs, ".needs-values")
	madeNeedsValueDir := false
	for _, w := range write {
		full := filepath.Join(outAbs, w.relPath)
		if filepath.Dir(full) == needsValueDir && !madeNeedsValueDir {
			if err := os.MkdirAll(needsValueDir, 0o755); err != nil {
				return nil, fmt.Errorf("create --out/.needs-values: %w", err)
			}
			madeNeedsValueDir = true
		}
		if err := os.WriteFile(full, []byte(w.body), 0o644); err != nil {
			return nil, fmt.Errorf("write %s: %w", w.relPath, err)
		}
	}

	sort.Slice(rep.UnresolvedPrefixes, func(i, j int) bool {
		if rep.UnresolvedPrefixes[i].File != rep.UnresolvedPrefixes[j].File {
			return rep.UnresolvedPrefixes[i].File < rep.UnresolvedPrefixes[j].File
		}
		return rep.UnresolvedPrefixes[i].Line < rep.UnresolvedPrefixes[j].Line
	})
	sort.Slice(rep.UnparseableFiles, func(i, j int) bool { return rep.UnparseableFiles[i].File < rep.UnparseableFiles[j].File })
	rep.SchemaFiles = dedupSorted(rep.SchemaFiles)
	rep.DocFiles = dedupSorted(rep.DocFiles)

	return rep, nil
}

func source(r Route) string { return fmt.Sprintf("%s:%d", r.File, r.Line) }

func dedupSorted(in []string) []string {
	sort.Strings(in)
	out := in[:0]
	var last string
	first := true
	for _, s := range in {
		if !first && s == last {
			continue
		}
		out = append(out, s)
		last, first = s, false
	}
	return out
}

func routeLess(a, b Route) bool {
	if a.File != b.File {
		return a.File < b.File
	}
	if a.Line != b.Line {
		return a.Line < b.Line
	}
	if a.Method != b.Method {
		return a.Method < b.Method
	}
	return a.Path < b.Path
}

// outIsInsideRepo reports whether outAbs is repoAbs itself or resolves under it (spec item 4).
func outIsInsideRepo(repoAbs, outAbs string) bool {
	rel, err := filepath.Rel(repoAbs, outAbs)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return !strings.HasPrefix(rel, "..")
}

// baseIDForGroup turns a routeGroup into a deterministic, idFormat-legal scenario ID:
// RT-<METHOD>-<slug>, truncated to keep the whole id within the 64-char limit validate.go enforces.
// When the SAME method+path recurs under a DIFFERENT service (g.ambiguousSvc — spec item 3, "six
// GET /health in six services"), the service/package directory is folded into the id instead of a
// numeric `-2 … -6` suffix: RT-<service>-<METHOD>-<slug>, e.g. RT-custody-service-GET-health.
func baseIDForGroup(g routeGroup) string {
	prefix := "RT-"
	hashSeed := g.method + " " + g.path
	if g.ambiguousSvc && g.serviceKey != "" {
		prefix += slugify(g.serviceKey) + "-"
		hashSeed = g.serviceKey + " " + hashSeed
	}
	slug := slugify(g.path)
	id := prefix + g.method + "-" + slug
	if len(id) > 64 {
		// Keep it deterministic and unique-ish when truncated: fold in a short hash of the full path.
		h := sha256.Sum256([]byte(hashSeed))
		suffix := "-" + hex.EncodeToString(h[:])[:8]
		max := 64 - len(suffix)
		if max < 0 {
			max = 0
		}
		id = id[:max] + suffix
	}
	return id
}

var nonSlug = regexp.MustCompile(`[^A-Za-z0-9]+`)

func slugify(path string) string {
	s := nonSlug.ReplaceAllString(path, "-")
	s = strings.Trim(s, "-")
	s = strings.ToLower(s)
	if s == "" {
		s = "root"
	}
	return s
}

func withID(body, id string) string {
	return strings.Replace(body, "__SCENARIO_ID__", id, 1)
}

type builtDraft struct {
	group    routeGroup
	body     string
	err      string
	proposed bool
}

// notProposedReason is the exact wording spec item 4 requires for a money_handling GET this tool
// declines to generate: "not proposed: money_handling — generation proposes only probe/health/
// metrics paths; a human may author reads by hand".
const notProposedReason = "not proposed: money_handling — generation proposes only probe/health/metrics paths; a human may author reads by hand"

// buildDrafts turns each deduplicated group into a scenario draft and gates it through ValidateAll
// (T5.3) and, when moneyHandling is set, MoneyGuardViolations (T5.4) — kept as defence in depth even
// though the money_handling GET-path allowlist below (spec item 4) already screens most of what
// MoneyGuardViolations would otherwise have to catch.
func buildDrafts(groups []routeGroup, moneyHandling bool, rep *Report) []builtDraft {
	out := make([]builtDraft, 0, len(groups))
	for _, g := range groups {
		if moneyHandling && g.method == getMethod && !isMoneyHandlingAllowedGet(g.path) {
			rep.NotProposed = append(rep.NotProposed, Refusal{
				Method: g.method, Path: g.path, Source: source(g.sources[0]), Reason: notProposedReason,
			})
			continue
		}
		body, proposed := draftBody(g, moneyHandling)
		parsed, verrs := toolcore.ValidateAll(withID(body, "RT-VALIDATE-PLACEHOLDER"))
		if len(verrs) > 0 {
			var msgs []string
			for _, e := range verrs {
				msgs = append(msgs, e.String())
			}
			out = append(out, builtDraft{group: g, err: "failed validate-scenario: " + strings.Join(msgs, "; ")})
			continue
		}
		if moneyHandling {
			// money_writes (2026-09-26 follow-up) is deliberately NOT threaded through here: this
			// tool's job is READ-ONLY discovery of routes a REAL repo exposes, and generating a
			// non-GET draft for one that happens to match a SUT's allowlist is a bigger scope change
			// than this door needs — a human authors a real write by hand, same as today. Passing nil
			// keeps this call's behaviour BYTE-IDENTICAL to before money_writes existed.
			if v := scenario.MoneyGuardViolations(parsed, nil); len(v) > 0 {
				out = append(out, builtDraft{group: g, err: "refused by the money-path guard: " + strings.Join(v, "; ")})
				continue
			}
		}
		out = append(out, builtDraft{group: g, body: body, proposed: proposed})
	}
	return out
}
