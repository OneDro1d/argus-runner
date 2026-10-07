package toolcore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// U2 — where a run's scenarios come from.
//
// On the compose tier the operator's local directory is the source of truth and the fast
// fix-loop depends on it. On a k8s executor with min-3 replicas there IS no shared local
// directory: /scenarios is a per-pod emptyDir, so a run must be able to source its set from
// the control-plane catalog instead. These pin both, and — most importantly — pin that the
// two failure shapes stay DISTINGUISHABLE, because conflating them is a false green.

func writeScenario(t *testing.T, dir, rel string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "# Scenario: x\n\n## Metadata\n- **ID**: X-001\n- **Layer**: HTTP Ingestion\n\n" +
		"## TRIGGER\nPOST `${INGESTION_URL}/x`\n\n## EXPECT\n- status=202\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A non-empty LOCAL directory always wins: the compose fix-loop must keep working exactly as
// before, with no CP round-trip and no behaviour change.
// REWRITTEN for VR-F6/INT-020. This test used to assert:
//
//	if fetched { t.Error("a non-empty local dir must NOT trigger a catalog fetch (the compose
//	                      fix-loop must stay offline)") }
//
// which encoded the DEFECT as the contract. A local directory winning unconditionally is exactly how
// a scenario edited in the registry and then run locally executed the OLD copy on disk and reported
// a pass for a version nobody was testing.
//
// The concern behind it was legitimate and is preserved: the fix-loop must not pay a full set
// transfer per iteration. That is what the HASH is for, and the fast path is covered in
// catalogsync_test.go. What this test now pins is the case with NO cheap question available — no
// SetHasher — where the only correct move is to ask the authority.
func TestResolveScenarioDir_consultsTheCatalogEvenWithALocalDir(t *testing.T) {
	dir := t.TempDir()
	writeScenario(t, dir, "http-ingestion/X-001.md")

	var fetched bool
	e := Env{
		ScenariosDir: dir,
		ResultsRoot:  t.TempDir(),
		SetFetcher: func(context.Context, string, string, string) ([]CatalogScenario, error) {
			fetched = true
			return []CatalogScenario{{Path: "http-ingestion/X-001.md", Body: "# Scenario: x\n\n## Metadata\n- **ID**: X-001\n- **Layer**: HTTP Ingestion\n\n## TRIGGER\nPOST `${INGESTION_URL}/x`\n\n## EXPECT\n- status=202\n"}}, nil
		},
	}
	got, src, cleanup, err := resolveScenarioDir(e, "run1", "", "", "")
	if err != nil {
		t.Fatalf("resolveScenarioDir: %v", err)
	}
	defer cleanup()
	if !fetched {
		t.Error("the catalog was NOT consulted. With no SetHasher there is no cheap way to ask " +
			"'has anything changed', so the only correct answer is to fetch the authority's set.")
	}
	if src != sourceCatalog {
		t.Errorf("source = %q, want %q", src, sourceCatalog)
	}
	if got == dir {
		t.Errorf("the run used the local dir %q rather than the materialized catalog set", dir)
	}
}

// The k8s case: no local set, but the catalog has one. The run must materialize and proceed —
// this is what lets three replicas run with no shared volume and no kubectl cp.
func TestResolveScenarioDir_fallsBackToCatalog(t *testing.T) {
	e := Env{
		ScenariosDir: t.TempDir(), // empty
		ResultsRoot:  t.TempDir(),
		SetFetcher: func(context.Context, string, string, string) ([]CatalogScenario, error) {
			return []CatalogScenario{
				{Path: "http-ingestion/A-001.md", Body: "# A\n"},
				{Path: "permissions/B-002.md", Body: "# B\n"},
			}, nil
		},
	}
	got, src, cleanup, err := resolveScenarioDir(e, "run2", "", "", "")
	if err != nil {
		t.Fatalf("resolveScenarioDir: %v", err)
	}
	defer cleanup()
	if src != sourceCatalog {
		t.Fatalf("source = %q, want %q", src, sourceCatalog)
	}
	for _, rel := range []string{"http-ingestion/A-001.md", "permissions/B-002.md"} {
		if _, err := os.Stat(filepath.Join(got, rel)); err != nil {
			t.Errorf("catalog scenario %s was not materialized: %v", rel, err)
		}
	}
}

// A path escape in a catalog-supplied path must not write outside the materialized dir.
func TestResolveScenarioDir_rejectsPathEscape(t *testing.T) {
	root := t.TempDir()
	e := Env{
		ScenariosDir: t.TempDir(),
		ResultsRoot:  root,
		SetFetcher: func(context.Context, string, string, string) ([]CatalogScenario, error) {
			return []CatalogScenario{{Path: "../../escaped.md", Body: "# nope\n"}}, nil
		},
	}
	got, _, cleanup, err := resolveScenarioDir(e, "run3", "", "", "")
	if err != nil {
		t.Fatalf("resolveScenarioDir: %v", err)
	}
	defer cleanup()
	if _, err := os.Stat(filepath.Join(root, "escaped.md")); err == nil {
		t.Error("a ../ path in a catalog scenario escaped the materialized dir")
	}
	if _, err := os.Stat(filepath.Join(got, "escaped.md")); err != nil {
		t.Errorf("the escaping path should have been clamped INTO the materialized dir: %v", err)
	}
}

// ── The false-green guard — the whole point of U2's safety half ──────────────────────

// No local set AND no way to ask the catalog: we cannot tell "legitimately empty" from
// "delivery failed". That is an UNKNOWN, and an unknown must never render as a pass.
func TestResolveScenarioDir_refusesWhenSourceIsUnknowable(t *testing.T) {
	e := Env{ScenariosDir: t.TempDir(), ResultsRoot: t.TempDir()} // no SetFetcher
	_, _, cleanup, err := resolveScenarioDir(e, "run4", "", "", "")
	if cleanup != nil {
		cleanup()
	}
	if err == nil {
		t.Fatal("an empty scenario dir with no catalog source must REFUSE — reporting a 0-scenario run as passed is a false green")
	}
	if !errors.Is(err, ErrNoScenarioSource) {
		t.Errorf("err = %v, want ErrNoScenarioSource", err)
	}
	if !strings.Contains(err.Error(), "0 scenarios") && !strings.Contains(err.Error(), "no scenarios") {
		t.Errorf("the refusal must say plainly that nothing would run; got %q", err)
	}
}

// A reachable catalog that reports ZERO active scenarios must also REFUSE.
//
// Owner ruling 2026-07-22, superseding the earlier "a full-scope run against a legitimately
// empty instance is allowed": if there is nothing to run, the run cannot happen and the agent
// must say so. There is no useful sense in which a run of nothing succeeded, so the reason for
// the emptiness no longer changes the outcome — only the wording.
func TestResolveScenarioDir_refusesWhenCatalogIsEmpty(t *testing.T) {
	e := Env{
		Instance:     "orderservice-k3d",
		ScenariosDir: t.TempDir(),
		ResultsRoot:  t.TempDir(),
		SetFetcher: func(context.Context, string, string, string) ([]CatalogScenario, error) {
			return nil, nil // reachable, genuinely empty
		},
	}
	_, _, cleanup, err := resolveScenarioDir(e, "run5", "", "", "")
	if cleanup != nil {
		cleanup()
	}
	if err == nil {
		t.Fatal("a run against an instance with 0 scenarios must be REFUSED — there is nothing to run")
	}
	if !errors.Is(err, ErrNoScenarioSource) {
		t.Errorf("err = %v, want ErrNoScenarioSource", err)
	}
	// The message must be usable by an agent relaying it to a human: which instance, and what to do.
	for _, want := range []string{"orderservice-k3d", "no scenarios"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal message must mention %q; got %q", want, err)
		}
	}
}

