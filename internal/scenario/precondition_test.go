package scenario

import (
	"strings"
	"testing"
)

// The precondition tier's authoring rules. A check declares the preconditions it needs
// (`**Preconditions**`) and which of them it depends on (`**Depends on**`); the kind is a BUILT-IN from a
// closed list, never author code.

// preconditionScenario is a minimal valid http scenario with the two metadata lines spliced in.
func preconditionScenario(extraMeta string) string {
	return `# T1

## Metadata
- **ID**: PRECOND-001
- **Layer**: HTTP Ingestion
- **Tags**: http
` + extraMeta + `
## TRIGGER
GET ` + "`${APP_URL}/health`" + `

## EXPECT
### Runnable
- status=200
### Non-runnable
- none

## TIMEOUT
10s

## CLEANUP
N/A - read-only
`
}

func problemsOf(errs []Error) string {
	var b strings.Builder
	for _, e := range errs {
		b.WriteString(e.Message)
		b.WriteString("\n")
	}
	return b.String()
}

func TestPrecondition_ParsesDeclarationAndDependsOn(t *testing.T) {
	s := Parse(preconditionScenario("- **Preconditions**: np-in=networkpolicy.ingress_enforced, np-out=networkpolicy.egress_enforced\n- **Depends on**: np-in\n"))
	if len(s.Preconditions) != 2 || s.Preconditions[0].ID != "np-in" || s.Preconditions[0].Kind != PreconditionIngressEnforced ||
		s.Preconditions[1].ID != "np-out" || s.Preconditions[1].Kind != PreconditionEgressEnforced {
		t.Fatalf("preconditions = %+v", s.Preconditions)
	}
	if len(s.DependsOn) != 1 || s.DependsOn[0] != "np-in" {
		t.Fatalf("depends_on = %v", s.DependsOn)
	}
}

func TestPrecondition_ValidScenarioHasNoErrors(t *testing.T) {
	_, errs := Validate(preconditionScenario("- **Preconditions**: np-in=networkpolicy.ingress_enforced\n- **Depends on**: np-in\n"))
	if len(errs) != 0 {
		t.Fatalf("a valid precondition scenario was refused: %s", problemsOf(errs))
	}
}

func TestPrecondition_UnknownKindIsRefused(t *testing.T) {
	_, errs := Validate(preconditionScenario("- **Preconditions**: np-in=networkpolicy.ingres_enforced\n- **Depends on**: np-in\n"))
	got := problemsOf(errs)
	if !strings.Contains(got, "networkpolicy.ingres_enforced") || !strings.Contains(got, "networkpolicy.ingress_enforced") {
		t.Fatalf("an unknown kind must be refused by name with the nearest built-in; got: %q", got)
	}
}

func TestPrecondition_UndeclaredDependsOnIsRefused(t *testing.T) {
	_, errs := Validate(preconditionScenario("- **Preconditions**: np-in=networkpolicy.ingress_enforced\n- **Depends on**: np-nope\n"))
	if got := problemsOf(errs); !strings.Contains(got, "np-nope") || !strings.Contains(got, "not declared") {
		t.Fatalf("a depends_on naming an undeclared precondition must be refused; got: %q", got)
	}
}

func TestPrecondition_DependsOnWithNoDeclarationIsRefused(t *testing.T) {
	_, errs := Validate(preconditionScenario("- **Depends on**: np-in\n"))
	if got := problemsOf(errs); !strings.Contains(got, "not declared") {
		t.Fatalf("got: %q", got)
	}
}

func TestPrecondition_MalformedAndDuplicateEntriesAreRefused(t *testing.T) {
	for name, meta := range map[string]string{
		"no kind":      "- **Preconditions**: np-in\n",
		"bad id":       "- **Preconditions**: NP IN=networkpolicy.ingress_enforced\n",
		"duplicate id": "- **Preconditions**: a=networkpolicy.ingress_enforced, a=networkpolicy.egress_enforced\n",
		"empty":        "- **Preconditions**: \n",
	} {
		if _, errs := Validate(preconditionScenario(meta)); len(errs) == 0 {
			t.Errorf("%s: a malformed declaration was accepted", name)
		}
	}
}

// A scenario with neither key parses exactly as before: nothing declared, nothing depended on.
func TestPrecondition_AbsentKeysLeaveTheScenarioUntouched(t *testing.T) {
	s, errs := Validate(preconditionScenario(""))
	if len(errs) != 0 || len(s.Preconditions) != 0 || len(s.DependsOn) != 0 {
		t.Fatalf("errs=%v preconditions=%v depends=%v", errs, s.Preconditions, s.DependsOn)
	}
	if _, ok := s.AsParsedMap()["preconditions"]; ok {
		t.Fatal("a scenario without preconditions must not emit the key (the parsed map is byte-identical to today)")
	}
}

func TestPrecondition_ParsedMapCarriesBoth(t *testing.T) {
	m := Parse(preconditionScenario("- **Preconditions**: np-in=networkpolicy.ingress_enforced\n- **Depends on**: np-in\n")).AsParsedMap()
	if _, ok := m["preconditions"]; !ok {
		t.Fatal("preconditions missing from the parsed map")
	}
	if _, ok := m["depends_on"]; !ok {
		t.Fatal("depends_on missing from the parsed map")
	}
}
