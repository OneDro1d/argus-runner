package argus

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/OneDro1d/argus-runner/internal/amqpengine"
	"github.com/OneDro1d/argus-runner/internal/chain"
	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/mcp"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// dialBus opens the connection a bus step's subscriptions share. A package var ONLY so tests can put a
// fake broker behind the whole chain path; production never reassigns it.
var dialBus chain.BusDialer = chain.DialBus

// busPrepared is one bus step, resolved and checked, ready to be built into a chain.Step and armed.
type busPrepared struct {
	name string
	spec chain.BusSpec
	want []mcp.BodyAssert
	arm  chain.BusArm
}

// busRefusal says why a chain's bus steps cannot run. env is true for an ENVIRONMENT refusal (the harness is not set
// up for it): the scenario is then `error`, not `failed`, as signPreflight's are. A shape or claim refusal is `failed`
// through preflightChainFail, as the logs step's are.
type busRefusal struct {
	step, why string
	env       bool
	arming    bool // the refusal came from arming (a broker was dialled), not from the preflight
}

// busPreflight is the executor's door for bus steps (door B), run BEFORE any step of the chain is built, so a refusal
// leaves nothing dialled, nothing armed and nothing fired. It asks the SAME functions the authoring validator asks
// (scenario.BusStepProblems and BusStepPositionProblem: shape, binding, `where`, wait, position; scenario.BusStepClaims:
// only `body …` claims, C1 and the anchor C2), then checks the environment: the broker target, its envelope, its url
// reference and its vhost.
//
// ⛔ The step is read AS AUTHORED (pristine, the unsubstituted payload): parseChainSpec has already filled every
// environment value into specs, and an environment value must never be put into a binding or a `where`. The run's own id
// is the only thing placed in a `where` here, by fillCorrelationID; a ${saved.<var>} is bound by the step at run time.
//
// ⛔ THE URL NEVER PASSES THROUGH CODE: the step dials with the NAME of the variable the target's `url` references
// (config.MQTarget.BusURLEnv), taken from the declaration, never with the value.
//
// The returned map is keyed by the step's index.
func busPreflight(c *config.Config, s *scenario.Scenario, specs, pristine []chainStepSpec, claims map[string][]string, corr string) (map[int]busPrepared, *busRefusal) {
	if !scenario.HasBusStep(specs) {
		return nil, nil
	}
	raws := scenarioRawSteps(s)
	out := map[int]busPrepared{}
	for i, st := range specs {
		if st.Type != "bus" {
			continue
		}
		if i >= len(pristine) {
			return nil, &busRefusal{step: st.Name, why: "the step could not be read as authored"}
		}
		p := pristine[i]
		var raw map[string]json.RawMessage
		if i < len(raws) {
			raw = raws[i]
		}
		probs := scenario.BusStepProblems(p, raw)
		if why := scenario.BusStepPositionProblem(i); why != "" {
			probs = append(probs, why)
		}
		if len(probs) > 0 {
			return nil, &busRefusal{step: st.Name, why: strings.Join(probs, "; ")}
		}
		asserts, cerr := scenario.BusStepClaims(claims[st.Name])
		if cerr != nil {
			return nil, &busRefusal{step: st.Name, why: cerr.Error()}
		}
		if len(asserts) == 0 {
			return nil, &busRefusal{step: st.Name, why: "a bus step needs at least one `body …` claim (e.g. `body has count >= 1`)"}
		}
		t, label, terr := c.BusTargetFor(p.Target)
		if terr != nil {
			return nil, &busRefusal{step: st.Name, why: terr.Error(), env: true}
		}
		if why := t.BusPreflightProblem(label); why != "" {
			return nil, &busRefusal{step: st.Name, why: why, env: true}
		}
		urlEnv, _ := t.BusURLEnv()
		if v, isSet := os.LookupEnv(urlEnv); !isSet || v == "" {
			return nil, &busRefusal{step: st.Name, env: true,
				why: fmt.Sprintf("%s.url refers to the environment variable %s, which is not set in the executor", label, urlEnv)}
		}
		exchange, routingKey := p.Exchange, p.RoutingKey
		if p.Stream != "" {
			ss, serr := t.BusStreamNamed(label, p.Stream)
			if serr != nil {
				return nil, &busRefusal{step: st.Name, why: serr.Error(), env: true}
			}
			exchange, routingKey = ss.Exchange, ss.RoutingKey
		}
		where, _ := scenario.BusStepWhere(p.Where)
		for path, vals := range where {
			for j := range vals {
				vals[j] = fillCorrelationID(vals[j], corr)
			}
			where[path] = vals
		}
		wait, _ := scenario.BusStepWait(p.Wait)
		env := amqpengine.Envelope{ListPayload: t.Envelope.ListPayload}
		for _, src := range t.Envelope.Correlation {
			env.Correlation = append(env.Correlation, amqpengine.CorrelationSource{Property: src.Property, Header: src.Header, Body: src.Body})
		}
		out[i] = busPrepared{
			name: st.Name,
			spec: chain.BusSpec{
				Binding: fmt.Sprintf("exchange %q, routing_key %q", exchange, routingKey),
				Where:   where, Wait: wait, Scrub: credentialScrub(c),
			},
			want: resolveBodyAssertsCorrelationID(asserts, corr),
			arm: chain.BusArm{
				URLEnv: urlEnv, Queue: scenario.ListenQueueName(corr, i), Exchange: exchange, RoutingKey: routingKey,
				Opts: amqpengine.SubscribeOptions{
					Envelope: env, CorrelationID: corr, TTLMillis: scenario.ListenQueueTTLMillis,
					DecodedMax: scenario.AMQPDecodedMaxBytes, CorrelatedMax: scenario.BusCorrelatedMax, SeenMax: scenario.BusSeenMax,
				},
			},
		}
	}
	return out, nil
}

