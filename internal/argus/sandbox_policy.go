package argus

import (
	"sort"
	"time"

	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/obsquery"
	"github.com/OneDro1d/argus-runner/internal/report"
)

// ── Spec 26, A1 (observe only): the sandbox evidence pass ─────────────────────────────────────────
//
// When argus-config declares observability.openshell, every scenario row gets a sandbox_policy block:
// what the SUT agent's OpenShell sandbox denied during that scenario's padded window.
//
// ⛔ IT NEVER TOUCHES THE VERDICT. Nothing here reads or writes a row's Status or Failure; the block
// is attached and never consulted (TestRunAll_SandboxPolicyNeverChangesVerdict). The opt-in verdict
// (`sandbox-policy-clean`) is spec 26 P2, not this.
//
// ONE READ PASS AFTER THE LOOP (, finding A4): each window is [start - pad, end + pad]; the run
// waits once until the last window's end + pad (ingestion settle), then reads every window. Reading
// after each scenario would cost a pad per scenario.

// sandboxPolicySleepUntil is the one wait before the read. A package variable so tests replace it.
// It takes no context; what bounds it is the load-time cap on window_pad
// (config.MaxOpenShellWindowPad), which keeps the wait under two pads.
var sandboxPolicySleepUntil = func(t time.Time) {
	if d := time.Until(t); d > 0 {
		time.Sleep(d)
	}
}

// The reasons a row's block is `unavailable` without a read.
const (
	sandboxPolicyNoReader     = "no evidence reader was wired for this run, so the sandbox's events were not read"
	sandboxPolicyNoWindow     = "the scenario has no recorded window, so no event can be attributed to it"
	sandboxPolicyShortCircuit = "the run short-circuited before any scenario ran (executor preflight), so there is no window to read"
)

// unavailableSandboxPolicy is a block that says why nothing was read. window may be nil. DeniedCount
// stays nil ("denied_count":null) and events [], as on every unavailable block.
func unavailableSandboxPolicy(o *config.OpenShellObs, window *report.SandboxPolicyWindow, reason string) *report.SandboxPolicy {
	return &report.SandboxPolicy{
		Source: o.Source, Sandbox: obsquery.SafeSandboxID(o.Sandbox), Window: window,
		Coverage: report.CoverageUnavailable, CoverageReason: reason,
		Events: []report.SandboxPolicyEvent{},
	}
}

// applySandboxPolicy attaches a block to every row when the block is declared, and does nothing at
// all — no read, no wait — when it is not.
func applySandboxPolicy(c *config.Config, byLayer map[string][]report.ScenarioResult, ev obsquery.SandboxEvidence) {
	o := c.OpenShellEvidence()
	if o == nil {
		return
	}
	pad := o.Pad()
	type pending struct {
		res *report.ScenarioResult
		w   obsquery.ScenarioWindow
	}
	var (
		todo    []pending
		windows []obsquery.ScenarioWindow
		lastTo  time.Time
	)
	layers := make([]string, 0, len(byLayer))
	for l := range byLayer {
		layers = append(layers, l)
	}
	sort.Strings(layers) // a fixed read order: the same run reads Loki the same way every time
	for _, l := range layers {
		scs := byLayer[l]
		for i := range scs {
			res := &scs[i]
			if res.WindowStart.IsZero() || res.WindowEnd.IsZero() {
				res.SandboxPolicy = unavailableSandboxPolicy(o, nil, sandboxPolicyNoWindow)
				continue
			}
			w := obsquery.ScenarioWindow{ID: res.ID, From: res.WindowStart.Add(-pad), To: res.WindowEnd.Add(pad)}
			if ev == nil {
				res.SandboxPolicy = unavailableSandboxPolicy(o, &report.SandboxPolicyWindow{
					From: w.From.UTC().Format(obsquery.RFC3339Milli), To: w.To.UTC().Format(obsquery.RFC3339Milli),
				}, sandboxPolicyNoReader)
				continue
			}
			todo = append(todo, pending{res: res, w: w})
			windows = append(windows, w)
			if w.To.After(lastTo) {
				lastTo = w.To
			}
		}
	}
	if len(todo) == 0 {
		return
	}
	sandboxPolicySleepUntil(lastTo.Add(pad))
	for _, p := range todo {
		read := ev.SandboxLines(o.Selector, o.Sandbox, p.w.From, p.w.To.Add(pad))
		p.res.SandboxPolicy = obsquery.SandboxPolicyFor(read, o.Sandbox, p.w, windows)
	}
}

// markSandboxPolicyShortCircuit gives every row of a short-circuited run (RO-04: the executor
// preflight failed and no scenario ran) an `unavailable` block, so "block on ⇒ every row carries one"
// holds on that path too.
func markSandboxPolicyShortCircuit(c *config.Config, rep *report.Report) {
	o := c.OpenShellEvidence()
	if o == nil || rep == nil {
		return
	}
	for i := range rep.Layers {
		for j := range rep.Layers[i].Scenarios {
			rep.Layers[i].Scenarios[j].SandboxPolicy = unavailableSandboxPolicy(o, nil, sandboxPolicyShortCircuit)
		}
	}
}
