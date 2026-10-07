package config

import (
	"strings"
	"testing"
)

// What `outside_cluster:` accepts and refuses, as the loader actually behaves. The runbook
// describes this; TestTestTargetsRunbook_DescribesTheAcceptedValuesTruthfully keeps the two from drifting.
func TestTestTargets_OutsideClusterValues(t *testing.T) {
	const site = "  - name: site\n"
	const match = "    match: { tags: [site] }\n"
	accepted := map[string]struct {
		body    string
		outside bool
		ns      string
	}{
		"true":                  {site + "    outside_cluster: true\n" + match, true, ""},
		"yes is a YAML boolean": {site + "    outside_cluster: yes\n" + match, true, ""},
		"null is absent":        {site + "    outside_cluster:\n" + match, false, ""},
		"false keeps namespace": {site + "    outside_cluster: false\n    namespace: foo\n" + match, false, "foo"},
		"empty namespace":       {site + "    outside_cluster: true\n    namespace: \"\"\n" + match, true, ""},
	}
	for name, c := range accepted {
		cfg, err := Load(writeTTConfig(t, ttBase+"test_targets:\n"+c.body))
		if err != nil {
			t.Errorf("%s: refused: %v", name, err)
			continue
		}
		if got := cfg.TestTargets[0]; got.OutsideCluster != c.outside || got.Namespace != c.ns {
			t.Errorf("%s: OutsideCluster=%v Namespace=%q, want %v %q", name, got.OutsideCluster, got.Namespace, c.outside, c.ns)
		}
	}
	refused := map[string]struct {
		body string
		want string // a substring the refusal must carry
	}{
		// A quoted string is not a YAML boolean. yaml.v3 refuses it with a line number and the value, but does not
		// name the key; the loader has no existing way to add the key to a type error, so it is left as it is.
		"quoted true":          {site + "    outside_cluster: \"true\"\n" + match, "cannot unmarshal !!str `true` into bool"},
		"with namespace":       {site + "    outside_cluster: true\n    namespace: foo\n" + match, "outside_cluster: true and namespace"},
		"whitespace namespace": {site + "    outside_cluster: true\n    namespace: \"  \"\n" + match, "outside_cluster: true and namespace"},
		"misspelt key":         {site + "    outsideCluster: true\n" + match, `unknown key "outsideCluster"`},
	}
	for name, c := range refused {
		_, err := Load(writeTTConfig(t, ttBase+"test_targets:\n"+c.body))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want a refusal containing %q", name, err, c.want)
		}
	}
}
