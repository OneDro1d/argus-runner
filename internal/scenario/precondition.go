package scenario

import (
	"fmt"
	"regexp"
	"strings"
)

// THE PRECONDITION TIER: authoring shape.
//
// A check whose enforcement mechanism is absent cannot fail on the system's merits; it can only report the
// instrument. A scenario therefore DECLARES the preconditions it needs and which of them it DEPENDS ON:
//
//	- **Preconditions**: np-in=networkpolicy.ingress_enforced, np-out=networkpolicy.egress_enforced
//	- **Depends on**: np-in
//
// Both lines live in the scenario's own `## Metadata`, so they are part of the file's bytes: the set hash and
// the Merkle commitment (both over path + body) change when either changes, with no second hashing path to
// forget. A set "declares" its preconditions as the union over its files; two files declaring one id must
// agree on its kind (the executor refuses a set where they do not: internal/precondition.Collect).
//
// The KIND is a built-in from a closed list. There is no author-supplied probe code anywhere in this tier.

// Built-in precondition kinds, in the order the docs list them.
const (
	PreconditionIngressEnforced = "networkpolicy.ingress_enforced"
	PreconditionEgressEnforced  = "networkpolicy.egress_enforced"
)

// PreconditionKinds is the closed list of built-in kinds.
var PreconditionKinds = []string{PreconditionIngressEnforced, PreconditionEgressEnforced}

// PreconditionIDPattern is the shape of a precondition id.
const PreconditionIDPattern = `^[a-z0-9][a-z0-9_-]{0,62}$`

var preconditionIDRe = regexp.MustCompile(PreconditionIDPattern)

// Precondition is one declared precondition: an id the scenarios refer to and the built-in kind that probes it.
type Precondition struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
}

var (
	preconditionsRe = regexp.MustCompile(`\*\*Preconditions\*\*:[ \t]*(.*)`)
	dependsOnRe     = regexp.MustCompile(`\*\*Depends on\*\*:[ \t]*(.*)`)
)

// parsePreconditions reads the two metadata lines leniently (the parser never errors: Validate does). Entries
// that are not `id=kind` are kept with an empty Kind so the validator can name them.
func parsePreconditions(meta string) (decl []Precondition, depends []string, declared, dependsDeclared bool) {
	if m := preconditionsRe.FindStringSubmatch(meta); m != nil {
		declared = true
		for _, e := range strings.Split(m[1], ",") {
			e = strings.TrimSpace(e)
			if e == "" {
				continue
			}
			id, kind, _ := strings.Cut(e, "=")
			decl = append(decl, Precondition{ID: strings.TrimSpace(id), Kind: strings.TrimSpace(kind)})
		}
	}
	if m := dependsOnRe.FindStringSubmatch(meta); m != nil {
		dependsDeclared = true
		for _, e := range strings.Split(m[1], ",") {
			if e = strings.TrimSpace(e); e != "" {
				depends = append(depends, e)
			}
		}
	}
	return
}

// PreconditionKindKnown reports whether kind is a built-in.
func PreconditionKindKnown(kind string) bool { return in(kind, PreconditionKinds) }

// PreconditionProblems lists everything wrong with a scenario's precondition declarations, as sentences. The
// runner calls it too (the second door: a file can reach a run without passing the validator).
func PreconditionProblems(decl []Precondition, depends []string, declared, dependsDeclared bool) []string {
	var out []string
	if declared && len(decl) == 0 {
		out = append(out, "`**Preconditions**` is empty - list `id=kind` pairs (kinds: "+strings.Join(PreconditionKinds, ", ")+") or delete the line")
	}
	if dependsDeclared && len(depends) == 0 {
		out = append(out, "`**Depends on**` is empty - list precondition ids or delete the line")
	}
	seen := map[string]bool{}
	for _, p := range decl {
		switch {
		case !preconditionIDRe.MatchString(p.ID):
			out = append(out, fmt.Sprintf("precondition id %q must match %s", p.ID, PreconditionIDPattern))
		case seen[p.ID]:
			out = append(out, fmt.Sprintf("precondition %q is declared twice", p.ID))
		}
		seen[p.ID] = true
		switch {
		case p.Kind == "":
			out = append(out, fmt.Sprintf("precondition %q has no kind - write `%s=<kind>` (kinds: %s)", p.ID, p.ID, strings.Join(PreconditionKinds, ", ")))
		case !PreconditionKindKnown(p.Kind):
			msg := fmt.Sprintf("precondition %q has the unknown kind %q - the list is closed and built in: %s", p.ID, p.Kind, strings.Join(PreconditionKinds, ", "))
			if near := NearestName(p.Kind, PreconditionKinds); near != "" {
				msg += fmt.Sprintf(". Did you mean `%s`?", near)
			}
			out = append(out, msg)
		}
	}
	for _, d := range depends {
		if !seen[d] {
			out = append(out, fmt.Sprintf("`**Depends on**` names %q, which is not declared in this scenario's `**Preconditions**`", d))
		}
	}
	return out
}

// preconditionErrors is the validator's door for the two metadata lines.
func preconditionErrors(s *Scenario, text string, metaLine int) []Error {
	var errs []Error
	line := lineContaining(text, "**Preconditions**", lineContaining(text, "**Depends on**", metaLine))
	for _, p := range PreconditionProblems(s.Preconditions, s.DependsOn, s.PreconditionsDeclared, s.DependsOnDeclared) {
		errs = append(errs, Error{Line: line, Message: p + ""})
	}
	return errs
}
