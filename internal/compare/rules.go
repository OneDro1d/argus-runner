package compare

// Reference says what a check's output is compared with (design 2.1).
type Reference string

const (
	RefMeasured Reference = "measured" // another member's recorded output
	RefFixed    Reference = "fixed"    // the check's own claims are the expected output
	RefProperty Reference = "property" // the claims are a property that must hold in p% of runs
)

// Bounds of the `## COMPARE` section (design 2.1 and A9). One constant each.
const (
	MaxHeaders         = 8
	MaxMasks           = 32
	MaxUnordered       = 8
	MaxTolerances      = 16
	MaxSteps           = 32
	MaxRepeats         = 20
	MaxPathSegments    = 8
	MinAgreementPct    = 1.0
	MaxBandPercent     = 1000.0
	MaxBandPoints      = 100.0
	MaxSamples         = 8
	MaxToleranceValues = 64
	MaxBodyBytes       = 1 << 20   // the existing http read cap: a larger body is never recorded
	StoredBodyLimit    = 256 << 10 // the stored file keeps at most this much canonical body
	// MaxStoredFileBytes bounds the stored file ENCODED (a control byte is a 6-byte escape): the relayed
	// get_output answer is this file and must fit the control plane's 1 MiB command-result cap with room
	// for the envelope. BuildRecord cuts the body further until the file fits.
	MaxStoredFileBytes = 768 << 10
	MaxOutputsBytes    = 1 << 20 // ResultsPush.outputs, encoded
)

// OutputSel is which parts of a response are compared.
type OutputSel struct {
	Status  bool     `json:"status"`
	Body    bool     `json:"body"`
	Headers []string `json:"headers"` // lower-case names, sorted
}

// Tolerance lets numeric leaves at Path differ by an absolute (abs) or relative (rel) amount.
type Tolerance struct {
	Path  string  `json:"path"`
	Kind  string  `json:"kind"` // abs | rel
	Value float64 `json:"value"`
}

// Band is one `Not Worse Than` entry: Metric is p50 | p95 | p99 (Unit "%") or error_rate (Unit "pp").
type Band struct {
	Metric string  `json:"metric"`
	Value  float64 `json:"value"`
	Unit   string  `json:"unit"`
}

// Rules is the parsed, validated, defaulted `## COMPARE` section. Every list is sorted and
// de-duplicated and every default is applied, so two sections that mean the same thing encode to
// the same bytes (CanonicalJSON). It is the type the rules_hash is taken over.
type Rules struct {
	Reference    Reference   `json:"reference"`
	Output       OutputSel   `json:"output"`
	Mask         []string    `json:"mask"`
	Unordered    []string    `json:"unordered"`
	Tolerance    []Tolerance `json:"tolerance"`
	Repeats      int         `json:"repeats"`
	Agreement    float64     `json:"agreement"` // percent, 1..100
	NotWorseThan []Band      `json:"not_worse_than"`
	Steps        []string    `json:"steps"`
}

// RawKV is one `**Key**: value` line of the section as the scenario parser read it.
type RawKV struct {
	Key   string
	Value string
	Line  int // 1-based line in the scenario file; 0 when unknown
}

// Problem is one reason the section is not valid. UnknownKey marks a key outside the closed list so
// the caller can add a nearest-name suggestion.
type Problem struct {
	Key        string
	Line       int
	Msg        string
	UnknownKey bool
}

// PathRules pairs a scenario path with its rules, for RulesHash.
type PathRules struct {
	Path  string
	Rules *Rules
}

// Floors: the executor release that first understands a key (design 11.2).
const (
	FloorE1 = "E1"
	FloorE2 = "E2"
)

// KeysNeedingE2 names, in a fixed order and as an author writes them, the `## COMPARE` keys that some rule of the set uses
// and that KeyFloors files under E2 (ARGUS-CMP-11). It is what the control plane's floor reads, so the table stays the one
// place a key's release is decided. Empty when no rule uses one.
func KeysNeedingE2(rules []*Rules) []string {
	var tol, nwt bool
	for _, r := range rules {
		if r == nil {
			continue
		}
		tol = tol || len(r.Tolerance) > 0
		nwt = nwt || len(r.NotWorseThan) > 0
	}
	var out []string
	if tol && KeyFloors["Tolerance"] == FloorE2 {
		out = append(out, "Tolerance")
	}
	if nwt && KeyFloors["Not Worse Than"] == FloorE2 {
		out = append(out, "Not Worse Than")
	}
	return out
}

// KeyFloors is the one table of per-key executor floors. CompareTarget is the run-level override
// of the wire (RunAssignment.CompareTarget), not a section key.
var KeyFloors = map[string]string{
	"Reference":      FloorE1,
	"Output":         FloorE1,
	"Mask":           FloorE1,
	"Unordered":      FloorE1,
	"Repeats":        FloorE1,
	"Agreement":      FloorE1,
	"Steps":          FloorE1,
	"Tolerance":      FloorE2,
	"Not Worse Than": FloorE2,
	"CompareTarget":  FloorE2,
}
