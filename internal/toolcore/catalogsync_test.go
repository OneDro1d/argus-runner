package toolcore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/report"
)

// VR-F6 / INT-020 — a DIRECT local run SYNCS from the catalog before it executes.
//
// UC059 ("the hybrid, half 1") specifies: edit a scenario in the registry → trigger a direct local
// run → the in-env server asks for a FRESH materialization → the run executes the just-edited
// version. Step 3 was short-circuited whenever local files existed:
//
//	// 1. A non-empty local directory always wins — the compose fix-loop keeps working exactly
//	//    as before and never pays for a CP round-trip.
//	if len(scenario.DiscoverFiles(e.ScenariosDir)) > 0 { return e.ScenariosDir, sourceLocal, noop, nil }
//
// So a direct run could execute a DIFFERENT scenario from the one just authored, pass, and report
// that pass — for a version nobody was testing. Severity is not "stale files": it is a green result
// over a set that was never the set.
//
// The comment was protecting something real, which is why the fix is a HASH and not just "always
// fetch". Consulting the catalog on every fix-loop iteration is only affordable if "nothing changed"
// is cheap; at a full set transfer per run it becomes something people switch off.

const mdA = "## Metadata\n- **ID**: A-001\n- **Layer**: HTTP Ingestion\n\n## Steps\n1. GET /a\n"
const mdB = "## Metadata\n- **ID**: A-001\n- **Layer**: HTTP Ingestion\n\n## Steps\n1. GET /b\n"

