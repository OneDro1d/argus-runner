package argus

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"time"

	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/fault"
	"github.com/OneDro1d/argus-runner/internal/obsquery"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// The run's half of the `fault` chain step. internal/fault applies and undoes; this file
// is the door (allowlist, journal, shape) and the guarantee that Finish runs on every path.

// faultEnv is where a run keeps its fault state. A zero value (a caller with no results volume) means a
// chain carrying a fault step is REFUSED: a fault whose undo cannot be made durable is never applied.
type faultEnv struct {
	// JournalDir is the undo journal: <results>/fault-journal, on the shared results volume.
	JournalDir string
	// Logs is the executor's own Loki for a chain's `logs` step, carried here because this
	// struct is already how a run hands a chain what its non-http steps need. nil = this executor has no Loki.
	Logs obsquery.LogsReader
}

// FaultJournalDir is where a run whose results go to resultsDir keeps its undo journal. The executor's
// startup replay (internal/runner) reads the same directory, so the two cannot disagree.
func FaultJournalDir(resultsDir string) string { return filepath.Join(resultsDir, "fault-journal") }

// newFaultKube and newFaultToxi are package vars ONLY so tests can put fakes behind the whole chain path
// (like dialAMQPBroker); production never reassigns them.
var (
	newFaultKube = fault.DefaultKube
	newFaultToxi = fault.DefaultToxi
)

// faultTargets adapts the operator's allowlist to fault.Targets.
type faultTargets struct{ c *config.Config }

func (t faultTargets) Deployment(n string) (string, bool)   { return t.c.FaultDeployment(n) }
func (t faultTargets) Link(n string) (string, string, bool) { return t.c.FaultLink(n) }

// faultSpecOf turns a (shape-validated) fault step into the manager's Spec.
func faultSpecOf(st chainStepSpec) fault.Spec {
	sp := fault.Spec{Step: st.Name, Op: st.Op, Deployment: st.Deployment, Link: st.Link, Replicas: st.Replicas}
	if st.Timeout != "" {
		sp.Timeout, _ = time.ParseDuration(st.Timeout)
	}
	if st.LatencyMs != nil {
		sp.LatencyMs = *st.LatencyMs
	}
	if st.JitterMs != nil {
		sp.JitterMs = *st.JitterMs
	}
	if st.Bytes != nil {
		sp.Bytes = *st.Bytes
	}
	return sp
}

// faultPreflight refuses a whole chain that carries a fault step it may not run, BEFORE its first step
// fires - so a later bad step cannot leave an earlier fault applied. It is the allowlist door; the
// manager checks again when it applies. The reason names the entry, never an address. "" = fine.
func faultPreflight(c *config.Config, s *scenario.Scenario, specs []chainStepSpec, claims map[string][]string, fe faultEnv) (step, why string) {
	raws := scenarioRawSteps(s)
	first := ""
	for i, st := range specs {
		if st.Type != "fault" {
			continue
		}
		if first == "" {
			first = st.Name
		}
		var raw map[string]json.RawMessage
		if i < len(raws) {
			raw = raws[i]
		}
		if probs := scenario.FaultStepProblems(st, raw); len(probs) > 0 {
			return st.Name, strings.Join(probs, "; ")
		}
		if len(claims[st.Name]) > 0 {
			return st.Name, "a fault step takes no claim (put the claim on the step that observes the effect)"
		}
	}
	if first == "" {
		return "", ""
	}
	if !c.FaultEnabled() {
		return first, "fault steps are off: argus-config.yaml declares no fault_allowed_targets (top level, beside targets:) - nothing was applied"
	}
	if fe.JournalDir == "" {
		return first, "this run has no undo journal (a fault step needs the executor's results volume to keep the undo durable) - nothing was applied"
	}
	for _, st := range specs {
		if st.Type != "fault" {
			continue
		}
		switch st.Op {
		case fault.OpRestore:
			// names no target
		case fault.OpDelay, fault.OpDrop, fault.OpTruncate:
			if _, _, ok := c.FaultLink(st.Link); !ok {
				return st.Name, fmt.Sprintf("link %q is not listed under fault_allowed_targets.links in argus-config.yaml - nothing was applied", st.Link)
			}
		default:
			if _, ok := c.FaultDeployment(st.Deployment); !ok {
				return st.Name, fmt.Sprintf("deployment %q is not listed under fault_allowed_targets.deployments in argus-config.yaml - nothing was applied", st.Deployment)
			}
		}
	}
	return "", ""
}

func scenarioRawSteps(s *scenario.Scenario) []map[string]json.RawMessage {
	var doc struct {
		Steps []map[string]json.RawMessage `json:"steps"`
	}
	if json.Unmarshal([]byte(s.Trigger.Payload), &doc) != nil {
		return nil
	}
	return doc.Steps
}

func newFaultManager(c *config.Config, corr string, fe faultEnv) *fault.Manager {
	return fault.New(fault.Config{
		Run: corr, Journal: &fault.Journal{Dir: fe.JournalDir}, Targets: faultTargets{c}, Owner: fault.Hostname(),
		Kube: newFaultKube, Toxi: newFaultToxi,
	})
}

// finishFaults is the END-OF-CHAIN restore: it undoes whatever is still active and attaches the record to
// the row. ⛔ It NEVER touches res.Status or res.Failure (cleanup rule 2): a red restore cannot fail a
// passing scenario and a green one cannot rescue a failed scenario. A failed restore is LOUD instead: it is
// on the record (restore_outcome "failed"), on the row's residue line (shown on the terminal), and in the
// executor log; the undo stays journaled so the next executor start retries it.
func finishFaults(mgr *fault.Manager, res *report.ScenarioResult, corr string) {
	errs := mgr.Finish(context.Background())
	res.Faults = mgr.Records()
	if len(errs) == 0 {
		return
	}
	var failed []string
	for _, f := range res.Faults {
		if f.RestoreOutcome == report.FaultRestoreFailed {
			failed = append(failed, f.Op+" of "+f.Target)
		}
	}
	line := "FAULT NOT RESTORED: " + strings.Join(failed, ", ") + " could not be undone - the system under test may still be broken; " +
		"the undo stays journaled and is retried when the executor next starts"
	if res.Residue != "" {
		line = res.Residue + "; " + line
	}
	res.Residue = line
	log.Printf("FAULT NOT RESTORED (run %s): %d undo(s) failed: %v", corr, len(errs), errs)
}
