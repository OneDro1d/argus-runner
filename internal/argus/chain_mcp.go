package argus

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/OneDro1d/argus-runner/internal/chain"
	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// mcpPreflight is the executor's door for the new parts of an mcp step (door B), run BEFORE any step
// of the chain is built, so a refusal leaves nothing fired. It asks the SAME function the authoring validator asks
// (scenario.MCPStepProblems), then resolves the step's options.
//
// The patterns and the poll are read from the step AS AUTHORED (pristine, the unsubstituted payload): parseChainSpec
// has already filled every environment value into specs, and an environment value must never be spliced into a
// pattern. The returned map is keyed by the step's index. step and why name the first refusal ("" = fine).
func mcpPreflight(c *config.Config, s *scenario.Scenario, specs, pristine []chainStepSpec, claims map[string][]string) (map[int]chain.MCPOptions, string, string) {
	raws := scenarioRawSteps(s)
	out := map[int]chain.MCPOptions{}
	for i, st := range specs {
		if st.Type != "mcp" {
			continue
		}
		p := st
		if i < len(pristine) {
			p = pristine[i]
		}
		var raw map[string]json.RawMessage
		if i < len(raws) {
			raw = raws[i]
		}
		if probs := scenario.MCPStepProblems(p, raw, claims[st.Name]); len(probs) > 0 {
			return nil, st.Name, strings.Join(probs, "; ")
		}
		opt := chain.MCPOptions{Idempotent: p.Idempotent, Scrub: credentialScrub(c)}
		if p.Extract != nil {
			opt.Extract = p.Extract.Regex
			opt.ExtractPath = p.Extract.Path
		}
		if p.Unavailable != nil {
			opt.Unavailable = p.Unavailable.Matching
		}
		if p.Poll != nil {
			pt, terr := time.ParseDuration(p.Poll.Timeout)
			if terr != nil {
				return nil, st.Name, "poll.timeout is not a valid duration: " + terr.Error()
			}
			opt.Poll = &chain.Poll{Timeout: pt, Interval: scenario.MCPStepPollInterval(p.Poll)}
		}
		out[i] = opt
	}
	return out, "", ""
}

// signPreflight resolves every `sign` block of the chain's mcp steps BEFORE the first step is built or sent
// the key NAME must be declared under check_env, the executor's environment must hold a value
// under it, and the value must be an Ed25519 key. Any of these is the harness's problem - the caller turns the
// answer into an `error` (grey) scenario, never a `failed` one. The sentences name the reference and never the value.
//
// The structure of the block (door B for a malformed one) was already judged by mcpPreflight; the key NAME is read
// from the step AS AUTHORED (pristine) so an environment value is never spliced into it, and the rest of the block
// from the resolved step, so ${cid} and ${saved.…} in a field behave as they do in the step's args.
func signPreflight(c *config.Config, specs, pristine []chainStepSpec) (map[int]*chain.SignPlan, string, string) {
	out := map[int]*chain.SignPlan{}
	var declared map[string]bool
	for i, st := range specs {
		if st.Type != "mcp" || st.Sign == nil {
			continue
		}
		if declared == nil {
			declared = map[string]bool{}
			if c != nil {
				for _, n := range c.CheckEnvNames() {
					declared[n] = true
				}
			}
		}
		spec := *st.Sign
		if i < len(pristine) && pristine[i].Sign != nil {
			spec.Key = pristine[i].Sign.Key
		}
		if !declared[spec.Key] {
			return nil, st.Name, fmt.Sprintf("signing key reference %q is not declared under check_env in argus-config.yaml, so the executor was never given it - declare the name there (it is delivered to the executor like any check secret)", spec.Key)
		}
		val, ok := os.LookupEnv(spec.Key)
		if !ok || strings.TrimSpace(val) == "" {
			return nil, st.Name, fmt.Sprintf("signing key reference %q is declared but the executor's environment holds no value under it", spec.Key)
		}
		key, err := chain.ParseEd25519Key(val)
		if err != nil {
			return nil, st.Name, fmt.Sprintf("signing key reference %q cannot be used: %s", spec.Key, err.Error())
		}
		out[i] = &chain.SignPlan{Ref: spec.Key, Key: key, Spec: spec}
	}
	return out, "", ""
}

// CheckSigningKeys is the config-aware half of the validator for `sign`: every key reference a
// chain mcp step names must be a name declared under check_env. The config-free half (the shape of the block) is
// scenario.MCPSignProblems. A nil config reports nothing, so `argus validate-scenario` without --config is unchanged.
func CheckSigningKeys(s *scenario.Scenario, c *config.Config) []string {
	if s == nil || c == nil || !contains(s.Tags, ChainTag) {
		return nil
	}
	steps, err := scenario.ParseChainSteps(s.Trigger.Payload)
	if err != nil {
		return nil
	}
	declared := map[string]bool{}
	for _, n := range c.CheckEnvNames() {
		declared[n] = true
	}
	var errs []string
	for _, st := range steps {
		if st.Type != "mcp" || st.Sign == nil || st.Sign.Key == "" || declared[st.Sign.Key] {
			continue
		}
		errs = append(errs, fmt.Sprintf("step %q: signing key reference %q is not declared under check_env in argus-config.yaml", st.Name, st.Sign.Key))
	}
	return errs
}