func writeScenarioFile(t *testing.T, dir, rel, body string) {
	t.Helper()
	full := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// THE FAST PATH. The catalog agrees with the directory, so nothing is transferred and the run uses
// the local files — but the label records that the catalog was ASKED and AGREED.
func TestResolveScenarioDir_MatchingHashRunsLocalAndSaysItWasVerified(t *testing.T) {
	dir := t.TempDir()
	writeScenarioFile(t, dir, "http-ingestion/A-001.md", mdA)

	local, ok := localSetHash(dir)
	if !ok || local == "" {
		t.Fatal("localSetHash produced nothing for a directory with one scenario")
	}

	fetched := false
	e := Env{
		ScenariosDir: dir, ResultsRoot: t.TempDir(), Instance: "inst",
		SetHasher: func(context.Context, string, string, string) (string, error) { return local, nil },
		SetFetcher: func(context.Context, string, string, string) ([]CatalogScenario, error) {
			fetched = true
			return []CatalogScenario{{Path: "http-ingestion/A-001.md", Body: mdB}}, nil
		},
	}
	got, source, cleanup, err := resolveScenarioDir(e, "run1", "", "", "")
	defer cleanup()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != dir {
		t.Errorf("dir = %q, want the local dir %q", got, dir)
	}
	if source != sourceLocalVerified {
		t.Errorf("source = %q, want %q — the label is how a reader tells a VERIFIED local set from "+
			"one nobody checked", source, sourceLocalVerified)
	}
	if fetched {
		t.Error("the full set was fetched even though the hashes matched — that is the cost the " +
			"hash-only endpoint exists to avoid, and paying it every run is how the check gets " +
			"switched off")
	}
}

// THE DEFECT ITSELF. The catalog holds a DIFFERENT body; the run must execute the catalog's version.
func TestResolveScenarioDir_DifferingHashRunsTheCatalogVersion(t *testing.T) {
	dir := t.TempDir()
	writeScenarioFile(t, dir, "http-ingestion/A-001.md", mdA) // the OLD copy on disk

	e := Env{
		ScenariosDir: dir, ResultsRoot: t.TempDir(), Instance: "inst",
		SetHasher: func(context.Context, string, string, string) (string, error) {
			return "a-different-hash", nil
		},
		SetFetcher: func(context.Context, string, string, string) ([]CatalogScenario, error) {
			return []CatalogScenario{{Path: "http-ingestion/A-001.md", Body: mdB}}, nil
		},
	}
	got, source, cleanup, err := resolveScenarioDir(e, "run2", "", "", "")
	defer cleanup()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if source != sourceCatalog {
		t.Fatalf("source = %q, want %q — the catalog is the authority on what the set IS", source, sourceCatalog)
	}
	b, rerr := os.ReadFile(filepath.Join(got, "http-ingestion", "A-001.md"))
	if rerr != nil {
		t.Fatalf("read materialized: %v", rerr)
	}
	if string(b) != mdB {
		t.Errorf("the run would execute the OLD body.\n"+
			"  got:  %q\n  want: %q\n"+
			"  This is INT-020: a scenario edited in the registry, run locally, passing for a version\n"+
			"  nobody was testing.", b, mdB)
	}
}

// A scenario on disk that the catalog does not know about is IGNORED — left in place, not run.
func TestResolveScenarioDir_OnDiskExtrasAreNotRun(t *testing.T) {
	dir := t.TempDir()
	writeScenarioFile(t, dir, "http-ingestion/A-001.md", mdA)
	writeScenarioFile(t, dir, "http-ingestion/ROGUE-9.md", mdB) // never authored to the catalog

	e := Env{
		ScenariosDir: dir, ResultsRoot: t.TempDir(), Instance: "inst",
		SetHasher: func(context.Context, string, string, string) (string, error) { return "differs", nil },
		SetFetcher: func(context.Context, string, string, string) ([]CatalogScenario, error) {
			return []CatalogScenario{{Path: "http-ingestion/A-001.md", Body: mdA}}, nil
		},
	}
	got, _, cleanup, err := resolveScenarioDir(e, "run3", "", "", "")
	defer cleanup()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if _, serr := os.Stat(filepath.Join(got, "http-ingestion", "ROGUE-9.md")); serr == nil {
		t.Error("a scenario that exists only on disk reached the run set — the catalog decides what " +
			"the set IS, and a file nobody authored must not silently join it")
	}
	// LEFT IN PLACE: ignoring it is not deleting it. Someone's work-in-progress is not ours to remove.
	if _, serr := os.Stat(filepath.Join(dir, "http-ingestion", "ROGUE-9.md")); serr != nil {
		t.Errorf("the on-disk scenario was REMOVED (%v) — it must be ignored, not deleted", serr)
	}
}

// THE OUTAGE — and the owner's ruling of 2026-08-11, which changed what this does.
//
//	"I don't want to force each user to wait until it is available and allow them continue using it
//	 with plan B: use scenarios stored on the disk. But user must explicitly confirm Plan B.
//	 Otherwise by default user is waiting until CP gets available."
//
// So the DEFAULT IS TO REFUSE. An earlier draft ran the local set and labelled it `local-unverified`,
// which still produces a green result over a set nobody verified — the label documents the defect
// rather than preventing it.
func TestResolveScenarioDir_UnreachableCatalogRefusesAndOffersTheChoice(t *testing.T) {
	dir := t.TempDir()
	writeScenarioFile(t, dir, "http-ingestion/A-001.md", mdA)

	down := func(context.Context, string, string, string) ([]CatalogScenario, error) {
		return nil, errors.New("dial tcp: connection refused")
	}
	e := Env{
		ScenariosDir: dir, ResultsRoot: t.TempDir(), Instance: "inst",
		SetHasher: func(context.Context, string, string, string) (string, error) {
			return "", errors.New("dial tcp: connection refused")
		},
		SetFetcher: down,
	}
	_, _, cleanup, err := resolveScenarioDir(e, "run4", "", "", "")
	defer cleanup()
	if err == nil {
		t.Fatal("the run PROCEEDED with an unreachable control plane.\n" +
			"  The default is to wait: running the local set without being asked is what this fixes.")
	}
	if !errors.Is(err, ErrCatalogUnreachable) {
		t.Errorf("err = %v, want ErrCatalogUnreachable — a caller must be able to tell this apart from "+
			"\"you have no scenarios\", because only one of them has a choice attached", err)
	}
	// The message IS the feature: it has to carry both options, and waiting has to read as the default.
	msg := err.Error()
	for _, want := range []string{"has NOT started", "WAIT", "use_local_scenarios", "--use-local-scenarios", "unverified"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal does not mention %q:\n%s", want, msg)
		}
	}
	if !strings.Contains(msg, "1 scenario file(s)") {
		t.Errorf("the refusal does not say WHAT is on disk — a choice between two options needs to "+
			"state what the second one would run:\n%s", msg)
	}
}

