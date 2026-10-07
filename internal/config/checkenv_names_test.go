package config

import (
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/OneDro1d/argus-runner/schemas"
)

// (amended): the loader and the JSON schema must give the SAME verdict on every name in
// `check_env`, and refuse a name that would change how the executor process itself runs.
//
// The schema is read by no runtime code (VR-TH1), so "agree" has to be proved here: this file reads the
// schema's `check_env.items` node and evaluates the subset of JSON Schema it uses (type, pattern, allOf,
// not) against the same list of names the loader sees.

type schemaNode struct {
	Type    string       `yaml:"type"`
	Pattern string       `yaml:"pattern"`
	AllOf   []schemaNode `yaml:"allOf"`
	Not     *schemaNode  `yaml:"not"`
}

func checkEnvItemsSchema(t *testing.T) schemaNode {
	t.Helper()
	var root struct {
		Properties map[string]yaml.Node `yaml:"properties"`
	}
	if err := yaml.Unmarshal(schemas.ArgusConfig, &root); err != nil {
		t.Fatalf("the argus-config schema does not parse: %v", err)
	}
	n, ok := root.Properties["check_env"]
	if !ok {
		t.Fatal("the schema has no top-level check_env property")
	}
	var ce struct {
		Items schemaNode `yaml:"items"`
	}
	if err := n.Decode(&ce); err != nil {
		t.Fatalf("check_env does not decode: %v", err)
	}
	return ce.Items
}

// accepts evaluates the node against a string.
func (n schemaNode) accepts(t *testing.T, s string) bool {
	t.Helper()
	if n.Pattern != "" && !regexp.MustCompile(n.Pattern).MatchString(s) {
		return false
	}
	for _, a := range n.AllOf {
		if !a.accepts(t, s) {
			return false
		}
	}
	if n.Not != nil && n.Not.accepts(t, s) {
		return false
	}
	return true
}

// schemaRule is what the schema's `not` branches say, reduced to the two shapes the loader's list has.
func schemaReserved(t *testing.T, items schemaNode) (names, prefixes []string) {
	t.Helper()
	exact := regexp.MustCompile(`^\^\(([A-Z0-9_|]+)\)\$$`)
	prefix := regexp.MustCompile(`^\^([A-Z0-9_]+_)$`)
	for _, a := range items.AllOf {
		if a.Not == nil {
			continue
		}
		p := a.Not.Pattern
		switch {
		case exact.MatchString(p):
			names = append(names, strings.Split(exact.FindStringSubmatch(p)[1], "|")...)
		case prefix.MatchString(p):
			prefixes = append(prefixes, prefix.FindStringSubmatch(p)[1])
		default:
			t.Errorf("a `not` pattern in the schema has a shape this test does not know: %q", p)
		}
	}
	sort.Strings(names)
	sort.Strings(prefixes)
	return names, prefixes
}

// the names the brief requires to be refused, whatever the lists say.
var mustRefuse = []string{
	"PATH", "HOME", "USER", "SHELL", "PWD", "HOSTNAME", "TMPDIR",
	"LD_PRELOAD", "LD_LIBRARY_PATH", "LD_AUDIT", "KUBERNETES_SERVICE_HOST", "KUBERNETES_PORT",
	"JAVA_TOOL_OPTIONS", "JAVA_HOME", "GODEBUG", "SSL_CERT_FILE", "SSL_CERT_DIR",
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY",
	"ARGUS_X", "ARGUS_RUNNER_TOKEN",
	// lowercase forms: the name pattern does not admit lowercase at all, so these stay refused by it
	"http_proxy", "https_proxy", "no_proxy", "path", "ld_preload",
	// not a name
	"", "1STARTS_WITH_DIGIT", "HAS SPACE", "A-B", "NAME=value", "${SOME_PASSWORD}",
}

// ordinary names that merely RESEMBLE a reserved one, which must stay accepted.
var mustAccept = []string{
	"SOME_PASSWORD", "OTHER_TOKEN", "_UNDERSCORE_FIRST", "PATHS", "HOMEDIR", "USERNAME", "SHELLY",
	"LDAP_PASSWORD", "LDX", "KUBERNETESX", "KUBE_TOKEN", "HTTP_PROXY_PASSWORD", "NO_PROXY_LIST", "JAVA_HOME_DIR",
}

func loaderVerdict(name string) (bool, string) {
	c := &Config{CheckEnv: []string{name}}
	err := c.validateCheckEnv()
	if err == nil {
		return true, ""
	}
	return false, err.Error()
}

func TestCheckEnv_LoaderAndSchemaAgreeOnEveryName(t *testing.T) {
	items := checkEnvItemsSchema(t)
	all := append(append([]string{}, mustRefuse...), mustAccept...)
	for _, n := range all {
		loader, _ := loaderVerdict(n)
		schema := items.accepts(t, n)
		if loader != schema {
			t.Errorf("name %q: the loader says accepted=%v, the schema says accepted=%v", n, loader, schema)
		}
	}
	for _, n := range mustRefuse {
		if ok, _ := loaderVerdict(n); ok {
			t.Errorf("name %q must be refused by the loader", n)
		}
		if items.accepts(t, n) {
			t.Errorf("name %q must be refused by the schema", n)
		}
	}
	for _, n := range mustAccept {
		if ok, why := loaderVerdict(n); !ok {
			t.Errorf("name %q is an ordinary name and must be accepted by the loader: %s", n, why)
		}
		if !items.accepts(t, n) {
			t.Errorf("name %q is an ordinary name and must be accepted by the schema", n)
		}
	}
}

// The list lives in ONE place (CheckEnvReservedNames / CheckEnvReservedPrefixes); the schema mirrors it and
// this test fails when the two drift.
func TestCheckEnv_ReservedListAndSchemaDoNotDrift(t *testing.T) {
	gotNames, gotPrefixes := schemaReserved(t, checkEnvItemsSchema(t))
	wantNames := append([]string{}, CheckEnvReservedNames()...)
	wantPrefixes := append([]string{}, CheckEnvReservedPrefixes()...)
	sort.Strings(wantNames)
	sort.Strings(wantPrefixes)
	if strings.Join(gotNames, ",") != strings.Join(wantNames, ",") {
		t.Errorf("reserved names drifted:\n schema: %v\n  go:    %v", gotNames, wantNames)
	}
	if strings.Join(gotPrefixes, ",") != strings.Join(wantPrefixes, ",") {
		t.Errorf("reserved prefixes drifted:\n schema: %v\n  go:    %v", gotPrefixes, wantPrefixes)
	}
}

// A refusal names the entry's position and the rule, never the entry.
func TestCheckEnv_ReservedRefusalNamesPositionAndRuleNeverTheEntry(t *testing.T) {
	for _, n := range mustRefuse {
		if n == "" || !regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`).MatchString(n) {
			continue
		}
		c := &Config{CheckEnv: []string{"SOME_PASSWORD", n}}
		err := c.validateCheckEnv()
		if err == nil {
			t.Errorf("%q accepted", n)
			continue
		}
		msg := err.Error()
		if !strings.Contains(msg, "check_env[1]") {
			t.Errorf("%q: the refusal must name the entry's position: %s", n, msg)
		}
		if strings.Contains(msg, n) {
			t.Errorf("%q: the refusal echoed the entry: %s", n, msg)
		}
		if !strings.Contains(msg, "reserved") {
			t.Errorf("%q: the refusal must name the rule: %s", n, msg)
		}
	}
}
