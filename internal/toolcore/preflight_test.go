package toolcore

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// PreflightRun answers "would this run execute anything?" BEFORE the run is started.
//
// runner__run is asynchronous: it returns a run_id and the agent polls for a report. Without a
// synchronous pre-check, a run that can only execute nothing still hands back a run_id, and the
// agent has to infer the problem from total:0 in a later report. The owner's requirement is that
// the agent SAYS the run cannot be run, so the answer has to be available at call time.

func TestPreflightRun_passesWhenLocalSetHasAMatch(t *testing.T) {
	dir := t.TempDir()
	writeScenario(t, dir, "http-ingestion/X-001.md")
	e := Env{ScenariosDir: dir}
	if err := PreflightRun(e, "", "", "X-001"); err != nil {
		t.Errorf("a matching scenario must preflight clean, got %v", err)
	}
	if err := PreflightRun(e, "", "", ""); err != nil {
		t.Errorf("an unfiltered run over a non-empty set must preflight clean, got %v", err)
	}
}

func TestPreflightRun_refusesUnmatchedSelectionOnLocalSet(t *testing.T) {
	dir := t.TempDir()
	writeScenario(t, dir, "http-ingestion/X-001.md")
	e := Env{Instance: "orderservice-compose", ScenariosDir: dir}

	err := PreflightRun(e, "", "", "NOPE-999")
	if err == nil {
		t.Fatal("a scenario id matching nothing must be refused up front — there is nothing to run")
	}
	if !strings.Contains(err.Error(), "NOPE-999") {
		t.Errorf("the refusal must name the selection; got %q", err)
	}
	if !strings.Contains(err.Error(), "cannot run") {
		t.Errorf("the refusal must say plainly that the run cannot run; got %q", err)
	}
	// It must NOT claim the instance is empty — the set has scenarios, just not this one.
	if strings.Contains(err.Error(), "has no scenarios to run") {
		t.Errorf("a selection miss must not be reported as an empty instance; got %q", err)
	}
}

func TestPreflightRun_refusesWhenThereIsNoSetAtAll(t *testing.T) {
	e := Env{Instance: "inst-x", ScenariosDir: t.TempDir()} // empty, no catalog
	err := PreflightRun(e, "", "", "")
	if err == nil {
		t.Fatal("no local set and no catalog must be refused up front")
	}
	if !errors.Is(err, ErrNoScenarioSource) {
		t.Errorf("err = %v, want ErrNoScenarioSource", err)
	}
}

// A local set that is EMPTY but a catalog that has scenarios must preflight CLEAN — that is the
// k8s shape, and refusing it would break the catalog delivery path entirely.
func TestPreflightRun_passesWhenOnlyTheCatalogHasScenarios(t *testing.T) {
	e := Env{
		ScenariosDir: t.TempDir(),
		SetFetcher: func(context.Context, string, string, string) ([]CatalogScenario, error) {
			return []CatalogScenario{{Path: "a.md", Body: "# a\n"}}, nil
		},
	}
	if err := PreflightRun(e, "", "", ""); err != nil {
		t.Errorf("a catalog-only instance must preflight clean, got %v", err)
	}
}

// Preflight must not be destructive: it answers a question, it must not leave a materialized set
// behind that the real run would then discover as a "local" set.
func TestPreflightRun_leavesNothingBehind(t *testing.T) {
	root := t.TempDir()
	e := Env{
		ScenariosDir: t.TempDir(),
		ResultsRoot:  root,
		SetFetcher: func(context.Context, string, string, string) ([]CatalogScenario, error) {
			return []CatalogScenario{{Path: "a.md", Body: "# a\n"}}, nil
		},
	}
	if err := PreflightRun(e, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if entries, _ := readDirNames(root); len(entries) != 0 {
		t.Errorf("preflight left artifacts behind in the results root: %v", entries)
	}
}
