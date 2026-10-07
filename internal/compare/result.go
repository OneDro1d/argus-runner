package compare

import "fmt"

// Verdict is the roll-up of a comparison (design 7.4).
type Verdict string

const (
	VerdictSame             Verdict = "same"
	VerdictSameWithApproved Verdict = "same_with_approved_differences"
	VerdictDifferences      Verdict = "differences"
	VerdictIncomplete       Verdict = "incomplete"
)

// Member roles in a comparison.
const (
	RoleReference = "reference"
	RoleCandidate = "candidate"
)

// Roles of one run REQUEST of a comparison (run_requests.comparison_role). A candidate's runs carry
// RoleCandidate. The reference member's first run is the reference and its others are controls: the
// reference member run again against itself, which is what shows how much it varies on its own.
const (
	RunRoleReference = "reference"
	RunRoleControl   = "control"
)

// Control states of a reference (design 5).
const (
	ControlStable   = "stable"
	ControlUnstable = "unstable"
	ControlUntested = "untested"
)

// Claim kinds, as the table names them.
const (
	ClaimSameOutput = "same_output"
	ClaimFixed      = "fixed"
	ClaimProperty   = "property"
)

// Notes: a closed list of sentences about the comparison as a whole, never built from a response.
const (
	NoteNoSeedStep                = "no seed step declared: the same starting state is assumed, not checked"
	NoteNoReferenceMember         = "a measured check needs a reference member and the comparison has none"
	NoteReferenceVersionAmbiguous = "the reference ran at more than one version and none was chosen"
	NoteReferenceSingleRun        = "the reference ran once, so its own variation could not be tested"
	// Why there is no usable reference at all (every measured cell then reads "not measured: there is no
	// reference output to compare with", and the table must say what stands behind that).
	NoteReferenceOtherSet = "the reference run was of a different set than the comparison"
	NoteReferenceFailed   = "the reference run failed before running"
	NoteReferenceNotRun   = "the reference has not run yet"
	// A seed check that declares its own ## COMPARE and whose comparison does not hold: the systems did not start
	// from the same state (ARGUS-CMP-7).
	NoteSeedDidNotHold = "a seed check's own comparison did not hold: the systems did not start from the same state"
)

// RunsPendingNote is the closed sentence for planned runs that have not finished ("" when none): only the
// count is interpolated, never anything from a run.
func RunsPendingNote(n int) string {
	switch {
	case n <= 0:
		return ""
	case n == 1:
		return "1 planned run has not finished"
	}
	return fmt.Sprintf("%d planned runs have not finished", n)
}

// Check is one check of the set.
type Check struct {
	ID    string
	Path  string
	Rules *Rules // nil: the check declares no ## COMPARE and is not part of the comparison
	Seed  bool
}

// Member is one system in the comparison.
type Member struct {
	Name string
	Role string // RoleReference | RoleCandidate
}

// Run is one terminal (or pending) run of one member.
type Run struct {
	ID         string
	Member     string
	VersionKey string
	Status     string // completed | degraded | failed | abandoned | anything else = not terminal yet. `failed` WITH Outcomes is a run that ran with a failing check (ARGUS-CMP-14); `failed` without any is one that never ran its checks
	SetHash    string
	Outcomes   map[string]string // check id -> passed | failed | errored | degraded
	Outputs    []ScenarioOutput
}

// Approval pins an accepted difference to the two hashes it was accepted at (design 7.5, ARGUS-CMP-10). It is DATA the
// caller read from its store: the pure function reads no clock and no store. ID names the stored record (the cell
// lists it by id, so the author can see which approval counts, which was revoked and which no longer matches); Step
// is the step of a chain check the author looked at ("" for a check with no named step); Revoked is true once the
// author withdrew it. No reason and no approver are here: they are free text and never part of the hashed output.
type Approval struct {
	ID            string
	Check         string
	Step          string
	Member        string
	VersionKey    string
	ReferenceHash string
	MemberHash    string
	Revoked       bool
}

// The three states an approval listed on a cell can be in.
const (
	ApprovalLive    = "live"    // matches this cell's two hashes now, and the cell differs
	ApprovalRevoked = "revoked" // the author withdrew it
	ApprovalStale   = "stale"   // an output moved (or the cell no longer differs): it no longer counts
)

// ApprovalRef is one approval listed on a cell: its id, the step it was given for and why it does or does not count.
type ApprovalRef struct {
	ID     string `json:"id"`
	Step   string `json:"step"`
	Status string `json:"status"`
}

// Input is everything Result reads.
type Input struct {
	SetHash          string // the comparison's set hash; a run of any other set contributes nothing
	ReferenceVersion string // optional: which version_key of the reference member is the reference
	Checks           []Check
	Members          []Member
	Runs             []Run
	Approvals        []Approval
}

