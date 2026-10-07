package scenario

import (
	"strings"
	"testing"
)

// AC-11: the `## LOAD` block declares mode 2 ("under load") on a scenario — an optional profile
// that drives the JMeter thread group beyond the product's one-thread/one-loop default.

func withLoad(md, body string) string {
	block := "## LOAD\n" + body + "\n\n"
	return strings.Replace(md, "## TIMEOUT", block+"## TIMEOUT", 1)
}

const validLoadBody = "" +
	"- **Users**: 50\n" +
	"- **Ramp Seconds**: 10\n" +
	"- **Duration Seconds**: 60\n" +
	"- **Target P95 Ms**: 300\n" +
	"- **Max Error Rate**: 0.05"

// Absent `## LOAD` = the one-thread/one-loop default: Load stays nil, LoadDeclared stays false,
// and Validate raises nothing about it.
func TestParse_LoadAbsent(t *testing.T) {
	s := Parse(validMD())
	if s.LoadDeclared {
		t.Fatalf("a scenario with no ## LOAD must not report LoadDeclared")
	}
	if s.Load != nil {
		t.Fatalf("a scenario with no ## LOAD must have a nil Load, got %+v", s.Load)
	}
	if _, errs := Validate(validMD()); len(errs) != 0 {
		t.Fatalf("a scenario with no ## LOAD must validate clean, got %v", errs)
	}
}

// A valid `## LOAD` block parses into a fully-populated LoadProfile and validates clean.
func TestParse_LoadValid(t *testing.T) {
	md := withLoad(validMD(), validLoadBody)
	s := Parse(md)
	if !s.LoadDeclared {
		t.Fatalf("a scenario with ## LOAD must report LoadDeclared")
	}
	if s.Load == nil {
		t.Fatalf("a valid ## LOAD block must parse into a non-nil Load")
	}
	want := LoadProfile{Users: 50, RampSeconds: 10, DurationSeconds: 60, TargetP95Ms: 300, MaxErrorRate: 0.05}
	if *s.Load != want {
		t.Fatalf("Load = %+v, want %+v", *s.Load, want)
	}
	if _, errs := Validate(md); len(errs) != 0 {
		t.Fatalf("a valid ## LOAD block must validate clean, got %v", errs)
	}
}

// A nonsense value (non-numeric, negative, zero where positive is required, or out of bounds) is
// refused BY NAME with the ## LOAD line number — never silently dropped or half-kept.
func TestValidate_LoadNonsenseRefusedWithLine(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string // substring the refusal must contain
	}{
		{"non-numeric users", strings.Replace(validLoadBody, "**Users**: 50", "**Users**: fifty", 1), "Users"},
		{"zero users", strings.Replace(validLoadBody, "**Users**: 50", "**Users**: 0", 1), "Users"},
		{"negative ramp", strings.Replace(validLoadBody, "**Ramp Seconds**: 10", "**Ramp Seconds**: -1", 1), "Ramp Seconds"},
		{"out-of-bounds p95", strings.Replace(validLoadBody, "**Target P95 Ms**: 300", "**Target P95 Ms**: 9999999", 1), "Target P95 Ms"},
		{"error rate over 1", strings.Replace(validLoadBody, "**Max Error Rate**: 0.05", "**Max Error Rate**: 1.5", 1), "Max Error Rate"},
		{"zero error rate", strings.Replace(validLoadBody, "**Max Error Rate**: 0.05", "**Max Error Rate**: 0", 1), "Max Error Rate"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			md := withLoad(validMD(), c.body)
			_, errs := Validate(md)
			var found *Error
			for i := range errs {
				if strings.Contains(errs[i].Message, c.want) {
					found = &errs[i]
					break
				}
			}
			if found == nil {
				t.Fatalf("expected a ## LOAD refusal naming %q, got %v", c.want, errs)
			}
			if found.Line == 0 {
				t.Errorf("the refusal must carry the ## LOAD line number, got line 0: %+v", found)
			}
		})
	}
}

// The parser stays lenient (VR-A10 split): a value that does not even PARSE as a number leaves Load
// nil rather than half-populated. Validate is where nonsense is refused, with a line number.
func TestParse_LoadUnparseableLeavesLoadNil(t *testing.T) {
	body := strings.Replace(validLoadBody, "**Users**: 50", "**Users**: fifty", 1)
	s := Parse(withLoad(validMD(), body))
	if s.Load != nil {
		t.Fatalf("a non-numeric field must leave Load nil, got %+v", s.Load)
	}
	if !s.LoadDeclared {
		t.Fatalf("LoadDeclared must still be true — the section IS present, just unparseable")
	}
}

// A ## LOAD block missing a required field is refused by name, with the line number.
func TestValidate_LoadMissingFieldRefused(t *testing.T) {
	body := "- **Users**: 50\n- **Ramp Seconds**: 10\n- **Duration Seconds**: 60\n- **Target P95 Ms**: 300"
	md := withLoad(validMD(), body)
	_, errs := Validate(md)
	found := false
	for _, e := range errs {
		if strings.Contains(e.Message, "Max Error Rate") && e.Line != 0 {
			found = true
		}
	}
	if !found {
		t.Fatalf("a missing **Max Error Rate** must be refused by name with a line number, got %v", errs)
	}
}