// A KNOWN-empty catalog and an UNREACHABLE one both refuse, but must not say the same thing —
// one is "you have no scenarios", the other is "I could not find out".
func TestResolveScenarioDir_emptyAndUnreachableGiveDifferentReasons(t *testing.T) {
	mk := func(f func(context.Context, string, string, string) ([]CatalogScenario, error)) string {
		e := Env{Instance: "inst-a", ScenariosDir: t.TempDir(), ResultsRoot: t.TempDir(), SetFetcher: f}
		_, _, cleanup, err := resolveScenarioDir(e, "r", "", "", "")
		if cleanup != nil {
			cleanup()
		}
		if err == nil {
			t.Fatal("expected a refusal")
		}
		return err.Error()
	}
	empty := mk(func(context.Context, string, string, string) ([]CatalogScenario, error) { return nil, nil })
	unreach := mk(func(context.Context, string, string, string) ([]CatalogScenario, error) {
		return nil, errors.New("dial tcp: connection refused")
	})
	if empty == unreach {
		t.Fatal("an empty catalog and an unreachable one must be distinguishable to the operator")
	}
	if strings.Contains(empty, "could not be reached") {
		t.Errorf("a KNOWN-empty catalog must not be reported as unreachable; got %q", empty)
	}
}

// The CP was UNREACHABLE. We do not know whether scenarios exist, so we must refuse rather
// than run zero and call it green. This is the exact k8s delivery-failure shape.
func TestResolveScenarioDir_refusesWhenCatalogUnreachable(t *testing.T) {
	e := Env{
		ScenariosDir: t.TempDir(),
		ResultsRoot:  t.TempDir(),
		SetFetcher: func(context.Context, string, string, string) ([]CatalogScenario, error) {
			return nil, errors.New("dial tcp: connection refused")
		},
	}
	_, _, cleanup, err := resolveScenarioDir(e, "run6", "", "", "")
	if cleanup != nil {
		cleanup()
	}
	if err == nil {
		t.Fatal("an unreachable catalog with no local set must REFUSE — running 0 scenarios and reporting green is the failure mode this guards")
	}
	if !errors.Is(err, ErrNoScenarioSource) {
		t.Errorf("err = %v, want ErrNoScenarioSource", err)
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("the refusal must carry WHY the catalog could not be consulted; got %q", err)
	}
}

