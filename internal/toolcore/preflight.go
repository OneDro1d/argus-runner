package toolcore

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// PreflightRun answers, BEFORE a run starts, whether the run would execute anything at all.
// It returns nil when there is something to run, and a refusal explaining why not otherwise.
//
// Why this exists as a separate, synchronous check: runner__run is ASYNC. It mints a run_id,
// backgrounds the work and returns immediately, so an agent that asks for a run which can only
// ever execute nothing still gets a run_id back and has to infer the problem from a later
// report showing total:0. The owner's requirement is that the agent SAY the run cannot be run,
// which means the answer has to exist at call time.
//
// It deliberately performs no side effects: no materialized set, no results directory, nothing
// the subsequent real run could mistake for a local scenario set.
func PreflightRun(e Env, layer, tag, scenarioID string) error {
	local := scenario.DiscoverFiles(e.ScenariosDir)

	// A non-empty local set is what the run will use, so answer from it directly — no CP call,
	// keeping the compose fix-loop offline and instant.
	if len(local) > 0 {
		if matchesAny(local, layer, tag, scenarioID) {
			return nil
		}
		if sel := describeSelection(layer, tag, scenarioID); sel != "" {
			return fmt.Errorf("%w: cannot run — no scenario matches %s in the scenario set for instance %q (%d scenario(s) present), so there is nothing to run. Check the selection: a scenario id is not a tag, and a layer must be the canonical name (e.g. \"Rate Limiting\")",
				ErrNoScenarioSource, sel, e.instanceLabel(), len(local))
		}
		// Unfiltered over a non-empty set: there IS something to run.
		return nil
	}

	// No local set — the run would source from the catalog, so ask it the same question.
	if e.SetFetcher == nil {
		return fmt.Errorf("%w: cannot run — instance %q has no scenarios to run. There are none in %q and this executor is not wired to a control-plane catalog. Point --scenarios at your set, or wire the executor to a control plane",
			ErrNoScenarioSource, e.instanceLabel(), e.ScenariosDir)
	}
	ctx, cancel := context.WithTimeout(context.Background(), catalogCallTimeout)
	defer cancel()
	set, err := e.SetFetcher(ctx, layer, tag, scenarioID)
	if err != nil {
		return fmt.Errorf("%w: cannot run — no scenarios in %q for instance %q, and the control-plane catalog could not be reached to fetch them (%v)",
			ErrNoScenarioSource, e.ScenariosDir, e.instanceLabel(), err)
	}
	if len(set) == 0 {
		if sel := describeSelection(layer, tag, scenarioID); sel != "" {
			return fmt.Errorf("%w: cannot run — no scenario matches %s for instance %q, so there is nothing to run. Check the selection, or author/activate a matching scenario in the catalog",
				ErrNoScenarioSource, sel, e.instanceLabel())
		}
		return fmt.Errorf("%w: cannot run — instance %q has no scenarios to run. Its control-plane catalog reports 0 active scenarios and there is no local set in %q. Author or activate scenarios for this instance, then run again",
			ErrNoScenarioSource, e.instanceLabel(), e.ScenariosDir)
	}
	return nil
}

// matchesAny reports whether any discovered scenario satisfies the selection, using the SAME
// rules the runner applies — an id match is exact, a tag is membership, and a layer matches the
// scenario's terminal (run) layer. If these drifted from the runner, preflight would refuse runs
// that would actually have executed, which is worse than the gap it closes.
func matchesAny(found []scenario.Discovered, layer, tag, scenarioID string) bool {
	for _, d := range found {
		s := d.Scenario
		if scenarioID != "" && !strings.EqualFold(s.ID, scenarioID) {
			continue
		}
		if tag != "" && !hasTag(s.Tags, tag) {
			continue
		}
		if layer != "" && !matchesLayer(s.Layers, layer) {
			continue
		}
		return true
	}
	return false
}

func hasTag(tags []string, want string) bool {
	for _, t := range tags {
		if strings.EqualFold(strings.TrimSpace(t), want) {
			return true
		}
	}
	return false
}

// matchesLayer compares against the TERMINAL layer — the one the runner keys on for a
// multi-layer scenario ("HTTP Ingestion -> Database State" runs as Database State).
func matchesLayer(layers []string, want string) bool {
	if len(layers) == 0 {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(layers[len(layers)-1]), want)
}

// readDirNames is a small helper used by the tests to assert preflight left no artifacts.
func readDirNames(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out, nil
}
