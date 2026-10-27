package argus

import (
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// TestMoneyGuard_JudgedKindEqualsDispatchedKind_EveryTagAndLayerCombination: for EVERY subset of
// {mcp, ui, chain} as tags × {a plain layer, HTTP Load, AMQP Load}: the runner kind the money guard judged
// the scenario as must equal the kind the real dispatch (runOneScenario -> runnerChosen) hands it to, and both
// must equal the table's own oracle (the dispatch order of record: AMQP Load layer, HTTP Load layer, chain, mcp,
// ui, else plain).
//
// A load layer with an engine tag never reaches the dispatch: the target door that runs before it reads the tags
// (SelectTarget) and refuses the file ("dispatched" is false for those rows, and nothing is sent). The guard still
// judges them, as the load runner they would get, at the doors that have no target door (authoring, validate-config).
func TestMoneyGuard_JudgedKindEqualsDispatchedKind_EveryTagAndLayerCombination(t *testing.T) {
	tagSets := [][]string{{}, {"mcp"}, {"ui"}, {"chain"}, {"mcp", "ui"}, {"mcp", "chain"}, {"ui", "chain"}, {"mcp", "ui", "chain"}}
	layers := []struct{ layer, target string }{{"Permissions", ""}, {"HTTP Load", "api-lab"}, {"AMQP Load", "load-lab"}}

	oracle := func(layer string, tags []string) scenario.RunnerKind {
		has := func(x string) bool {
			for _, v := range tags {
				if v == x {
					return true
				}
			}
			return false
		}
		switch {
		case layer == "AMQP Load":
			return scenario.RunnerAMQPLoad
		case layer == "HTTP Load":
			return scenario.RunnerHTTPLoad
		case has("chain"):
			return scenario.RunnerChain
		case has("mcp"):
			return scenario.RunnerMCP
		case has("ui"):
			return scenario.RunnerUI
		}
		return scenario.RunnerPlain
	}

	rows := 0
	for _, ts := range tagSets {
		for _, ly := range layers {
			tags := strings.Join(ts, ", ")
			if tags == "" {
				tags = "http"
			}
			name := ly.layer + " | tags: " + tags
			e := newMixedEnv(t) // money_handling is switched off below: this test observes the DISPATCH
			e.cfg.MoneyHandling = false
			specs := fakeUIRun(t)
			_ = specs
			load := ""
			expect := "### Runnable\n- status=200\n- step s: status=200\n"
			switch ly.layer {
			case "HTTP Load":
				load, expect = mixedHTTPLoad, "### Runnable\n- every step is measured\n"
			case "AMQP Load":
				load, expect = mixedAMQPLoad, "### Runnable\n- every step is measured\n"
			}
			md := mixedMD(ly.layer, tags, ly.target, "GET `/health`", chainGetPayload(e.srv.URL), expect, load)
			s := scenario.Parse(md)

			guardKind, _ := scenario.MoneyGuardJudgement(s, nil)

			var chosen []scenario.RunnerKind
			orig := runnerChosen
			runnerChosen = func(k scenario.RunnerKind) { chosen = append(chosen, k) }
			row := runOneScenario(e.cfg, scenarioFile{s: s}, newScenarioCorrelationID("20261002T120000000", "MX-001"), t.TempDir(), e.loadR,
				e.cfg.MoneyWriteAllowlist(), &scenario.MoneySpendLedger{}, "", nil)
			runnerChosen = orig

			want := oracle(ly.layer, ts)
			dispatched := "no (refused at a door before the dispatch)"
			if len(chosen) == 1 {
				dispatched = string(chosen[0])
			}
			t.Logf("%-34s guard judged %-9s | dispatch ran %-44s | oracle %s", name, guardKind, dispatched, want)
			rows++

			if guardKind != want {
				t.Errorf("%s: the guard judged %q, the dispatch order of record says %q", name, guardKind, want)
			}
			if len(chosen) > 1 {
				t.Errorf("%s: the dispatch chose %d times: %v", name, len(chosen), chosen)
			}
			if len(chosen) == 1 && chosen[0] != guardKind {
				t.Errorf("%s: the guard judged %q but the dispatch ran %q", name, guardKind, chosen[0])
			}
			if len(chosen) == 0 {
				// only a load layer carrying an engine tag may be stopped before the dispatch, and it sends nothing
				if (ly.layer == "Permissions") || len(ts) == 0 {
					t.Errorf("%s: the dispatch was never reached: %+v", name, row.Failure)
				}
				if len(e.loadR.calls) != 0 || len(e.hitList()) != 0 {
					t.Errorf("%s: a file stopped before the dispatch still fired: load calls %d, hits %v", name, len(e.loadR.calls), e.hitList())
				}
			}
		}
	}
	if rows != 24 {
		t.Fatalf("the table ran %d rows, want 24", rows)
	}
}