// busResult is the scenario result of a bus refusal.
func busResult(s *scenario.Scenario, corr string, r *busRefusal) report.ScenarioResult {
	if r.arming {
		return report.ScenarioResult{ID: s.ID, CorrelationID: corr, Status: report.StatusError,
			Failure: &report.Failure{Observed: "chain step '" + r.step + "': " + r.why + " - arming, nothing was sent"}}
	}
	if r.env {
		return report.ScenarioResult{ID: s.ID, CorrelationID: corr, Status: report.StatusError,
			Failure: &report.Failure{Observed: "chain step '" + r.step + "': " + r.why + " - preflight, nothing was dialled and nothing was sent"}}
	}
	return preflightChainFail(s, corr, r.step, r.why)
}

// armBus arms every bus step's subscription, in step order, and returns the taps by step index. ⛔ It runs after every
// preflight and the build loop, and BEFORE chain.Run: a message the system publishes after the broker confirmed the
// consumer is routed to the queue, so arming first is what makes "nothing arrived" mean nothing arrived. An arming failure
// is `error` and nothing was fired; whatever was armed before it is torn down by hub.Close (the caller defers it).
func armBus(hub *chain.BusHub, prepared map[int]busPrepared, n int) (map[int]chain.BusTap, *busRefusal) {
	taps := map[int]chain.BusTap{}
	for i := 0; i < n; i++ {
		p, ok := prepared[i]
		if !ok {
			continue
		}
		tap, err := hub.Arm(p.arm)
		if err != nil {
			return nil, &busRefusal{step: p.name, why: err.Error(), env: true, arming: true}
		}
		taps[i] = tap
	}
	return taps, nil
}

// chainHasBusStep reports whether the scenario is a chain carrying a bus step.
func chainHasBusStep(s *scenario.Scenario) bool {
	if !contains(s.Tags, ChainTag) {
		return false
	}
	steps, err := scenario.ParseChainSteps(s.Trigger.Payload)
	return err == nil && scenario.HasBusStep(steps)
}

// busNotRetriedNote is said in Observed when a rate-limited chain with a bus step is NOT re-fired (design 3.8): a retry
// reuses the correlation id, so a message the first attempt provoked late could be counted for the second.
const busNotRetriedNote = " - this chain has a bus step, so it was not re-fired after the rate limit (a re-fire reuses the correlation id, and a " +
	"late message from the first attempt would be counted for the second)"

// noteBusNotRetried appends busNotRetriedNote to the result's Observed.
func noteBusNotRetried(res *report.ScenarioResult) {
	if res.Failure == nil {
		res.Failure = &report.Failure{Observed: "the chain was rate limited" + busNotRetriedNote}
		return
	}
	res.Failure.Observed += busNotRetriedNote
}
