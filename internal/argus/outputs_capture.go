package argus

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/OneDro1d/argus-runner/internal/amqpengine"
	"github.com/OneDro1d/argus-runner/internal/chain"
	"github.com/OneDro1d/argus-runner/internal/compare"
	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/obsquery"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// ARGUS-CMP-3 (/-05): recording the output of a check, in run mode `compare` and nowhere
// else. ONE predicate decides it (newOutputCtx: the mode; outputCtx.records: the check declares
// `## COMPARE`), and every path that records goes through it, so a build, final, scheduled or rehearsal
// run is byte-identical to before.

// executorCompareFloor is the newest `## COMPARE` key set this executor understands (compare.KeyFloors): E2 since
// ARGUS-CMP-11 (tolerance values, load numbers, the member's target). A check that needs a newer one is refused
// before firing, never run with a rule ignored (design 11.2).
//
// It is a variable only so a test can stand in for the release before: an E1 executor handed an E2 key (design 11.2 item 4).
var executorCompareFloor = compare.FloorE2

// CompareRefusal prefixes the observed text of a check the executor refused in mode compare. It is the
// wording of the S9 door (AMQP Load), and it names a key and a line, never a response.
const CompareRefusal = "refused before firing: "

// jmeterCaptureTemplates are the JMeter templates whose JSR223 block writes the capture side files.
// It IS compare.CaptureTemplateBases, the list the control plane's "is this check compared" predicate reads.
var jmeterCaptureTemplates = compare.CaptureTemplateBases

// RecordsComparedOutput says whether this executor, in run mode `compare`, records an output for the check:
// the facts of the check fed to compare.RecordsOutput (see internal/compare/records.go for the dispatch it
// mirrors). The control plane calls it to tell the author which checks of a set cannot be compared.
func RecordsComparedOutput(s *scenario.Scenario) bool {
	if s == nil {
		return false
	}
	f := compare.RecorderFacts{
		AMQPLoad:     PrimaryLayer(s) == scenario.AMQPLoadLayer,
		Chain:        contains(s.Tags, ChainTag),
		MCP:          contains(s.Tags, MCPTag),
		UI:           contains(s.Tags, UITag),
		Load:         s.Load != nil || PrimaryLayer(s) == scenario.HTTPLoadLayer, // a numbers-only ramp, like a `## LOAD` check
		TemplateBase: RuntimeTemplateBase(s),
	}
	if f.Chain {
		if steps, err := scenario.ParseChainSteps(s.Trigger.Payload); err == nil {
			for _, st := range steps {
				if st.Type == "http" {
					f.ChainHasHTTPStep = true
				}
			}
		}
	}
	return compare.RecordsOutput(f)
}

// rawCaptureFile matches the only files the capture block writes: <n>.status, <n>.headers, <n>.body.
var rawCaptureFile = regexp.MustCompile(`^[0-9]+\.(status|headers|body)$`)

// captureDirMapper is implemented by a Runner whose JMeter does not see the executor's filesystem the way
// the executor does (DockerRunner: the results dir is bind-mounted at /results).
type captureDirMapper interface {
	JMeterCapturePath(hostPath string) string
}

// outputCtx is one run's recording state. nil means "this run records nothing".
type outputCtx struct {
	runID       string
	storeBase   string // <resultsDir>/outputs  (the stored files, 0700)
	captureRoot string // <resultsDir>/capture  (scratch for JMeter's raw side files, deleted after each check)
	jmeterPath  func(hostDir string) string
	remove      func(string) error // deletes one stale stored file of an earlier attempt (os.Remove when nil); a test injects a fake
}

// newOutputCtx is the ONE mode check. Only mode compare records.
func newOutputCtx(mode, resultsDir, runID string, r Runner) *outputCtx {
	if mode != report.ModeCompare {
		return nil
	}
	oc := &outputCtx{runID: runID, storeBase: OutputsBase(resultsDir), captureRoot: filepath.Join(resultsDir, "capture")}
	if m, ok := r.(captureDirMapper); ok {
		oc.jmeterPath = m.JMeterCapturePath
	}
	return oc
}

// records: this run records, and this check declares a valid `## COMPARE`.
func (oc *outputCtx) records(s *scenario.Scenario) bool {
	return oc != nil && s != nil && s.Compare != nil
}

