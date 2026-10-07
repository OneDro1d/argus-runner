package compare

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
)

func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func hasControl(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] == 0x7f {
			return true
		}
	}
	return false
}

func okNumber(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) && f >= 0 }

// loadNumbersOK is the ONE rule for the load numbers of a row: finite, non-negative, an error rate in [0, 1], a count that is not
// negative, and (ARGUS-CMP-11 fix F3) percentiles that are in order, p50 <= p95 <= p99. Equal values are fine: report.ComputeLoadStats
// takes nearest-rank percentiles of one ascending-sorted sample list, so the three are non-decreasing for ANY list, and one sample
// (or no sample) gives three equal numbers; a real run is never dropped by the ordering.
func loadNumbersOK(l *LoadNumbers) bool {
	return okNumber(l.P50Ms) && okNumber(l.P95Ms) && okNumber(l.P99Ms) && okNumber(l.ErrorRate) && l.ErrorRate <= 1 && l.Samples >= 0 &&
		l.P50Ms <= l.P95Ms && l.P95Ms <= l.P99Ms
}

func (o ScenarioOutput) validate() error {
	switch {
	case o.ScenarioID == "" || len(o.ScenarioID) > 64 || hasControl(o.ScenarioID):
		return fmt.Errorf("scenario_id %q is not a scenario id", o.ScenarioID)
	case len(o.Step) > 128 || hasControl(o.Step):
		return fmt.Errorf("step is too long or has control characters")
	case o.V != RecordVersion:
		return fmt.Errorf("record version %d is not %d", o.V, RecordVersion)
	case o.Sample < 0 || o.Sample > MaxSamples:
		return fmt.Errorf("sample %d is out of [0, %d]", o.Sample, MaxSamples)
	case o.BodyBytes < 0 || o.BodyBytes > MaxBodyBytes+1:
		return fmt.Errorf("body_bytes %d is out of range", o.BodyBytes)
	case o.MasksApplied < 0 || o.MasksApplied > 10000:
		return fmt.Errorf("masks_applied %d is out of range", o.MasksApplied)
	case o.Status != 0 && (o.Status < 100 || o.Status > 599):
		return fmt.Errorf("status %d is not an HTTP status", o.Status)
	case o.BodyKind != "" && o.BodyKind != KindJSON && o.BodyKind != KindText && o.BodyKind != KindEmpty:
		return fmt.Errorf("body_kind %q is not json, text or empty", o.BodyKind)
	case len(o.Values) > MaxToleranceValues:
		return fmt.Errorf("%d tolerant values, at most %d", len(o.Values), MaxToleranceValues)
	}
	for _, v := range o.Values {
		if v.Path == "" || len(v.Path) > 512 || hasControl(v.Path) || v.Rule < 0 || v.Rule >= MaxTolerances ||
			math.IsNaN(v.Value) || math.IsInf(v.Value, 0) {
			return fmt.Errorf("a tolerant value is malformed")
		}
	}
	if l := o.Load; l != nil && !loadNumbersOK(l) {
		return fmt.Errorf("load numbers are malformed")
	}
	for _, p := range []string{o.Parts.Status, o.Parts.Headers, o.Parts.Body} {
		if p != "" && !isHex64(p) {
			return fmt.Errorf("a part hash is not 64 lower-case hex characters")
		}
	}
	switch o.State {
	case StateRecorded:
		if o.Reason != "" {
			return fmt.Errorf("a recorded output has no reason")
		}
		if !isHex64(o.Hash) {
			return fmt.Errorf("hash is not 64 lower-case hex characters")
		}
		if o.Status < 100 {
			return fmt.Errorf("a recorded output has a status")
		}
	case StateNotRecorded:
		if !ValidReason(o.Reason) {
			return fmt.Errorf("reason is not in the closed vocabulary")
		}
		if o.Hash != "" || o.Parts != (Parts{}) {
			return fmt.Errorf("an output that was not recorded carries no hash")
		}
	default:
		return fmt.Errorf("state %q is not recorded or not_recorded", o.State)
	}
	return nil
}

// DecodeOutputs is how the control plane reads ResultsPush.outputs: it decodes into the typed row,
// validates every field against the closed vocabulary and the bounds, and returns the re-encoded
// bytes, so a key the type does not declare (a body, a header value, a claim) cannot be stored.
func DecodeOutputs(raw []byte) ([]ScenarioOutput, []byte, error) {
	if len(raw) > MaxOutputsBytes {
		return nil, nil, fmt.Errorf("outputs are %d bytes, at most %d", len(raw), MaxOutputsBytes)
	}
	if !strings.HasPrefix(strings.TrimSpace(string(raw)), "[") {
		return nil, nil, fmt.Errorf("outputs must be a JSON array")
	}
	var rows []ScenarioOutput
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, nil, fmt.Errorf("outputs do not decode: %v", err)
	}
	if rows == nil {
		rows = []ScenarioOutput{}
	}
	for i, r := range rows {
		if err := r.validate(); err != nil {
			return nil, nil, fmt.Errorf("outputs[%d]: %v", i, err)
		}
	}
	norm, err := json.Marshal(rows)
	if err != nil {
		return nil, nil, err
	}
	if len(norm) > MaxOutputsBytes {
		return nil, nil, fmt.Errorf("outputs are %d bytes once encoded, at most %d", len(norm), MaxOutputsBytes)
	}
	return rows, norm, nil
}
