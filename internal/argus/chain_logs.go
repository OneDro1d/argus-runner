package argus

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/OneDro1d/argus-runner/internal/chain"
	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/mcp"
	"github.com/OneDro1d/argus-runner/internal/obsquery"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// logsPrepared is one logs step, resolved and checked, ready to be built into a chain.Step.
type logsPrepared struct {
	spec chain.LogsSpec
	want []mcp.BodyAssert
	poll *chain.Poll
}

// logsPreflight is the executor's door for logs steps (door B), run BEFORE any step of the chain is built, so a
// refusal leaves nothing fired and nothing half-assembled. It asks the SAME functions the authoring validator asks
// (scenario.LogsStepProblems: shape, scope, labels, the line filters, window, settle, poll; scenario.LogsStepClaims:
// only `body …` claims), then checks the environment the step needs: a Loki this executor reads, and that the
// instance's config does not select BetterStack.
//
// ⛔ The labels, `contains` and `matching` are read from the step AS AUTHORED (pristine, the unsubstituted payload),
// never from specs: parseChainSpec has already filled every environment value into specs, and an environment value
// must never be put into a log selector. The run's own id is the only thing placed in `contains` here, by
// fillCorrelationID; a ${saved.<var>} is bound by the step at run time.
//
// ⛔ A SCENARIO CANNOT NAME A URL: nothing here reads one. logs is the executor's own Loki (RunAllWithLogs), or nil.
//
// started is when the chain was assembled; the default window opens scenario.LogsLookBack before it.
// The returned map is keyed by the step's index. step and why name the first refusal ("" = fine).
func logsPreflight(c *config.Config, s *scenario.Scenario, specs, pristine []chainStepSpec, claims map[string][]string, corr string, fe faultEnv, started time.Time) (map[int]logsPrepared, string, string) {
	if !scenario.HasLogsStep(specs) {
		return nil, "", ""
	}
	raws := scenarioRawSteps(s)
	out := map[int]logsPrepared{}
	for i, st := range specs {
		if st.Type != "logs" {
			continue
		}
		if i >= len(pristine) {
			return nil, st.Name, "the step could not be read as authored"
		}
		p := pristine[i]
		var raw map[string]json.RawMessage
		if i < len(raws) {
			raw = raws[i]
		}
		if probs := scenario.LogsStepProblems(p, raw); len(probs) > 0 {
			return nil, st.Name, strings.Join(probs, "; ")
		}
		asserts, cerr := scenario.LogsStepClaims(claims[st.Name])
		if cerr != nil {
			return nil, st.Name, cerr.Error()
		}
		if len(asserts) == 0 {
			return nil, st.Name, "a logs step needs at least one `body …` claim on its count (e.g. `body has count >= 1`)"
		}
		if why := logsEnvironmentRefusal(c, fe); why != "" {
			return nil, st.Name, why
		}
		labels, _ := scenario.LogsStepLabels(p.Labels)
		window, _ := scenario.LogsStepWindow(p.Window)
		settle, _ := scenario.LogsStepSettle(p.Settle)
		var poll *chain.Poll
		if p.Poll != nil {
			pt, terr := time.ParseDuration(p.Poll.Timeout)
			if terr != nil {
				return nil, st.Name, "poll.timeout is not a valid duration: " + terr.Error()
			}
			iv, _ := scenario.LogsStepPollInterval(p.Poll)
			poll = &chain.Poll{Timeout: pt, Interval: iv}
		}
		out[i] = logsPrepared{
			spec: chain.LogsSpec{Scope: p.Scope, Labels: labels, Contains: fillCorrelationID(p.Contains, corr), Matching: p.Matching,
				Window: window, Settle: settle, ChainStart: started, Scrub: credentialScrub(c)},
			want: resolveBodyAssertsCorrelationID(asserts, corr),
			poll: poll,
		}
	}
	return out, "", ""
}

// SharedLogSourceFor is the shared-Loki log source of an instance: the cluster label, the SUT
// namespace and the SUT's declared log-field translation table, read the way the ingest-time promtail pipeline
// read it. Shared by the executor's Loki client (toolcore.lokiFor) and the saga-presence props (DeriveProps), so
// the two cannot select differently.
func SharedLogSourceFor(c *config.Config, cluster, namespace, instance string) *obsquery.SharedLogSource {
	return &obsquery.SharedLogSource{
		Cluster:          cluster,
		Namespace:        strings.TrimSpace(namespace),
		Constants:        map[string]string{"argus_instance": instance, "project": c.ProjectLabel()},
		Format:           c.LogFormat(),
		Regexp:           c.SharedLokiLogRegex(),
		LevelField:       c.LevelField(),
		CorrelationField: c.CorrelationField(),
		SagaField:        c.SagaEventField(),
		SagaMarkers:      c.SagaEventValues(),
	}
}

// logsEnvironmentRefusal says, by name, why this executor cannot read logs: the instance's config selects
// BetterStack (a logs step reads Loki only for now), or the executor has no Loki (--obs none, an empty --loki).
// BetterStack is asked first: a BetterStack instance has no Loki either, and "no Loki" would hide the real reason.
func logsEnvironmentRefusal(c *config.Config, fe faultEnv) string {
	if c != nil && c.UseBetterStack() {
		return "a logs step reads Loki only for now, and this instance's argus-config selects BetterStack " +
			"(observability.betterstack) - a logs step cannot read BetterStack yet"
	}
	if fe.Logs == nil {
		return "a logs step reads the Loki this executor is configured with, and this executor has none (onboarded with " +
			"--obs none, or --loki is empty) - the operator can onboard with --obs bundled, adopt, export (hosted Loki) or shared"
	}
	return ""
}