// credentialScrub is the scrub of design 1.5 steps 2 and 3: every credential the config resolved, in
// plain, URL-encoded and Base64 forms (amqpengine.RedactSecrets), then token-shaped runs
// (obsquery.RedactTokens). Step 1, the saved variables, is applied by the engine that holds them.
func credentialScrub(c *config.Config) func(string) string {
	secrets := c.CredentialValues()
	return func(s string) string { return obsquery.RedactTokens(amqpengine.RedactSecrets(s, secrets...)) }
}

// floorRank orders the executor releases of compare.KeyFloors. An unknown floor ranks above every known
// one, so a future key is refused by an executor that has never heard of it.
func floorRank(f string) int {
	switch f {
	case compare.FloorE1:
		return 1
	case compare.FloorE2:
		return 2
	}
	return 1 << 20
}

// compareRefusal is the door in front of a check whose `## COMPARE` this executor cannot honour: a
// problem in the section (a key it does not know included), or a key from a newer release. nil = go on.
func compareRefusal(oc *outputCtx, s *scenario.Scenario, corr string) *report.ScenarioResult {
	if oc == nil || !s.CompareDeclared {
		return nil
	}
	why := ""
	if probs := s.CompareProblems(); len(probs) > 0 {
		p := probs[0]
		switch {
		case p.UnknownKey:
			why = fmt.Sprintf("this executor does not know COMPARE key %q (line %d)", p.Key, p.Line)
		case p.Key != "":
			why = fmt.Sprintf("its ## COMPARE section has a problem at key %q (line %d)", p.Key, p.Line)
		default:
			why = fmt.Sprintf("its ## COMPARE section has a problem at line %d", p.Line)
		}
	} else if s.Compare != nil && floorRank(s.Compare.Floor()) > floorRank(executorCompareFloor) {
		key := "Tolerance"
		if len(s.Compare.Tolerance) == 0 {
			key = "Not Worse Than"
		}
		why = fmt.Sprintf("this executor does not know COMPARE key %q", key)
	}
	if why == "" {
		return nil
	}
	return &report.ScenarioResult{ID: s.ID, CorrelationID: corr, Status: report.StatusError,
		Failure: &report.Failure{Observed: CompareRefusal + why}}
}

// store writes one stored file; a record with no stored form (not_recorded) writes nothing. A write that
// fails leaves the digest in the report and no body to drill into (get_output then answers "no recorded
// output"); it is never fatal. It is warned ONCE, naming run id, scenario id, step and sample and a closed
// reason: never a response byte, a header value, a saved value or a filesystem path.
func (oc *outputCtx) store(scenarioID string, rec compare.OutputRecord, st *compare.Stored) {
	if oc == nil || st == nil {
		return
	}
	if err := WriteOutputFile(oc.storeBase, oc.runID, scenarioID, rec.Step, rec.Sample, st.File()); err != nil {
		slog.Warn("compare: the recorded output was not stored; get_output will answer that there is no recorded output",
			"run_id", oc.runID, "scenario_id", scenarioID, "step", rec.Step, "sample", rec.Sample, "reason", storeFailureReason(err))
	}
}

// storeFailureReason is the closed wording for why a stored file could not be written.
func storeFailureReason(err error) string {
	switch {
	case errors.Is(err, ErrOutputNameTooLong):
		return "the check id and step make a file name that is too long"
	case errors.Is(err, ErrBadOutputRef):
		return "the run id, check id or step cannot be used in a file name"
	default:
		return "the outputs directory could not be written"
	}
}

// clearCheck removes the stored files an EARLIER attempt of this check left in THIS run's directory, before
// the next attempt writes its own: a re-fire that records fewer samples (or steps) must not leave the
// earlier attempt's higher-numbered files readable under the same run. It is a no-op when nothing records.
func (oc *outputCtx) clearCheck(scenarioID string) {
	if oc == nil {
		return
	}
	if err := RemoveCheckOutputs(oc.storeBase, oc.runID, scenarioID, oc.remove); err != nil {
		slog.Warn("compare: the stored outputs of an earlier attempt could not be removed",
			"run_id", oc.runID, "scenario_id", scenarioID, "reason", "an earlier attempt's file could not be removed")
	}
}

