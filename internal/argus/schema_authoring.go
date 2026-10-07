package argus

import (
	"fmt"

	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// CheckScenarioSchemas is the STANDALONE authoring-time half of
// ARGUS-DESIGN-GENERIC-SCHEMA-MESSAGES-2026-09-28 §8: "argus validate-scenario --config
// argus-config.yaml run locally with the app's config, so they can check the record against the
// declared schema fully." It walks s's chain steps' PRISTINE (never textually substituted)
// `record`s and, for every `schema`+`record` publish, runs EXACTLY the check
// runChainScenarioWithMoneyWrites applies on every run (amqpStepSpec ->
// buildAMQPRecordTemplate(schema, record, required=true) -> avroschema.BuildNative in Authoring
// mode) — but WITHOUT resolving a single placeholder or dialing a broker, so a bad record is
// refused BY FIELD PATH before anything runs (design: "the tester should run validate-scenario
// --config locally before writing").
//
// Only a tag:chain scenario is checked — schema+record only exists inside a chain's amqp step;
// every other scenario (and a nil config, i.e. no --config given) returns no errors, which is what
// keeps `argus validate-scenario` WITHOUT --config byte-for-byte unchanged (design: "without
// --config, behaviour stays what it is now").
func CheckScenarioSchemas(s *scenario.Scenario, c *config.Config) []string {
	if s == nil || c == nil || !contains(s.Tags, ChainTag) {
		return nil
	}
	steps, err := scenario.ParseChainSteps(s.Trigger.Payload)
	if err != nil {
		// The ordinary chain-shape validator (scenario.Validate, run unconditionally above this)
		// already reports a payload parse error in its own voice; reporting it again here would be
		// the SAME defect told twice.
		return nil
	}
	var errs []string
	for _, st := range steps {
		if st.Type != "amqp" || st.Op != "publish" || st.Schema == "" {
			continue
		}
		schema, serr := c.MessageSchema(st.Schema)
		if serr != nil {
			errs = append(errs, fmt.Sprintf("step %q: %s", st.Name, serr.Error()))
			continue
		}
		if _, why := buildAMQPRecordTemplate(schema, st.Record, true); why != "" {
			errs = append(errs, fmt.Sprintf("step %q: %s", st.Name, why))
		}
	}
	return errs
}
