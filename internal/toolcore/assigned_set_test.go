package toolcore

import "testing"

// V32 Release QA finding (c) — A CLOUD RUN WAS REPORTED AS A LOCAL ONE.
//
// Measured 2026-09-14 on orderservice-compose, run 20260914T152153182: the control plane assigned the
// set, the executor materialized it into a temp directory and ran it — and report.json said
// `scenario_source: "local"`. The federated path builds its Env with no SetFetcher (the set has
// already been fetched by the poll), so resolveScenarioDir fell into branch 1, "no catalog wired, the
// local directory is all there is", and labelled a catalog-delivered set as the operator's own files.
//
// VR-F6 exists so a reader can tell a verified set from one nobody checked. A label that says "local"
// for the catalog's set is the same false statement in the other direction.

func TestResolveScenarioDir_AnAssignedSetIsLabelledCatalog(t *testing.T) {
	dir := t.TempDir()
	writeScenario(t, dir, "http-ingestion/X-001.md")
	e := Env{ScenariosDir: dir, ResultsRoot: t.TempDir(), AssignedSet: true}

	got, src, cleanup, err := resolveScenarioDir(e, "run-assigned", "", "", "")
	if err != nil {
		t.Fatalf("resolveScenarioDir: %v", err)
	}
	defer cleanup()
	if got != dir {
		t.Errorf("an assigned set is run from the directory it was materialized into (%q), got %q", dir, got)
	}
	if src != sourceCatalog {
		t.Errorf("a set the control plane ASSIGNED is the catalog's set, so the source is %q — got %q", sourceCatalog, src)
	}
}

// The control: the standalone rig, with no catalog and no assignment, is still `local`. Without this
// the fix above could pass by relabelling everything.
func TestResolveScenarioDir_AStandaloneLocalDirIsStillLocal(t *testing.T) {
	dir := t.TempDir()
	writeScenario(t, dir, "http-ingestion/X-001.md")
	e := Env{ScenariosDir: dir, ResultsRoot: t.TempDir()}

	_, src, cleanup, err := resolveScenarioDir(e, "run-standalone", "", "", "")
	if err != nil {
		t.Fatalf("resolveScenarioDir: %v", err)
	}
	defer cleanup()
	if src != sourceLocal {
		t.Errorf("a standalone rig with no catalog runs its own files, so the source is %q — got %q", sourceLocal, src)
	}
}
