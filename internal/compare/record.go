package compare

// States and body kinds of an OutputRecord.
const (
	StateRecorded    = "recorded"
	StateNotRecorded = "not_recorded"

	KindJSON  = "json"
	KindText  = "text"
	KindEmpty = "empty"

	// RecordVersion is OutputRecord.V.
	RecordVersion = 1
)

// Parts holds the hash of each selected part (lower-case hex sha256). A part the check does not
// compare is absent. The total OutputRecord.Hash is taken over these three, see OutputHash.
type Parts struct {
	Status  string `json:"status,omitempty"`
	Headers string `json:"headers,omitempty"`
	Body    string `json:"body,omitempty"`
}

// ToleranceValue is one numeric leaf that a Tolerance rule governs. Such a leaf is removed from the
// hashed body (it appears there as the tolerance token) and carried here instead, so that two
// outputs that differ only inside the declared tolerance still hash equal. Rule indexes
// Rules.Tolerance (the canonical, sorted list).
type ToleranceValue struct {
	Path  string  `json:"path"`
	Rule  int     `json:"rule"`
	Value float64 `json:"value"`
}

// LoadNumbers are the measured numbers of a `## LOAD` check run, for `Not Worse Than`. ErrorRate is a
// fraction (0..1). Numbers only: no body, no header, no claim text.
type LoadNumbers struct {
	Samples   int     `json:"samples"`
	P50Ms     float64 `json:"p50_ms"`
	P95Ms     float64 `json:"p95_ms"`
	P99Ms     float64 `json:"p99_ms"`
	ErrorRate float64 `json:"error_rate"`
}

// OutputRecord is what a run keeps of one output of one check: a digest and sizes, never a body, a
// header value or a claim (design 1.3). It is what report.ScenarioResult.Outputs will hold.
type OutputRecord struct {
	V            int              `json:"v"`
	Step         string           `json:"step"`
	Sample       int              `json:"sample"`
	State        string           `json:"state"`
	Reason       string           `json:"reason"`
	Status       int              `json:"status"`
	Parts        Parts            `json:"parts"`
	Hash         string           `json:"hash"`
	BodyKind     string           `json:"body_kind"`
	BodyBytes    int              `json:"body_bytes"`
	Truncated    bool             `json:"truncated"`
	MasksApplied int              `json:"masks_applied"`
	Values       []ToleranceValue `json:"values,omitempty"`
	Load         *LoadNumbers     `json:"load,omitempty"`
}

// ScenarioOutput is one row of ResultsPush.outputs: the record plus the check it belongs to.
type ScenarioOutput struct {
	ScenarioID string `json:"scenario_id"`
	OutputRecord
}

// Response is the response of one request, as the executor read it. Body must hold at most
// MaxBodyBytes+1 bytes: one more than the cap is how a too-large body is recognised.
type Response struct {
	Status  int
	Headers map[string][]string // any case; repeated lines are joined with ", "
	Body    []byte
}

// Stored is the canonical, masked, scrubbed form the executor keeps on disk for the drill-down
// (design 1.3). It is never hashed as a file and never leaves the executor except through the relay.
type Stored struct {
	status       int
	headers      string // canonical headers object, "" when no header is compared
	bodyKind     string
	body         string // canonical body text, at most StoredBodyLimit bytes
	bodySelected bool
	truncated    bool
}