// notSupported is the record of a check on a layer that records nothing yet: never a hash, so the cell
// can only say "not measured".
func (oc *outputCtx) notSupported(s *scenario.Scenario) []compare.OutputRecord {
	if !oc.records(s) {
		return nil
	}
	return []compare.OutputRecord{{V: compare.RecordVersion, Sample: 1, State: compare.StateNotRecorded, Reason: compare.ReasonLayerNotSupported}}
}

// loadRecords is the ONE record of a `## LOAD` check that declares `**Not Worse Than**` (ARGUS-CMP-11,
// ): the numbers report.LoadStats already computed from the .jtl, under compare.LoadRecord's shape
// (`not_recorded` / `load_numbers_only`, no status, no hash). Only in mode `compare`, only for a check whose rules ask for
// a band: a load check without one records nothing, as before, and so does every other mode.
func (oc *outputCtx) loadRecords(s *scenario.Scenario, ls *report.LoadStats) []compare.OutputRecord {
	if !oc.records(s) || ls == nil || len(s.Compare.NotWorseThan) == 0 {
		return nil
	}
	return []compare.OutputRecord{compare.LoadRecord(compare.LoadNumbers{
		Samples: ls.Samples, P50Ms: float64(ls.P50Ms), P95Ms: float64(ls.P95Ms), P99Ms: float64(ls.P99Ms), ErrorRate: ls.ErrorRate,
	})}
}

// withNotSupported attaches notSupported to a result of an engine that records nothing (mcp, ui).
func (oc *outputCtx) withNotSupported(s *scenario.Scenario, res report.ScenarioResult) report.ScenarioResult {
	if recs := oc.notSupported(s); recs != nil {
		res.Outputs = recs
	}
	return res
}

// ── chain http steps ──────────────────────────────────────────────────────────────────────────

// comparesStep is the `Steps` rule: the named steps, or by default every http step that is not `always`.
func comparesStep(rules *compare.Rules, st chainStepSpec) bool {
	if len(rules.Steps) > 0 {
		return contains(rules.Steps, st.Name)
	}
	return !st.Always
}

// chainCapture is the OutputCapture for one chain http step, or nil when the step is not compared.
func (oc *outputCtx) chainCapture(c *config.Config, s *scenario.Scenario, st chainStepSpec) *chain.OutputCapture {
	if !oc.records(s) || !comparesStep(s.Compare, st) {
		return nil
	}
	return &chain.OutputCapture{Spec: s.Compare.Spec(), Scrub: credentialScrub(c)}
}

// attachChainOutputs moves what the http steps recorded into the row and onto disk, and drops the
// in-memory bodies. A chain with no http step at all records "not supported" (it has nothing to compare).
func (oc *outputCtx) attachChainOutputs(s *scenario.Scenario, res *report.ScenarioResult, hasHTTPStep bool) {
	if !oc.records(s) {
		return
	}
	oc.clearCheck(s.ID) // an earlier attempt of this check (a re-fire) must not leave its files behind
	for i := range res.Steps {
		if o := res.Steps[i].Output; o != nil {
			res.Outputs = append(res.Outputs, o.Record)
			oc.store(s.ID, o.Record, o.Stored)
			res.Steps[i].Output = nil
		}
	}
	if !hasHTTPStep {
		res.Outputs = oc.notSupported(s)
	}
}

// ── JMeter http templates ─────────────────────────────────────────────────────────────────────

// applyCaptureProp sets `output.capture.dir`: only in mode compare, only for a check with `## COMPARE`,
// and never when the check declares `## LOAD` (a load run is numbers only, and one capture file per
// sample of thousands is not what the contract asks for).
func applyCaptureProp(props map[string]string, s *scenario.Scenario, mode, dir string) {
	if mode != report.ModeCompare || dir == "" || s == nil || s.Compare == nil || s.Load != nil {
		return
	}
	props["output.capture.dir"] = dir
}

// DerivePropsWithCapture is DeriveProps plus the `output.capture.dir` property (see applyCaptureProp).
// dir is the directory as JMeter sees it.
func DerivePropsWithCapture(c *config.Config, s *scenario.Scenario, corr, mode, dir string) (map[string]string, error) {
	props, err := DeriveProps(c, s, corr)
	if err != nil {
		return nil, err
	}
	applyCaptureProp(props, s, mode, dir)
	return props, nil
}

