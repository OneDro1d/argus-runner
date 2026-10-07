package toolcore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"github.com/OneDro1d/argus-runner/internal/retry"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// U2 (Stage II) — where a run's scenarios come from.
//
// The compose tier reads them from the operator's local directory, and the fast fix-loop
// depends on that: edit a file, re-run, see the change. A k8s executor at the min-3 axiom has
// no such directory — /scenarios is a per-pod emptyDir, so three pods have three different
// empty dirs and there is nothing a `kubectl cp` can usefully target. Stage II delivered
// scenarios by copying them into the single pod, which is why that run was honest only at
// replicas=1.
//
// So a run resolves its set in this order:
//
//  1. a NON-EMPTY local directory     -> use it (compose path, unchanged, no CP call)
//  2. else the control-plane catalog  -> materialize the fetched set and run that
//  3. else                            -> REFUSE: there is nothing to run
//
// Step 3 is the important one. A run that executes zero scenarios has verified NOTHING, and
// until this change it reported "passed" and pushed a GREEN row to the cloud ledger — proven
// live on 2026-07-22 against an empty directory. On a 3-replica k3d executor that is exactly
// what every run would have done, since a `kubectl cp` can only ever reach one pod.
//
// OWNER RULING 2026-07-22 (supersedes the earlier "a full-scope run against a legitimately
// empty instance is allowed"): if there are no scenarios, the run CANNOT run, and the agent
// must say so plainly. So the REASON for the emptiness no longer changes the outcome — it only
// changes the wording, which still matters: "you have no scenarios" and "I could not find out
// whether you have any" send an operator to different places.

// CatalogScenario is one scenario fetched from the control-plane catalog: its registry path
// and full markdown body. Declared here rather than reusing the federation type so toolcore
// stays decoupled from the federation client (the same reason RunReporter is a plain func).
type CatalogScenario struct {
	Path string
	Body string
}

// ErrNoScenarioSource is returned when a run cannot establish what it was supposed to execute.
// It is deliberately an ERROR and not an empty run: see the package comment above.
var ErrNoScenarioSource = errors.New("no scenario source")

// Where a run's set came from — surfaced on the run output so an operator (and the evidence
// docs) can tell a catalog-delivered run from a local one without guessing.
const (
	sourceLocal   = "local"
	sourceCatalog = "catalog"
	// VR-F6: two more, because "local" alone cannot say whether the catalog AGREED. The label
	// travels into the report, so a reader can tell a verified set from one nobody checked.
	//
	//   local-verified    the catalog was asked and its set hash MATCHES this directory byte for
	//                     byte — the fast path, no transfer, and provably the current set
	//   local-unverified  the catalog could NOT be reached and the CALLER EXPLICITLY ASKED to run
	//                     from disk anyway (UseLocalScenarios). Never reached by default — the
	//                     default is to refuse and let the user choose. The label survives into the
	//                     report so the choice is visible afterwards, not only at the moment it was
	//                     made.
	sourceLocalVerified   = "local-verified"
	sourceLocalUnverified = "local-unverified"
)

// describeSelection renders the filter an operator actually typed, for the refusal message.
// Empty when the run was unfiltered — which is what distinguishes "nothing matched your
// selection" from "this instance has nothing at all".
func describeSelection(layer, tag, scenarioID string) string {
	switch {
	case scenarioID != "":
		return fmt.Sprintf("scenario id %q", scenarioID)
	case tag != "":
		return fmt.Sprintf("tag %q", tag)
	case layer != "":
		return fmt.Sprintf("layer %q", layer)
	default:
		return ""
	}
}

// instanceLabel is the id an operator recognises — the REGISTERED instance where one exists
// (the tool identity stays "local" on a single-tenant executor, which would be useless in a
// refusal message that is meant to tell someone WHICH instance has no scenarios).
func (e Env) instanceLabel() string {
	if e.ObsInstance != "" {
		return e.ObsInstance
	}
	return e.Instance
}

// catalogCallTimeout bounds the WHOLE catalog conversation, retries included. The per-attempt
// deadline and the attempt count live in internal/retry (VR-F2b); this is the outer stop so a wedged
// control plane cannot hold a run open indefinitely. Slightly above the 45 s budget so the retry
// policy — not this timer — is what gives up, and the message a user sees is the policy's.
const catalogCallTimeout = retry.Budget + 10*time.Second

