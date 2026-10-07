// Package failcontext holds the saga + log result types shared by the runner-core's
// observability layer (internal/obsquery) and its consumers — get_sagas / get_tail_logs.
//
// The consolidated get_failure_context BUNDLE and its role-gating (Assemble/Redact) were
// removed in M25-FX4 (C1): the failure-triage skill uses the multi-call saga-first flow
// (get_report → get_sagas → get_tail_logs), and the product-hat holdout is enforced by
// get_report's expected-redaction. What remains here are just the wire shapes for sagas
// and logs.
package failcontext

type Saga struct {
	Available     bool       `json:"available"`
	FailedStep    string     `json:"failed_step,omitempty"`
	LastKnownGood string     `json:"last_known_good,omitempty"`
	Note          string     `json:"note,omitempty"`   // DF-09: why empty (e.g. unknown id / outside window) — never silent available:true+empty
	Window        string     `json:"window,omitempty"` // C7: the effective lookback window echoed by get_sagas
	Timeline      []SagaStep `json:"timeline"`
}

type SagaStep struct {
	SagaID     string     `json:"saga_id"`
	Step       int        `json:"step"`
	StepName   string     `json:"step_name"`
	Service    string     `json:"service"`
	Timestamp  string     `json:"timestamp"`
	DurationMs int        `json:"duration_ms"`
	StepStatus string     `json:"step_status"`
	Error      string     `json:"error,omitempty"`
	Fields     SagaFields `json:"fields"`
	// C6: provenance is populated ONLY for control-action steps — omit (don't render null)
	// for the common pipeline steps, instead of advertising fields that are always null.
	HMACVerified *bool   `json:"hmac_verified,omitempty"`
	ChainAnchor  *string `json:"chain_anchor,omitempty"`
}

// SagaFields carries the Axiom #25 what/why/by-whom (omitted when not a control action — C6).
type SagaFields struct {
	What   *string `json:"what,omitempty"`
	Why    *string `json:"why,omitempty"`
	ByWhom *string `json:"by_whom,omitempty"`
}

type Logs struct {
	Available bool      `json:"available"`
	Source    string    `json:"source"`
	Window    string    `json:"window"`
	Note      string    `json:"note,omitempty"`
	Lines     []LogLine `json:"lines"`
}

type LogLine struct {
	TS      string            `json:"ts"`
	Level   string            `json:"level"`
	Service string            `json:"service"`
	Msg     string            `json:"msg"`
	Fields  map[string]string `json:"fields,omitempty"`
}
