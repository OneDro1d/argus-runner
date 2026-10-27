package runner

import (
	"encoding/json"
	"sort"

	"github.com/OneDro1d/argus-runner/internal/report"
)

// loadRampWire builds ResultsPush.load_ramp: one report.LoadRampEntry per scenario that
// carries load steps, in ascending scenario-id order (never report/layer order, which depends on catalog
// grouping), or nil when the run carried none -- the field is then absent from the wire and an older
// control plane never knows it existed. Only the measurement record crosses: scenario id, the NAMED target
// (never its URL), the driver and each step as report.LoadStepWithoutTexts projects it (AC-F2.2): a load error
// as its word and count, a broker pause as its word and times, never the load tool's message or the broker's
// words, which stay in the report. The numbers are ALSO inside every scenario's evidence hash (evidenceHash
// marshals the whole report.ScenarioResult), so the run anchor commits to them without anything here.
func loadRampWire(rep *report.Report) json.RawMessage {
	var entries []report.LoadRampEntry
	for _, l := range rep.Layers {
		for _, sc := range l.Scenarios {
			if len(sc.LoadSteps) == 0 {
				continue
			}
			driver := sc.LoadDriver
			if driver == "" {
				driver = "amqp"
			}
			entries = append(entries, report.LoadRampEntry{ScenarioID: sc.ID, Target: sc.LoadTarget, Driver: driver, Steps: stepsWithoutTexts(sc.LoadSteps),
				StoppedAtStep: sc.LoadStoppedAtStep})
		}
	}
	if len(entries) == 0 {
		return nil
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].ScenarioID < entries[j].ScenarioID })
	b, err := json.Marshal(entries)
	if err != nil {
		return nil // cannot happen for this record; a ramp that cannot be encoded is simply not sent
	}
	return b
}

// stepsWithoutTexts is report.LoadStepWithoutTexts of every step, in a new slice.
func stepsWithoutTexts(steps []report.LoadStep) []report.LoadStep {
	out := make([]report.LoadStep, len(steps))
	for i, st := range steps {
		out[i] = report.LoadStepWithoutTexts(st)
	}
	return out
}

// loadRampWithoutTexts is a load_ramp already encoded — a push the outbox queued, perhaps by an executor older than
// AC-F2.2 whose ramp still holds the texts — with every step projected as loadRampWire projects it. A ramp that does
// not decode as entries is not sent at all (nil: the field is optional), never as its raw bytes.
func loadRampWithoutTexts(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}
	var entries []report.LoadRampEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil
	}
	for i := range entries {
		entries[i].Steps = stepsWithoutTexts(entries[i].Steps)
	}
	b, err := json.Marshal(entries)
	if err != nil {
		return nil
	}
	return b
}