// jmeterCapture is the scratch directory of one JMeter check. The zero/nil value does nothing.
type jmeterCapture struct {
	oc      *outputCtx
	hostDir string // where the executor reads and deletes
	jmDir   string // where JMeter writes (the same place, or the container's view of it)
}

// beginJMeterCapture prepares the scratch directory and sets the property, for a check that records
// through an http template. For any other layer, or a load check, it does nothing and the caller attaches
// notSupported (or nothing).
func (oc *outputCtx) beginJMeterCapture(s *scenario.Scenario, base, mode string, props map[string]string) *jmeterCapture {
	if !oc.records(s) || s.Load != nil || !jmeterCaptureTemplates[base] {
		return nil
	}
	if err := os.MkdirAll(oc.captureRoot, 0o700); err != nil {
		return nil
	}
	dir, err := os.MkdirTemp(oc.captureRoot, "c-")
	if err != nil {
		return nil
	}
	cp := &jmeterCapture{oc: oc, hostDir: dir, jmDir: dir}
	if oc.jmeterPath != nil {
		cp.jmDir = oc.jmeterPath(dir)
	}
	applyCaptureProp(props, s, mode, cp.jmDir)
	return cp
}

// cleanup deletes the raw side files and the directory we made, on every path. It removes only files
// that match the capture block's own names, individually, and the directory non-recursively: nothing
// else in it, and nothing outside it, can be touched.
func (cp *jmeterCapture) cleanup() {
	if cp == nil || cp.hostDir == "" || filepath.Dir(cp.hostDir) != cp.oc.captureRoot {
		return
	}
	if ents, err := os.ReadDir(cp.hostDir); err == nil {
		for _, e := range ents {
			if e.Type().IsRegular() && rawCaptureFile.MatchString(e.Name()) {
				_ = os.Remove(filepath.Join(cp.hostDir, e.Name()))
			}
		}
	}
	_ = os.Remove(cp.hostDir)
	_ = os.Remove(cp.oc.captureRoot) // only succeeds when empty
}

// collect reads the side files, builds one record per sample with the SAME compare.BuildRecord the chain
// uses, stores the files, and deletes the raw ones (also deferred by the caller). At most
// compare.MaxSamples samples.
func (cp *jmeterCapture) collect(c *config.Config, s *scenario.Scenario) []compare.OutputRecord {
	if cp == nil {
		return nil
	}
	defer cp.cleanup()
	cp.oc.clearCheck(s.ID) // an earlier attempt of this check (a re-fire) must not leave its files behind
	spec := s.Compare.Spec()
	spec.Scrub = credentialScrub(c)
	var rows []compare.OutputRecord
	for n := 1; n <= compare.MaxSamples; n++ {
		statusRaw, err := os.ReadFile(filepath.Join(cp.hostDir, strconv.Itoa(n)+".status"))
		if err != nil {
			break // no more samples
		}
		status, _ := strconv.Atoi(strings.TrimSpace(string(statusRaw)))
		resp := compare.Response{Status: status}
		if hb, err := readCapped(filepath.Join(cp.hostDir, strconv.Itoa(n)+".headers"), 64<<10); err == nil {
			resp.Headers = parseJMeterHeaders(string(hb))
		}
		if bb, err := readCapped(filepath.Join(cp.hostDir, strconv.Itoa(n)+".body"), compare.MaxBodyBytes+1); err == nil {
			resp.Body = bb
		}
		rec, st := compare.BuildRecord(resp, spec, "", n)
		cp.oc.store(s.ID, rec, st)
		rows = append(rows, rec)
	}
	return rows
}

func readCapped(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, limit))
}

// parseJMeterHeaders reads SampleResult.getResponseHeaders(): a status line, then `Name: value` lines.
func parseJMeterHeaders(raw string) map[string][]string {
	out := map[string][]string{}
	for i, line := range strings.Split(raw, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" || (i == 0 && strings.HasPrefix(line, "HTTP/")) {
			continue
		}
		if k := strings.IndexByte(line, ':'); k > 0 {
			name := strings.TrimSpace(line[:k])
			out[name] = append(out[name], strings.TrimSpace(line[k+1:]))
		}
	}
	return out
}
