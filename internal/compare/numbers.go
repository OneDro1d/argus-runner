package compare

import (
	"encoding/json"
	"fmt"
	"strings"
)

// numbers.go -- the numbers that cross to the control plane in a compare run (ARGUS-CMP-11,
// and -17): the tolerant values of a check that declares `Tolerance`, and the load
// numbers of a check that declares `## LOAD` + `Not Worse Than`.
//
// ⛔ CUSTODY. They are NUMBERS from a response or a measurement. They live in OutputRecord.Values and
// OutputRecord.Load, typed, bounded and re-encoded by the control plane (so a key the type does not declare cannot
// be stored); they reach no log, no anchor payload, no alert and no builder answer.

// LoadRecord is the ONE record of a load check: the numbers a `## LOAD` run already computed (report.LoadStats), and
// nothing else. It is `not_recorded` with the reason `load_numbers_only`, so it has no status, no part hash and no
// total hash: nothing is invented for it, and the measured-output judge (which reads only `recorded` rows) can never
// read it as an output that is identical or differs.
func LoadRecord(n LoadNumbers) OutputRecord {
	return OutputRecord{V: RecordVersion, Sample: 1, State: StateNotRecorded, Reason: ReasonLoadNumbersOnly, Load: &n}
}

// isLoadOnly is a row that carries load numbers and no output.
func isLoadOnly(o OutputRecord) bool {
	return o.State == StateNotRecorded && o.Reason == ReasonLoadNumbersOnly
}

// numbersValid is the part of validate that concerns only the numbers of a row.
func (o ScenarioOutput) numbersValid() bool {
	if len(o.Values) > MaxToleranceValues {
		return false
	}
	for _, v := range o.Values {
		if v.Path == "" || len(v.Path) > 512 || hasControl(v.Path) || v.Rule < 0 || v.Rule >= MaxTolerances || !finite(v.Value) {
			return false
		}
	}
	if l := o.Load; l != nil && !loadNumbersOK(l) {
		return false
	}
	return true
}

// SanitizeOutputs is how the control plane reads ResultsPush.outputs. It differs from DecodeOutputs in ONE thing: a
// row that is valid except for its NUMBERS (a negative or non-finite load number, an error rate over 1, a tolerant
// value with a rule index out of range or a path that is not one, more than MaxToleranceValues tolerant values in one
// check) is DROPPED, alone (a check with too many values loses every row it has), and counted; it does not take the
// other rows, and the caller never refuses the push over it. A dropped row is simply absent: "not measured". A row
// that is wrong in any other way (a state, a reason outside the closed vocabulary, a part hash that is not hex, ...)
// still refuses the whole array, because the type is the wire's contract.
//
// sentRoot is OutputsRoot over every row as sent (a row's root covers its scenario id, step, sample and hash and, for a row
// that carries them, its tolerant values and load numbers: fix F5), which is what the executor's own outputs_root commits to;
// kept is what is stored and norm its re-encoding.
func SanitizeOutputs(raw []byte) (kept []ScenarioOutput, norm []byte, sentRoot string, dropped int, err error) {
	if len(raw) > MaxOutputsBytes {
		return nil, nil, "", 0, fmt.Errorf("outputs are %d bytes, at most %d", len(raw), MaxOutputsBytes)
	}
	if !strings.HasPrefix(strings.TrimSpace(string(raw)), "[") {
		return nil, nil, "", 0, fmt.Errorf("outputs must be a JSON array")
	}
	var rows []ScenarioOutput
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, nil, "", 0, fmt.Errorf("outputs do not decode: %v", err)
	}
	bad := make([]bool, len(rows))
	sent := make([]ScenarioOutput, 0, len(rows))
	perCheck := map[string]int{}
	for i, r := range rows {
		if verr := r.validate(); verr != nil {
			bare := r
			bare.Values, bare.Load = nil, nil
			if bare.validate() != nil {
				return nil, nil, "", 0, fmt.Errorf("outputs[%d]: %v", i, verr)
			}
			bad[i] = true
			// the row as SENT, numbers included: the executor's root committed to them (fix F5), so the root over the rows as
			// sent must too, or a push with one malformed number would never match and would lose every row
			sent = append(sent, r)
			continue
		}
		perCheck[r.ScenarioID] += len(r.Values)
		sent = append(sent, r)
	}
	for i, r := range rows {
		if perCheck[r.ScenarioID] > MaxToleranceValues {
			bad[i] = true
		}
	}
	kept = []ScenarioOutput{}
	for i, r := range rows {
		if bad[i] {
			dropped++
			continue
		}
		kept = append(kept, r)
	}
	norm, err = json.Marshal(kept)
	if err != nil {
		return nil, nil, "", 0, err
	}
	if len(norm) > MaxOutputsBytes {
		return nil, nil, "", 0, fmt.Errorf("outputs are %d bytes once encoded, at most %d", len(norm), MaxOutputsBytes)
	}
	return kept, norm, OutputsRoot(sent), dropped, nil
}

// CapValuesPerCheck is the executor's half of the per-check bound: when the rows of ONE check (its steps and samples
// together) carry more than MaxToleranceValues tolerant values, every row of that check becomes `not_recorded` with the
// reason `too_many_values`: nothing is compared on a number set the control plane would drop. A check within the bound is
// untouched.
func CapValuesPerCheck(rows []ScenarioOutput) []ScenarioOutput {
	total := map[string]int{}
	for _, r := range rows {
		total[r.ScenarioID] += len(r.Values)
	}
	out := make([]ScenarioOutput, len(rows))
	for i, r := range rows {
		if total[r.ScenarioID] > MaxToleranceValues {
			r.State, r.Reason = StateNotRecorded, ReasonTooManyValues
			r.Hash, r.Parts, r.Values = "", Parts{}, nil
		}
		out[i] = r
	}
	return out
}
