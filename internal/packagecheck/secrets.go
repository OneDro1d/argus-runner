package packagecheck

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// clauseSecretSchema names the check the CLI and the report agree on.
const clauseSecretSchema = "secret schema"

// placeholderMarker is what a documented-but-not-real secret value looks like in this repo
// (gen-secrets.sh fills the real Secret out-of-band); anything else is a value-looking string.
const placeholderMarker = "REPLACE_ME"

// secretSchemaClause requires every key named by the declared secrets example's stringData to be
// declared in the declared env schema, and requires the value of every key declared secret:true to
// be the documented placeholder in the tracked file, never a real-looking secret. A key declared
// secret:false (a username beside its password) may carry its value. The conventional source of
// truth is k8s/base/cp-secrets.example.yaml, the concrete artifact AC-9 names; a deployment whose
// Secret example lives elsewhere declares it as package.secrets_example.
func secretSchemaClause(root string, d Declaration) Clause {
	if d.SecretsExample == "" {
		return fail(clauseSecretSchema, []Finding{{Location: "secrets_example:0", Reason: "no secrets example declared (package.secrets_example)"}})
	}
	raw, err := os.ReadFile(under(root, d.SecretsExample))
	if os.IsNotExist(err) {
		return fail(clauseSecretSchema, []Finding{{
			Location: d.SecretsExample + ":0",
			Reason:   "no secrets manifest at " + d.SecretsExample + " to check against the schema" + d.where("secrets_example"),
		}})
	}
	if err != nil {
		return fail(clauseSecretSchema, []Finding{{Location: d.SecretsExample + ":0", Reason: err.Error()}})
	}

	schema, err := loadEnvSchema(root, d.EnvSchema)
	if err != nil {
		return fail(clauseSecretSchema, []Finding{{Location: d.EnvSchema + ":0", Reason: err.Error()}})
	}

	entries, err := stringDataEntries(raw)
	if err != nil {
		return fail(clauseSecretSchema, []Finding{{Location: d.SecretsExample + ":0", Reason: err.Error()}})
	}

	var findings []Finding
	for _, e := range entries {
		loc := findingLocation(d.SecretsExample, e.line)
		decl, declared := schema[e.key]
		switch {
		case !declared:
			findings = append(findings, Finding{Location: loc, Reason: fmt.Sprintf("secret %s is not declared in %s", e.key, d.EnvSchema)})
		case !decl.Secret:
			// A key the schema declares as NOT secret (a username kept beside its password in the
			// same Secret) may carry its real value in the tracked example; only a secret must not.
			continue
		}
		if !isPlaceholder(e.value) {
			findings = append(findings, Finding{Location: loc, Reason: fmt.Sprintf("secret %s has a value-looking string in a tracked file, not the %s placeholder", e.key, placeholderMarker)})
		}
	}
	return fail(clauseSecretSchema, findings)
}

func isPlaceholder(value string) bool {
	return strings.Contains(value, placeholderMarker)
}

type secretEntry struct {
	key, value string
	line       int
}

// stringDataEntries reads every key/value of the first "stringData:" mapping found in raw.
func stringDataEntries(raw []byte) ([]secretEntry, error) {
	var node yaml.Node
	if err := yaml.Unmarshal(raw, &node); err != nil {
		return nil, err
	}
	var out []secretEntry
	findStringData(&node, &out)
	return out, nil
}

func findStringData(n *yaml.Node, out *[]secretEntry) {
	switch n.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, c := range n.Content {
			findStringData(c, out)
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			key, val := n.Content[i], n.Content[i+1]
			if key.Value == "stringData" && val.Kind == yaml.MappingNode {
				for j := 0; j+1 < len(val.Content); j += 2 {
					k, v := val.Content[j], val.Content[j+1]
					*out = append(*out, secretEntry{key: k.Value, value: v.Value, line: k.Line})
				}
				continue
			}
			findStringData(val, out)
		}
	}
}