// PLAN B, explicitly chosen: the run proceeds and the label records that nobody verified the set.
func TestResolveScenarioDir_UseLocalScenariosRunsTheLocalSet(t *testing.T) {
	dir := t.TempDir()
	writeScenarioFile(t, dir, "http-ingestion/A-001.md", mdA)
	e := Env{
		ScenariosDir: dir, ResultsRoot: t.TempDir(), Instance: "inst",
		UseLocalScenarios: true,
		SetHasher: func(context.Context, string, string, string) (string, error) {
			return "", errors.New("connection refused")
		},
		SetFetcher: func(context.Context, string, string, string) ([]CatalogScenario, error) {
			return nil, errors.New("connection refused")
		},
	}
	got, source, cleanup, err := resolveScenarioDir(e, "run4b", "", "", "")
	defer cleanup()
	if err != nil {
		t.Fatalf("an explicit plan-B choice was refused: %v", err)
	}
	if got != dir {
		t.Errorf("dir = %q, want the local dir", got)
	}
	if source != sourceLocalUnverified {
		t.Errorf("source = %q, want %q — the choice must stay visible in the report after the moment "+
			"it was made", source, sourceLocalUnverified)
	}
}

// THE FLAG IS NOT AN OVERRIDE. With the catalog UP it changes nothing: the catalog is still the
// authority. A flag that also skipped a reachable catalog would quietly become the way people run.
func TestResolveScenarioDir_UseLocalScenariosDoesNothingWhenTheCatalogAnswers(t *testing.T) {
	dir := t.TempDir()
	writeScenarioFile(t, dir, "http-ingestion/A-001.md", mdA) // the OLD copy
	e := Env{
		ScenariosDir: dir, ResultsRoot: t.TempDir(), Instance: "inst",
		UseLocalScenarios: true,
		SetHasher:         func(context.Context, string, string, string) (string, error) { return "differs", nil },
		SetFetcher: func(context.Context, string, string, string) ([]CatalogScenario, error) {
			return []CatalogScenario{{Path: "http-ingestion/A-001.md", Body: mdB}}, nil
		},
	}
	got, source, cleanup, err := resolveScenarioDir(e, "run4c", "", "", "")
	defer cleanup()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if source != sourceCatalog {
		t.Fatalf("source = %q, want %q — use_local_scenarios is plan B for an OUTAGE, not a way to "+
			"opt out of the catalog", source, sourceCatalog)
	}
	b, _ := os.ReadFile(filepath.Join(got, "http-ingestion", "A-001.md"))
	if string(b) != mdB {
		t.Error("the flag caused the stale local body to run even though the catalog answered")
	}
}

// A REFUSED CREDENTIAL GETS NO PLAN B, even with the flag set. An outage is temporary and the
// operator's files are a reasonable stand-in; a withdrawn authorisation is not temporary, and running
// anyway would be running without the permission that was just refused.
func TestResolveScenarioDir_ARejectedCredentialIsNotAnOutage(t *testing.T) {
	dir := t.TempDir()
	writeScenarioFile(t, dir, "http-ingestion/A-001.md", mdA)
	for _, optedIn := range []bool{false, true} {
		e := Env{
			ScenariosDir: dir, ResultsRoot: t.TempDir(), Instance: "inst",
			UseLocalScenarios: optedIn,
			SetHasher: func(context.Context, string, string, string) (string, error) {
				return "", fmt.Errorf("%w: set-hash returned 401", ErrCatalogUnauthorized)
			},
			SetFetcher: func(context.Context, string, string, string) ([]CatalogScenario, error) {
				return nil, fmt.Errorf("%w: materialize returned 401", ErrCatalogUnauthorized)
			},
		}
		_, _, cleanup, err := resolveScenarioDir(e, "run4d", "", "", "")
		cleanup()
		if err == nil {
			t.Fatalf("use_local_scenarios=%v: a REFUSED credential let the run proceed.\n"+
				"  Local scenarios are not a workaround for authorisation that was withdrawn.", optedIn)
		}
		if errors.Is(err, ErrCatalogUnreachable) {
			t.Errorf("use_local_scenarios=%v: a 401 was reported as an outage, so the caller would "+
				"offer a choice that does not exist: %v", optedIn, err)
		}
		if !strings.Contains(err.Error(), "REFUSED") {
			t.Errorf("use_local_scenarios=%v: the refusal does not say the credential was rejected: %v", optedIn, err)
		}
	}
}

