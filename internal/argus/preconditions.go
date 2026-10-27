package argus

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/precondition"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// The run's half of the precondition tier. internal/precondition probes; this file decides,
// BEFORE the first check fires, what every dependent check does:
//
//	holds   -> the check runs normally
//	absent  -> NOT executed: status not_applicable, carrying the precondition id and both observations
//	broken  -> NOT executed: status error (the instrument is broken). NEVER not_applicable: a broken probe
//	           that excused checks would be a way to skip any check by breaking the probe.
//
// A run whose checks declare no `Depends on` constructs no prober and touches no cluster.

// newPreconditionKube is a package var ONLY so tests can put a fake behind the whole run path (like
// newFaultKube); production never reassigns it. It returns the client and the executor's OWN namespace.
var newPreconditionKube = precondition.DefaultKube

// runPrecondition is a package var ONLY so a run-level test can fix a probe's verdict; production never
// reassigns it. (internal/precondition's own tests drive the probe itself against a fake cluster.)
var runPrecondition = func(ctx context.Context, pr *precondition.Prober, p scenario.Precondition) precondition.Result {
	return pr.Probe(ctx, p)
}

// preconditionPlan is what a run knows about its preconditions once they have been probed.
type preconditionPlan struct {
	results   map[string]precondition.Result
	conflicts map[string]string
	// problems are the scenarios whose own declarations are unusable (the second door), by scenario id.
	problems map[string][]string
	ordered  []report.Precondition
}

func toReport(r precondition.Result) report.Precondition {
	return report.Precondition{ID: r.ID, Kind: r.Kind, Verdict: r.Verdict, Positive: r.Positive, Negative: r.Negative, Residue: r.Residue}
}

// planPreconditions probes every precondition at least one selected scenario depends on, once each. nil when
// no selected scenario depends on anything.
func planPreconditions(ctx context.Context, c *config.Config, scns []scenarioFile) *preconditionPlan {
	needed := map[string]bool{}
	var decls [][]scenario.Precondition
	plan := &preconditionPlan{results: map[string]precondition.Result{}, problems: map[string][]string{}}
	for _, sf := range scns {
		s := sf.s
		if len(s.DependsOn) == 0 && len(s.Preconditions) == 0 {
			continue
		}
		// The second door: a file can reach a run without passing the validator.
		if probs := scenario.PreconditionProblems(s.Preconditions, s.DependsOn, s.PreconditionsDeclared, s.DependsOnDeclared); len(probs) > 0 {
			plan.problems[s.ID] = probs
			continue
		}
		decls = append(decls, s.Preconditions)
		for _, d := range s.DependsOn {
			needed[d] = true
		}
	}
	if len(needed) == 0 && len(plan.problems) == 0 {
		return nil
	}
	all, conflicts := precondition.Collect(decls)
	plan.conflicts = conflicts
	var todo []scenario.Precondition
	for _, p := range all {
		if needed[p.ID] && conflicts[p.ID] == "" {
			todo = append(todo, p)
		}
	}
	if len(todo) == 0 {
		return plan
	}
	pr := &precondition.Prober{}
	kube, ns, kerr := newPreconditionKube()
	if kerr == nil {
		set, serr := c.PreconditionProbeSettings()
		if serr != nil {
			kerr = serr
		} else {
			pr = &precondition.Prober{Kube: kube, NS: ns, Cfg: precondition.Config{
				Image: set.Image, ReadyTimeout: set.ReadyTimeout, ConnectTimeout: set.ConnectTimeout, Attempts: set.Attempts, Settle: set.Settle}}
		}
	}
	for _, p := range todo {
		var r precondition.Result
		if kerr != nil {
			r = precondition.Result{ID: p.ID, Kind: p.Kind, Verdict: precondition.Broken,
				Positive: "not run: " + kerr.Error(), Negative: "not run"}
		} else {
			r = runPrecondition(ctx, pr, p)
		}
		plan.results[p.ID] = r
		plan.ordered = append(plan.ordered, toReport(r))
	}
	sort.Slice(plan.ordered, func(i, j int) bool { return plan.ordered[i].ID < plan.ordered[j].ID })
	return plan
}

// rowFor answers, for one scenario, whether it must NOT be executed and the row it ends with. (_, false) = run it.
func (p *preconditionPlan) rowFor(sf scenarioFile, corr string) (report.ScenarioResult, bool) {
	if p == nil {
		return report.ScenarioResult{}, false
	}
	s := sf.s
	errRow := func(msg string) (report.ScenarioResult, bool) {
		return refusedBeforeFiring(report.ScenarioResult{ID: s.ID, CorrelationID: corr, Status: report.StatusError,
			Failure: &report.Failure{Observed: msg}}), true
	}
	if probs := p.problems[s.ID]; len(probs) > 0 {
		return errRow("refused before firing: its precondition declaration is unusable: " + strings.Join(probs, "; "))
	}
	var absent *precondition.Result
	for _, id := range s.DependsOn {
		if why := p.conflicts[id]; why != "" {
			return errRow("refused before firing: " + why)
		}
		r, ok := p.results[id]
		if !ok { // not probed: nothing may be assumed about it
			return errRow(fmt.Sprintf("refused before firing: precondition %q was not probed", id))
		}
		switch r.Verdict {
		case precondition.Broken:
			return errRow("not executed: the instrument is broken - " + r.Reason())
		case precondition.Absent:
			if absent == nil {
				rr := r
				absent = &rr
			}
		}
	}
	if absent == nil {
		return report.ScenarioResult{}, false
	}
	na := &report.NotApplicable{Precondition: absent.ID, Kind: absent.Kind, Positive: absent.Positive, Negative: absent.Negative}
	return refusedBeforeFiring(report.ScenarioResult{ID: s.ID, CorrelationID: corr, Status: report.StatusNotApplicable,
		NotApplicable: na, Failure: &report.Failure{Observed: na.Line()}}), true
}