// resolveScenarioDir returns the directory a run should execute from, a label for where the
// set came from, and a cleanup to run afterwards.
// ErrCatalogUnreachable means the control plane did not answer after VR-F2b's full retry policy, and
// a LOCAL scenario set exists — so there is a real choice to put to the user (VR-F6).
//
// Distinct from ErrNoScenarioSource on purpose: that one means "there is nothing to run and nothing
// to decide". This one means "there is something you could run, and only you can say whether you
// want to". A caller that cannot tell them apart would either offer a choice that does not exist or
// hide one that does.
var ErrCatalogUnreachable = errors.New("control plane unreachable")

// ErrCatalogUnauthorized is re-declared here so toolcore can recognise a REFUSED credential without
// importing the federation client (the fetcher is injected as a plain func precisely to avoid that
// dependency). cmd/argus wraps the client's error with this before handing it over.
var ErrCatalogUnauthorized = errors.New("control plane rejected the credential")

// unreachableChoices is the message a human actually reads when the control plane is down.
//
// The owner's shape, and the reason it is not a bare error: "AI agent informs user about it and
// proposes either to use locally stored scenarios or wait until CP is available." So it states what
// was tried, what is on disk, and the TWO options — with waiting first, because waiting is the
// default and running an unverified set is the deliberate departure from it.
func unreachableChoices(instance, dir string, n int, cause error) string {
	return fmt.Sprintf(
		"the control plane did not answer, so the scenario catalog could not be read for instance %q.\n"+
			"  Tried for up to %s (%d attempts). Last error: %v\n"+
			"\n"+
			"  The run has NOT started. There are %d scenario file(s) in %s, but the catalog is the\n"+
			"  authority on what the set IS, and it could not be consulted - so those files may be older\n"+
			"  than what was last authored, or may include scenarios the catalog does not have.\n"+
			"\n"+
			"  Two options:\n"+
			"    1. WAIT and run again once the control plane is back. This is the default, and it is\n"+
			"       the right choice unless you know the local files are current.\n"+
			"    2. Run the LOCAL set anyway, accepting that it is unverified - pass\n"+
			"       use_local_scenarios=true (runner__run) or --use-local-scenarios (the CLI).\n"+
			"       The run and its report will record that the catalog was unreachable and that this\n"+
			"       was an explicit choice.",
		instance, retry.Budget, retry.Attempts, cause, n, dir)
}

