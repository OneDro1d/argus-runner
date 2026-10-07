package chain

import (
	"net/http"

	"github.com/OneDro1d/argus-runner/internal/compare"
	"github.com/OneDro1d/argus-runner/internal/report"
)

// OutputCapture asks an http chain step to record the response it judged (ARGUS-CMP-3).
type OutputCapture struct {
	// Spec says which parts are compared, which fields are masked, which arrays are unordered. Its
	// Scrub field is ignored: the step builds the scrubber itself, from the run's saved variables
	// (redactEverySaved) and Scrub below.
	Spec compare.Spec
	// Scrub removes what must never be kept: the config's credentials and token-shaped runs. The caller
	// (internal/argus) supplies it; it is applied AFTER the saved values are put back as their placeholders.
	Scrub func(string) string
}

// recordOutput is the one place a response becomes a record. nil oc records nothing. The scrub runs
// inside compare.BuildRecord (Spec.Scrub: after masking, before hashing), so the hash and the stored
// bytes agree and nothing is scrubbed twice.
func recordOutput(oc *OutputCapture, step string, status int, hdr http.Header, body []byte, vars, captured map[string]string) *report.RecordedOutput {
	if oc == nil {
		return nil
	}
	// the saved values the step itself just captured count too: a login response echoes the very id or
	// token it saves, and the capture store only gets it after the step returns
	known := vars
	if len(captured) > 0 {
		known = make(map[string]string, len(vars)+len(captured))
		for k, v := range vars {
			known[k] = v
		}
		for k, v := range captured {
			known[k] = v
		}
	}
	spec := oc.Spec
	spec.Scrub = func(s string) string {
		s = redactEverySaved(s, known, "")
		if oc.Scrub != nil {
			s = oc.Scrub(s)
		}
		return s
	}
	rec, stored := compare.BuildRecord(compare.Response{Status: status, Headers: hdr, Body: body}, spec, step, 1)
	return &report.RecordedOutput{Record: rec, Stored: stored}
}
