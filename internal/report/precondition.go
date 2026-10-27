package report

import "fmt"

// Precondition is one precondition's result as the in-env report records it. It is
// executor-attested: the executor ran the probe in its own namespace. Positive and Negative are the two
// controls' observations in words, never an address.
type Precondition struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Verdict  string `json:"verdict"` // holds | absent | broken
	Positive string `json:"positive"`
	Negative string `json:"negative"`
	Residue  string `json:"residue,omitempty"`
}

// Precondition verdicts. Kept equal to internal/precondition's (a drift test pins it).
const (
	PreconditionHolds  = "holds"
	PreconditionAbsent = "absent"
	PreconditionBroken = "broken"
)

// NotApplicable is why a scenario ended StatusNotApplicable: the precondition, and the two observations that
// show the mechanism is absent.
type NotApplicable struct {
	Precondition string `json:"precondition"`
	Kind         string `json:"kind"`
	Positive     string `json:"positive"`
	Negative     string `json:"negative"`
}

// Line is the one sentence every surface prints: "not applicable: <precondition> absent" with both controls.
func (n NotApplicable) Line() string {
	return fmt.Sprintf("not applicable: %s absent (positive control: %s; negative control: %s)", n.Precondition, n.Positive, n.Negative)
}