func resolveScenarioDir(e Env, runID, layer, tag, scenarioID string) (dir, source string, cleanup func(), err error) {
	noop := func() {}

	// 1. NO CATALOG WIRED: the local directory is all there is, and that is legitimate — a
	//    standalone rig with no control plane. Nothing to sync against.
	if e.SetFetcher == nil && len(scenario.DiscoverFiles(e.ScenariosDir)) > 0 {
		// 1a. …unless the directory IS the catalog's set, materialized by the federated run. Same
		//     directory, different fact: the control plane chose these files, the operator did not.
		if e.AssignedSet {
			return e.ScenariosDir, sourceCatalog, noop, nil
		}
		return e.ScenariosDir, sourceLocal, noop, nil
	}

	// 1b. VR-F6/INT-020 — SYNC FROM THE CATALOG FIRST. This used to be "a non-empty local directory
	//     always wins", which meant a scenario edited in the registry and then run locally executed
	//     the OLD copy on disk and reported a pass for a version nobody was testing.
	//
	//     The fast path is the HASH, not the directory: ask what the catalog's set hash is (~64
	//     bytes) and, when the local directory already matches it byte-for-byte, run from local with
	//     no transfer at all. The fix-loop keeps its speed and stops being able to lie.
	hasLocal := len(scenario.DiscoverFiles(e.ScenariosDir)) > 0
	if e.SetHasher != nil && hasLocal {
		hctx, hcancel := context.WithTimeout(context.Background(), catalogCallTimeout)
		remote, herr := e.SetHasher(hctx, layer, tag, scenarioID)
		hcancel()
		if herr == nil && remote != "" {
			if local, ok := localSetHash(e.ScenariosDir); ok && local == remote {
				return e.ScenariosDir, sourceLocalVerified, noop, nil
			}
			// The hashes DIFFER: fall through to the fetch below, which is the authority. A
			// scenario on disk that the catalog does not know about is left in place and NOT run —
			// the catalog decides what the set IS.
		}
		// herr != nil: the cheap question could not be asked. Fall through — the fetch below tries
		// the same control plane and produces the one message the user should see, rather than two.
	}

	// 3a. Nothing local and no catalog to ask.
	if e.SetFetcher == nil {
		return "", "", noop, fmt.Errorf("%w: cannot run — instance %q has no scenarios to run. There are none in %q and this executor is not wired to a control-plane catalog, so a run would execute 0 scenarios and prove nothing. Point --scenarios at your set, or wire the executor to a control plane",
			ErrNoScenarioSource, e.instanceLabel(), e.ScenariosDir)
	}

	ctx, cancel := context.WithTimeout(context.Background(), catalogCallTimeout)
	defer cancel()
	set, ferr := e.SetFetcher(ctx, layer, tag, scenarioID)
	if ferr != nil {
		// VR-F6 + the owner's ruling (2026-08-11): the control plane could not be reached after the
		// full VR-F2b retry policy, and the DEFAULT IS TO WAIT.
		//
		// "I don't want to force each user to wait until it is available and allow them continue
		//  using it with plan B: use scenarios stored on the disk. But user must explicitly confirm
		//  Plan B. Otherwise by default user is waiting until CP gets available."
		//
		// So this REFUSES and hands the caller the two choices. It does not run. An earlier draft ran
		// the local set and merely labelled it `local-unverified`; that still produces a green result
		// over a set nobody verified, which is the defect this requirement exists to remove — the
		// label just documents it after the fact.
		if errors.Is(ferr, ErrCatalogUnauthorized) {
			// NO PLAN B for a rejected credential. An outage is temporary and the operator's own
			// files are a reasonable stand-in; a withdrawn authorisation is not temporary, and
			// running anyway would be running WITHOUT the permission that was just refused.
			return "", "", noop, fmt.Errorf("%w: cannot run — the control plane REFUSED this executor's credential (%v). This is not an outage and running your local scenarios would not be a workaround for it: the authorisation to run against instance %q has been withdrawn or has expired. Re-onboard, or rotate the machine identity",
				ErrNoScenarioSource, ferr, e.instanceLabel())
		}
		if hasLocal {
			if e.UseLocalScenarios {
				// PLAN B, explicitly chosen. The label travels into the report so the decision stays
				// visible after the moment it was made.
				return e.ScenariosDir, sourceLocalUnverified, noop, nil
			}
			return "", "", noop, fmt.Errorf("%w: %s", ErrCatalogUnreachable,
				unreachableChoices(e.instanceLabel(), e.ScenariosDir, len(scenario.DiscoverFiles(e.ScenariosDir)), ferr))
		}
		// 3c. The catalog could not be CONSULTED and there is no local set either, so there is no
		//     choice to offer. Distinct from "you have none": the operator should go look at
		//     connectivity, not at their scenario set.
		return "", "", noop, fmt.Errorf("%w: cannot run — no scenarios in %q for instance %q, and the control-plane catalog could not be reached to fetch them (%v). Refusing rather than running 0 scenarios and reporting a result",
			ErrNoScenarioSource, e.ScenariosDir, e.instanceLabel(), ferr)
	}
	if len(set) == 0 {
		// 3b. The catalog was reachable and returned nothing to run. WHY it returned nothing
		//     depends on whether a selection was applied, and conflating the two sends the
		//     operator to the wrong place: "your instance has no scenarios" is simply false when
		//     the catalog is full and it was the filter that matched nothing.
		if sel := describeSelection(layer, tag, scenarioID); sel != "" {
			return "", "", noop, fmt.Errorf("%w: cannot run — no scenario matches %s for instance %q, so there is nothing to run. Check the selection (a scenario id is not a tag), or author/activate a matching scenario in the catalog",
				ErrNoScenarioSource, sel, e.instanceLabel())
		}
		return "", "", noop, fmt.Errorf("%w: cannot run — instance %q has no scenarios to run. Its control-plane catalog reports 0 active scenarios and there is no local set in %q. Author or activate scenarios for this instance (or point --scenarios at a directory that has them), then run again",
			ErrNoScenarioSource, e.instanceLabel(), e.ScenariosDir)
	}

	// 2. Materialize the fetched set and run from there.
	dir, cleanup, err = materializeSet(e.ResultsRoot, runID, set)
	if err != nil {
		return "", "", noop, fmt.Errorf("materialize catalog set: %w", err)
	}
	return dir, sourceCatalog, cleanup, nil
}

