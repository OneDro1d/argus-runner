package scenario

import (
	"fmt"
	"sort"

	"gopkg.in/yaml.v3"

	"github.com/OneDro1d/argus-runner/schemas"
)

// VR12-S1 / VR12-S2 (V30-003) — THE SCHEMA IS THE SINGLE SOURCE OF TRUTH, AND THE VALIDATOR LOADS IT.
//
// The closed list of markdown SECTIONS was written down in no machine-readable place whatsoever.
// The schema LOOKED like that place — `additionalProperties: false` at four levels — but it
// constrains the PARSED OBJECT, and an unknown `## Notes` heading never becomes a property at all,
// so it could never be caught there. Meanwhile NOTHING HAD EVER LOADED THE SCHEMA
// (`grep -rn "scenario.schema"` over the tree returned zero hits), which is the only reason its
// drift went unnoticed: it was wrong in four separate ways.
//
// ⛔ THE CONSEQUENCE THAT MAKES THIS A DEFECT AND NOT TIDINESS: A TYPO IN A DEFINED NAME DELETES A
// SECTION IN SILENCE. `## TIMEOUT` mistyped ⇒ falls back to 10s. `## CLEANUP` mistyped ⇒ a scenario
// that promised to clean up simply does not. `## VERIFY`, `## References` ⇒ silent.
//
// ⛔ WHAT IS LOADED, AND WHAT IS NOT — say the boundary out loud rather than let a reader assume.
// This file reads the schema's `x-markdown` block: the section list, the EXPECT sub-section list,
// the Metadata key list, the dispatch tags and the METHOD enum. Those are the lists the VALIDATOR
// needs, and reading them costs NO new dependency (yaml.v3 is already here).
//
// It does NOT perform JSON-Schema validation of the parsed object — that would need a schema
// library, which is the one new dependency the requirement budgets and which buys much less: the
// parsed-object half is already covered by Go's own types plus the drift test in
// schema_truth_test.go. If full validation is wanted later, this is the seam.

// schemaYAML is the embedded contract — see package schemas for why it lives there.
var schemaYAML = schemas.Scenario

type schemaSection struct {
	Name     string `yaml:"name"`
	Required bool   `yaml:"required"`
}

type schemaMarkdown struct {
	Sections          []schemaSection `yaml:"sections"`
	ExpectSubsections []string        `yaml:"expect_subsections"`
	MetadataKeys      []schemaSection `yaml:"metadata_keys"`
	DispatchTags      []string        `yaml:"dispatch_tags"`
	Methods           []string        `yaml:"methods"`
}

// Schema is the loaded contract. It is loaded ONCE, at init, and a schema that will not load is a
// PANIC rather than a silent fallback: a validator running on a default list it invented would be
// the same class of defect as the one this rule closes.
var Schema = mustLoadSchema()

func mustLoadSchema() schemaMarkdown {
	var doc struct {
		Markdown schemaMarkdown `yaml:"x-markdown"`
	}
	if err := yaml.Unmarshal(schemaYAML, &doc); err != nil {
		panic(fmt.Sprintf("scenario.schema.yaml does not parse: %v", err))
	}
	m := doc.Markdown
	switch {
	case len(m.Sections) == 0:
		panic("scenario.schema.yaml: x-markdown.sections is empty — the validator would police nothing")
	case len(m.MetadataKeys) == 0:
		panic("scenario.schema.yaml: x-markdown.metadata_keys is empty")
	case len(m.DispatchTags) == 0:
		panic("scenario.schema.yaml: x-markdown.dispatch_tags is empty")
	case len(m.Methods) == 0:
		panic("scenario.schema.yaml: x-markdown.methods is empty")
	}
	return m
}

// SectionNames returns the closed list of `## ` names, in declaration order.
func SectionNames() []string { return names(Schema.Sections) }

// RequiredSections returns the `## ` names a scenario must carry.
//
// ⚠ `VERIFY` is CONDITIONAL and is therefore NOT here: VR12-VF1 refuses it on mcp/chain/ui and
// VR12-VF2 requires it with a runnable query on Database State. A flat "required" flag cannot say
// that, so the schema marks it optional and the conditional rules decide.
func RequiredSections() []string {
	var out []string
	for _, s := range Schema.Sections {
		if s.Required {
			out = append(out, s.Name)
		}
	}
	return out
}

// MetadataKeys returns the closed `**Key**` list.
func MetadataKeys() []string { return names(Schema.MetadataKeys) }

// DispatchTags returns the four tags, exactly one of which a scenario must carry.
func DispatchTags() []string { return append([]string(nil), Schema.DispatchTags...) }

// Methods returns the METHOD verbs a TRIGGER line may use.
func Methods() []string { return append([]string(nil), Schema.Methods...) }

// KnownSection reports whether a `## ` heading name is one of the defined sections (SectionNames).
func KnownSection(name string) bool { return in(name, SectionNames()) }

// NearestName returns the defined name closest to `got`, or "" when nothing is close. It exists so
// a refusal can say `## TIEMOUT` → "did you mean TIMEOUT?", which is the whole value of the rule:
// the defect it closes is a TYPO silently deleting a section.
func NearestName(got string, candidates []string) string {
	best, bestD := "", 0
	for _, c := range candidates {
		d := editDistance(fold(got), fold(c))
		// Only offer a suggestion that is genuinely close: at most a third of the name's length,
		// and never more than 3 edits. A wrong suggestion is worse than none.
		limit := len(c)/3 + 1
		if limit > 3 {
			limit = 3
		}
		if d <= limit && (best == "" || d < bestD) {
			best, bestD = c, d
		}
	}
	return best
}

func names(ss []schemaSection) []string {
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		out = append(out, s.Name)
	}
	return out
}

func in(v string, set []string) bool {
	for _, s := range set {
		if s == v {
			return true
		}
	}
	return false
}

func fold(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}

// editDistance is Levenshtein — small inputs (a heading name), so the simple two-row form.
func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min3(cur[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

func min3(a, b, c int) int {
	if b < a {
		a = b
	}
	if c < a {
		a = c
	}
	return a
}

// sortedCopy is used by the drift test's messages.
func sortedCopy(ss []string) []string {
	out := append([]string(nil), ss...)
	sort.Strings(out)
	return out
}