// Cell is one check x member x version.
type Cell struct {
	Member         string    `json:"member"`
	VersionKey     string    `json:"version_key"`
	State          CellState `json:"state"`
	Reference      bool      `json:"reference"` // the reference member's own cell of a measured check
	Runs           int       `json:"runs"`
	Agreed         int       `json:"agreed"`
	CouldNotRun    int       `json:"could_not_run"`
	Short          bool      `json:"short"` // fewer usable runs than the declared Repeats
	PartsDiffer    []string  `json:"parts_differ"`
	Control        string    `json:"control"`
	ClaimSupported bool      `json:"claim_supported"`
	Supports       string    `json:"supports"`
	Unsupported    string    `json:"unsupported"`
	ReferenceHash  string    `json:"reference_hash"`
	MemberHash     string    `json:"member_hash"`
	Approved       bool      `json:"approved"`
	// ValuePaths names the DECLARED tolerance paths (`**Tolerance**` rule paths) at which a tolerant value is outside
	// its tolerance, or present on one side only (ARGUS-CMP-11). Absent unless parts_differ has `values`.
	ValuePaths []string `json:"value_paths,omitempty"`
	// Approvals lists every approval pinned to this check, member and version, with why it does or does not count
	// (ARGUS-CMP-10). Absent when there is none, so the encoding (and the result hash) of a comparison with no approval
	// is exactly what it was before.
	Approvals []ApprovalRef `json:"approvals,omitempty"`
	// Steps is every step either side recorded for the check; DiffSteps the steps in which the member's outputs differ
	// from the reference's ([""] for a check with no named step and for a fixed or property cell). Neither is in the
	// encoding: they are what an approval is given against, read by the control plane.
	Steps     []string `json:"-"`
	DiffSteps []string `json:"-"`
	Sentence  string   `json:"sentence"`
}

// RefInfo describes the reference of one measured check.
type RefInfo struct {
	Stable       bool     `json:"stable"`
	Runs         int      `json:"runs"`          // the reference's usable runs (those that could not run are NOT in it)
	CouldNotRun  int      `json:"could_not_run"` // reference runs excluded because they could not run: named here, never a "different output"
	VaryingParts []string `json:"varying_parts"`
	Sentence     string   `json:"sentence"` // closed sentence when the reference is unstable ("" otherwise)
}

// CheckResult is one row of the table.
type CheckResult struct {
	ID        string  `json:"id"`
	Path      string  `json:"path"`
	Seed      bool    `json:"seed"`
	Claim     string  `json:"claim"`
	Reference RefInfo `json:"reference"`
	Cells     []Cell  `json:"cells"`
}

// PerfValue is a median and the number of runs it came from.
type PerfValue struct {
	Value float64 `json:"value"`
	Runs  int     `json:"runs"`
}

// PerfCell is one member's value for one band.
type PerfCell struct {
	Member     string    `json:"member"`
	VersionKey string    `json:"version_key"`
	Value      float64   `json:"value"`
	Runs       int       `json:"runs"`
	State      BandState `json:"state"`
}

// PerfResult is one `Not Worse Than` entry of one check.
type PerfResult struct {
	Check     string     `json:"check"`
	Metric    string     `json:"metric"`
	Band      string     `json:"band"`
	Reference PerfValue  `json:"reference"`
	Cells     []PerfCell `json:"cells"`
}

// Counts is the tally of the roll-up. Cells counted are the non-seed cells of candidate members (and,
// for fixed and property checks, of every member).
type Counts struct {
	Checks  int `json:"checks"`
	Members int `json:"members"`
	// Runs is the number of TERMINAL runs the table was built from: runs that landed (completed or degraded)
	// and runs that ended without a result (failed, abandoned, or cancelled before they ran), which the table
	// shows as could-not-run. A request that has not run yet is NOT in it.
	Runs int `json:"runs"`
	// RunsPending is the number of planned runs that have not finished: queued, picked up or running (a run
	// that has no result pushed yet is in it too). Runs + RunsPending is every run the table holds.
	RunsPending int `json:"runs_pending"`
	Identical   int `json:"identical"`
	Differs     int `json:"differs"`  // not approved
	Approved    int `json:"approved"` // differs, with a matching approval
	Noise       int `json:"noise"`
	NotMeasured int `json:"not_measured"`
	CouldNotRun int `json:"could_not_run"`
	Worse       int `json:"worse"` // Not Worse Than cells over their band
}

// System is one member at one version, for the column headers.
type System struct {
	Member     string `json:"member"`
	Role       string `json:"role"`
	VersionKey string `json:"version_key"`
	Runs       int    `json:"runs"`
}

// Output is the whole result. Its Hash is what the ledger anchors.
type Output struct {
	Verdict     Verdict       `json:"verdict"`
	Counts      Counts        `json:"counts"`
	Systems     []System      `json:"systems"`
	Checks      []CheckResult `json:"checks"`
	Performance []PerfResult  `json:"performance"`
	Notes       []string      `json:"notes"`
}