// The selection must reach the catalog, or a filtered run would silently fetch everything.
func TestResolveScenarioDir_passesSelectionToCatalog(t *testing.T) {
	var gotLayer, gotTag, gotID string
	e := Env{
		ScenariosDir: t.TempDir(),
		ResultsRoot:  t.TempDir(),
		SetFetcher: func(_ context.Context, layer, tag, id string) ([]CatalogScenario, error) {
			gotLayer, gotTag, gotID = layer, tag, id
			return []CatalogScenario{{Path: "a.md", Body: "# a\n"}}, nil
		},
	}
	_, _, cleanup, err := resolveScenarioDir(e, "run7", "Permissions", "critical", "PERM-002")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if gotLayer != "Permissions" || gotTag != "critical" || gotID != "PERM-002" {
		t.Errorf("selection reached the catalog as (%q,%q,%q), want (Permissions,critical,PERM-002)", gotLayer, gotTag, gotID)
	}
}

// The materialized dir must not survive the run — a leftover set would be picked up as a
// "local" set by the NEXT run and silently shadow the catalog.
func TestResolveScenarioDir_cleanupRemovesMaterializedSet(t *testing.T) {
	e := Env{
		ScenariosDir: t.TempDir(),
		ResultsRoot:  t.TempDir(),
		SetFetcher: func(context.Context, string, string, string) ([]CatalogScenario, error) {
			return []CatalogScenario{{Path: "a.md", Body: "# a\n"}}, nil
		},
	}
	dir, _, cleanup, err := resolveScenarioDir(e, "run8", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("materialized dir missing before cleanup: %v", err)
	}
	cleanup()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("materialized dir survived cleanup (%v) — the next run would mistake it for a local set", err)
	}
}

// "Nothing matched your selection" and "this instance has nothing at all" are different
// problems with different fixes. Saying the instance is empty when the catalog is full and it
// was the filter that matched nothing sends the operator to the wrong place entirely.
func TestResolveScenarioDir_emptyResultDistinguishesSelectionFromEmptyInstance(t *testing.T) {
	newEnv := func() Env {
		return Env{
			Instance:     "orderservice-k3d",
			ScenariosDir: t.TempDir(),
			ResultsRoot:  t.TempDir(),
			SetFetcher: func(context.Context, string, string, string) ([]CatalogScenario, error) {
				return nil, nil
			},
		}
	}
	msg := func(layer, tag, id string) string {
		_, _, cleanup, err := resolveScenarioDir(newEnv(), "r", layer, tag, id)
		if cleanup != nil {
			cleanup()
		}
		if err == nil {
			t.Fatal("expected a refusal")
		}
		return err.Error()
	}

	byID := msg("", "", "NOPE-999")
	if !strings.Contains(byID, "NOPE-999") {
		t.Errorf("a 0-match on a scenario id must name the id; got %q", byID)
	}
	if strings.Contains(byID, "has no scenarios to run") {
		t.Errorf("a 0-match on a SELECTION must not claim the instance is empty; got %q", byID)
	}
	if !strings.Contains(msg("", "critical", ""), "critical") {
		t.Error("a 0-match on a tag must name the tag")
	}
	if !strings.Contains(msg("Permissions", "", ""), "Permissions") {
		t.Error("a 0-match on a layer must name the layer")
	}

	unfiltered := msg("", "", "")
	if !strings.Contains(unfiltered, "has no scenarios to run") {
		t.Errorf("an UNFILTERED run against an empty catalog must say the instance has nothing; got %q", unfiltered)
	}
}
