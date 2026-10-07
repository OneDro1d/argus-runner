package compare

// CellState is the state of one cell: one check, one member, one measured version (design 7.2).
type CellState string

const (
	StateIdentical   CellState = "identical"
	StateDiffers     CellState = "differs"
	StateNoise       CellState = "noise"
	StateNotMeasured CellState = "not_measured"
	StateCouldNotRun CellState = "could_not_run"
)

// BandState is the state of one `Not Worse Than` cell.
type BandState string

const (
	BandNotWorse    BandState = "not_worse"
	BandWorse       BandState = "worse"
	BandNotMeasured BandState = "not_measured"
)

// The per-check outcomes the executor reports (report.ScenarioResult status).
const (
	OutcomePassed   = "passed"
	OutcomeFailed   = "failed"
	OutcomeErrored  = "errored"
	OutcomeDegraded = "degraded" // own claims passed; counts as passed
)

// ClaimJudgement is the verdict of a fixed or property claim over a member's runs (design 3).
type ClaimJudgement struct {
	State          CellState
	Runs           int // n: runs that passed or failed
	Held           int // passed (or degraded)
	CouldNotRun    int // errored or unrecognised, excluded from n
	ClaimSupported bool
	Supports       string // what n supports, "" when n = 0
	Unsupported    string // set when the claim is not supported
}

// BandJudgement is the verdict of one `Not Worse Than` entry for one member.
type BandJudgement struct {
	State         BandState
	Reference     float64 // median over the reference's runs
	Member        float64 // median over the member's runs
	ReferenceRuns int
	MemberRuns    int
}
