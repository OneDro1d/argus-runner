package runner

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/OneDro1d/argus-runner/internal/argus"
	"github.com/OneDro1d/argus-runner/internal/compare"
	"github.com/OneDro1d/argus-runner/internal/federation"
	"github.com/OneDro1d/argus-runner/internal/report"
)

// ARGUS-CMP-3 (/-05): the executor side of a `compare` run's push, and the retention of
// the stored outputs.

// attachCompareOutputs fills ResultsPush.Outputs, OutputsRoot and EnvFingerprint from the run's report.
//
//   - Outputs is the JSON array of compare.ScenarioOutput, one row per recorded sample, in scenario-id,
//     step, sample order, decoded and re-encoded through compare.DecodeOutputs, the control plane's own
//     reader, so the bytes on the wire are the bytes it would store. Digests, sizes and closed reasons only.
//   - An output set the reader refuses, or one over compare.MaxOutputsBytes, is OMITTED whole (not
//     measured on the control plane) and never fails the run: refusing would lose the verdict.
//   - EnvFingerprint is the environment capture's fingerprint when one was taken (it is taken in every
//     compare run); an unavailable capture leaves it empty, never a guess.
func AttachCompareOutputs(push *federation.ResultsPush, rep *report.Report) {
	attachCompareOutputs(push, rep)
}

func attachCompareOutputs(push *federation.ResultsPush, rep *report.Report) {
	if rep.Environment != nil && rep.Environment.Captured {
		push.EnvFingerprint = rep.Environment.Fingerprint
	}
	var rows []compare.ScenarioOutput
	for _, layer := range rep.Layers {
		for _, sc := range layer.Scenarios {
			for _, o := range sc.Outputs {
				row := compare.ScenarioOutput{ScenarioID: sc.ID, OutputRecord: o}
				// One row the control plane's reader would refuse (a 65-byte scenario id is legal in the local
				// store) must not take the others with it: it alone is dropped and warned, by id and rule, with
				// no content. A dropped row is simply absent (not measured), never a recorded one.
				if reason := rowRefusal(row); reason != "" {
					slog.Warn("compare: an output row was left out of the push; the control plane's reader would refuse it",
						"scenario_id", sc.ID, "rule", reason)
					continue
				}
				rows = append(rows, row)
			}
		}
	}
	if len(rows) == 0 {
		return
	}
	// ARGUS-CMP-11: a check whose rows carry more tolerant values than the control plane keeps (compare.MaxToleranceValues)
	// is not compared on them: every row of it says not_recorded / too_many_values
	rows = compare.CapValuesPerCheck(rows)
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.ScenarioID != b.ScenarioID {
			return a.ScenarioID < b.ScenarioID
		}
		if a.Step != b.Step {
			return a.Step < b.Step
		}
		return a.Sample < b.Sample
	})
	raw, err := json.Marshal(rows)
	if err != nil {
		return
	}
	decoded, norm, err := compare.DecodeOutputs(raw)
	if err != nil {
		return
	}
	push.Outputs = json.RawMessage(norm)
	push.OutputsRoot = compare.OutputsRoot(decoded)
}

// rowRefusal runs one row through compare.DecodeOutputs, the control plane's own reader, alone. It returns
// the reader's rule (its wording names a field and a bound, never a value) or "" when the row is accepted.
func rowRefusal(row compare.ScenarioOutput) string {
	raw, err := json.Marshal([]compare.ScenarioOutput{row})
	if err != nil {
		return "the row does not encode"
	}
	if _, _, err := compare.DecodeOutputs(raw); err != nil {
		return strings.TrimPrefix(err.Error(), "outputs[0]: ")
	}
	return ""
}

// storedRunExists: the run's own directory is there under the outputs base (a plain directory, not a link).
func storedRunExists(base, runID string) bool {
	if runID == "" || runID != filepath.Base(runID) || runID == "." || runID == ".." {
		return false
	}
	fi, err := os.Lstat(filepath.Join(base, runID))
	return err == nil && fi.IsDir()
}

// OutputRetention returns the hook an Executor runs AFTER a results push the control plane accepted: for a
// compare push it keeps the newest argus.KeepNewestOutputRuns run directories of this instance's outputs
// and removes the older ones (never the run just pushed), through argus.PruneOutputs and its path guard.
// remove is the deleting function (os.RemoveAll when nil); a test injects a recording fake.
func OutputRetention(cfg ExecConfig, remove func(string) error) func(federation.ResultsPush) {
	instance := cfg.ToolInstance
	if instance == "" {
		instance = "local" // execEnv's own default: the directory the run wrote to
	}
	base := argus.OutputsBase(filepath.Join(cfg.ResultsRoot, instance))
	return func(p federation.ResultsPush) {
		// A compare push is one that carries outputs OR whose run stored files here: the outputs are omitted
		// whole when the set is oversize or one the reader refuses, and retention must still run then. A push
		// of any other mode has neither, so nothing of ours is pruned.
		if len(p.Outputs) == 0 && p.OutputsRoot == "" && !storedRunExists(base, p.RunID) {
			return
		}
		_, _ = argus.PruneOutputs(base, argus.KeepNewestOutputRuns, p.RunID, remove)
	}
}
