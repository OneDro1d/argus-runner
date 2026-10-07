package config

import (
	"fmt"

	"gopkg.in/yaml.v3"

	"github.com/OneDro1d/argus-runner/internal/scenario"
	"github.com/OneDro1d/argus-runner/internal/testtargets"
)

// neverload.go -- `load_test: never` on a test_targets[] entry.
//
// A load check is an AMQP Load check or ANY check that carries a `## LOAD` section. It belongs to the test
// target testtargets.List.Map gives it (the ONE mapping). When that target declares `load_test: never`:
//   - validate-config reports an error (neverLoadValidateMessage);
//   - the executor refuses to fire it, before anything is dialled (NeverLoadRefusal, used by argus.runOneScenario).
// Absent key = today's behaviour. The text never echoes a URL, host or credential.

// strictLoadTestValues refuses a `load_test` key of a test_targets entry whose value is not exactly the plain
// word `never`: empty, null, quoted-empty, another word, a list or a map. An ABSENT key is fine. It reads the
// document's nodes because the typed decode cannot tell an empty value from an absent key, and a prohibition
// its author typed and left blank must not read as "no prohibition". A document that does not parse is left
// to the typed decode, which says why.
func strictLoadTestValues(b []byte) error {
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil
	}
	root := doc.Content[0]
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value != "test_targets" || root.Content[i+1].Kind != yaml.SequenceNode {
			continue
		}
		for n, entry := range root.Content[i+1].Content {
			if entry.Kind != yaml.MappingNode {
				continue
			}
			name := ""
			var val *yaml.Node
			for k := 0; k+1 < len(entry.Content); k += 2 {
				switch entry.Content[k].Value {
				case "name":
					name = entry.Content[k+1].Value
				case "load_test":
					val = entry.Content[k+1]
				}
			}
			if val == nil {
				continue
			}
			if val.Kind == yaml.ScalarNode && val.Tag == "!!str" && val.Value == testtargets.LoadTestNever {
				continue
			}
			shown := "an empty value"
			switch {
			case val.Kind == yaml.SequenceNode:
				shown = "a list"
			case val.Kind == yaml.MappingNode:
				shown = "a map"
			case val.Kind == yaml.ScalarNode && val.Value != "" && val.Tag != "!!null":
				shown = fmt.Sprintf("%q", val.Value)
			}
			return fmt.Errorf("line %d: test_targets[%d] (%s): load_test %s is not accepted — the only value is %q (or leave the key out)",
				val.Line, n, name, shown, testtargets.LoadTestNever)
		}
	}
	return nil
}

// isLoadCheck: an AMQP Load check, or any check with a `## LOAD` section (declared, valid or not).
func isLoadCheck(s *scenario.Scenario) bool {
	return s != nil && (s.LoadDeclared || scenario.PrimaryLayer(s) == scenario.AMQPLoadLayer)
}

// neverLoadTarget returns the name of the never-load target the check belongs to, "" when it belongs to none.
func (c *Config) neverLoadTarget(s *scenario.Scenario) string {
	if !isLoadCheck(s) || len(c.TestTargets) == 0 {
		return ""
	}
	t, ok := c.TestTargets.ByName(c.TestTargets.Map(s.ID, s.Tags))
	if !ok || !t.NeverLoadTested() {
		return ""
	}
	return t.Name
}

// NeverLoadRefusal says why a load check may not run because its test target declares `load_test: never`;
// "" = not refused. It is the Observed line of the check's result.
func (c *Config) NeverLoadRefusal(s *scenario.Scenario) string {
	name := c.neverLoadTarget(s)
	if name == "" {
		return ""
	}
	return fmt.Sprintf("refused before firing: this is a load check and it belongs to test target %q, which declares load_test: never in test_targets of argus-config.yaml. "+
		"That target must never be put under load. Nothing was sent. - preflight", name)
}

// neverLoadValidateMessage is the authoring-time wording (validate-config); file is the scenario file.
func (c *Config) neverLoadValidateMessage(s *scenario.Scenario, file string) string {
	name := c.neverLoadTarget(s)
	if name == "" {
		return ""
	}
	return fmt.Sprintf("scenario file %s is a load check and belongs to test target %q, which declares load_test: never in test_targets. "+
		"Move the check to a target that may be loaded, or remove the load section", file, name)
}