// materializeSet writes the fetched bodies into a fresh directory, built via a temp dir +
// rename so a concurrent reader never observes a partial set. Mirrors the federated path's
// materializer (internal/runner) — the two must agree, since a cloud run and a catalog-sourced
// direct run should execute byte-identical sets.
func materializeSet(base, runID string, set []CatalogScenario) (dir string, cleanup func(), err error) {
	root := filepath.Join(base, "materialized")
	final := filepath.Join(root, runID)
	tmp := final + ".tmp"
	_ = os.RemoveAll(tmp)
	_ = os.RemoveAll(final)
	if err = os.MkdirAll(tmp, 0o755); err != nil {
		return "", nil, err
	}
	for _, sc := range set {
		// Keep the registry path shape, but clamp it INTO the temp dir: force-absolute then
		// strip, which defeats ../ escapes without silently dropping the scenario.
		dst := filepath.Join(tmp, filepath.Clean("/"+sc.Path))
		if err = os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return "", nil, err
		}
		if err = os.WriteFile(dst, []byte(sc.Body), 0o644); err != nil {
			return "", nil, err
		}
	}
	if err = os.MkdirAll(root, 0o755); err != nil {
		return "", nil, err
	}
	if err = os.Rename(tmp, final); err != nil {
		return "", nil, err
	}
	// Remove the set afterwards: a leftover directory would be discovered as a "local" set by
	// the next run and would silently shadow the catalog.
	return final, func() { _ = os.RemoveAll(final) }, nil
}

// ── VR-F6 / INT-020: a direct local run SYNCS from the catalog first ─────────────────────────────
//
// UC059 ("the hybrid, half 1") specifies: edit a scenario in the registry → trigger a DIRECT local
// run → the in-env server asks for a FRESH materialization → the run executes the JUST-EDITED
// version. Step 3 was short-circuited whenever local files existed:
//
//	// 1. A non-empty local directory always wins — the compose fix-loop keeps working exactly
//	//    as before and never pays for a CP round-trip.
//	if len(scenario.DiscoverFiles(e.ScenariosDir)) > 0 { return e.ScenariosDir, sourceLocal, noop, nil }
//
// So a direct run could execute a DIFFERENT scenario from the one just authored, pass, and report a
// pass — for a version nobody was testing. The catalog is the authority on what the set IS; the
// directory is a materialised copy of it.
//
// THE COST THAT COMMENT WAS PROTECTING IS REAL, and is why the hash-only endpoint exists: consulting
// the catalog on every fix-loop iteration is only affordable if "nothing changed" is cheap. It now
// costs ~64 bytes, so the check stays on rather than becoming something people switch off.

// localSetHash computes the set hash of the LOCAL directory using the control plane's exact
// construction (store.SetHash / MaterializeSet): active scenarios ordered by path, each contributing
// len(path)\0path\0len(body)\0body\0 to a SHA-256.
//
// The paths must be in the CATALOG's shape — relative to the scenarios dir, forward slashes — since
// that is what the CP hashed. A mismatch here is not a correctness bug: the hashes simply never
// match, the set is re-fetched every time, and the run executes the catalog's truth. It fails toward
// "ask the authority", which is the right direction to fail in.
// ScenarioSetHash is localSetHash, EXPORTED so the agreement between this construction and the
// control plane's can be tested at the seam rather than assumed on both sides. Two hashers written
// separately drift, and the drift is silent: the fast path stops firing, every run re-downloads the
// set, and every run still passes.
func ScenarioSetHash(dir string) (string, bool) { return localSetHash(dir) }

func localSetHash(dir string) (string, bool) {
	files := scenario.DiscoverFiles(dir)
	if len(files) == 0 {
		return "", false
	}
	type entry struct{ path, body string }
	entries := make([]entry, 0, len(files))
	for _, f := range files {
		rel, err := filepath.Rel(dir, f.Path)
		if err != nil {
			return "", false
		}
		b, rerr := os.ReadFile(f.Path)
		if rerr != nil {
			return "", false
		}
		entries = append(entries, entry{path: filepath.ToSlash(rel), body: string(b)})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].path < entries[j].path })
	h := sha256.New()
	for _, e := range entries {
		h.Write([]byte(strconv.Itoa(len(e.path))))
		h.Write([]byte{0})
		h.Write([]byte(e.path))
		h.Write([]byte(strconv.Itoa(len(e.body))))
		h.Write([]byte{0})
		h.Write([]byte(e.body))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)), true
}