// No catalog wired at all is a legitimate standalone rig — unchanged behaviour, and NOT labelled
// unverified, because there is no authority that failed to answer.
func TestResolveScenarioDir_NoCatalogIsStillPlainLocal(t *testing.T) {
	dir := t.TempDir()
	writeScenarioFile(t, dir, "http-ingestion/A-001.md", mdA)
	_, source, cleanup, err := resolveScenarioDir(
		Env{ScenariosDir: dir, ResultsRoot: t.TempDir(), Instance: "inst"}, "run5", "", "", "")
	defer cleanup()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if source != sourceLocal {
		t.Errorf("source = %q, want %q — an unwired rig has no authority to be unverified against", source, sourceLocal)
	}
}

// localSetHash must be stable and must actually notice a change; if it did not, the fast path would
// either never fire or fire when it must not.
func TestLocalSetHash_StableAndChangeSensitive(t *testing.T) {
	dir := t.TempDir()
	writeScenarioFile(t, dir, "b/B-001.md", mdA)
	writeScenarioFile(t, dir, "a/A-001.md", mdB)

	h1, ok := localSetHash(dir)
	if !ok {
		t.Fatal("no hash for a populated directory")
	}
	if h2, _ := localSetHash(dir); h2 != h1 {
		t.Error("the hash is not stable across calls — ordering must not depend on walk order")
	}
	writeScenarioFile(t, dir, "a/A-001.md", mdA) // one byte of content changes
	if h3, _ := localSetHash(dir); h3 == h1 {
		t.Error("a CHANGED body produced the same hash — the fast path would run a stale set and " +
			"call it verified, which is worse than the defect being fixed")
	}
	if _, ok := localSetHash(t.TempDir()); ok {
		t.Error("an empty directory reported a hash")
	}
}

// VR-F6 — the CHOICE must survive into the report, not just the tool response.
//
// The tool response is read once, by the agent that made the call. The report is what anyone
// consults afterwards, and a green report over an unverified local set is a DIFFERENT CLAIM from a
// green report over the catalog's set. If only the response carries the distinction, the claim
// silently upgrades itself the moment the conversation ends.
func TestReport_CarriesTheScenarioSourceAndTheCaveat(t *testing.T) {
	// The field exists and is JSON-visible under a stable name.
	r := report.Report{Project: "p", ScenarioSource: sourceLocalUnverified}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"scenario_source":"local-unverified"`) {
		t.Errorf("the report does not serialise the scenario source: %s", b)
	}
	// And it is OMITTED when unset, so every pre-existing report stays byte-identical.
	b2, _ := json.Marshal(report.Report{Project: "p"})
	if strings.Contains(string(b2), "scenario_source") {
		t.Errorf("an unset source was serialised, changing every existing report: %s", b2)
	}

	// The human-readable caveat names the cause, the choice, and what to do about it.
	for _, want := range []string{"UNREACHABLE", "use_local_scenarios", "NOT verified", "Re-run"} {
		if !strings.Contains(unverifiedRunNote, want) {
			t.Errorf("the report note does not mention %q: %s", want, unverifiedRunNote)
		}
	}
}
