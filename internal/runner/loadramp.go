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
// (never its URL), the driver and the frozen per-step record. The numbers are ALSO inside every
// scenario's evidence hash (evidenceHash marshals the whole report.ScenarioResult), so the run anchor
// commits to them without anything here.
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
			entries = append(entries, report.LoadRampEntry{ScenarioID: sc.ID, Target: sc.LoadTarget, Driver: driver, Steps: sc.LoadSteps,
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
