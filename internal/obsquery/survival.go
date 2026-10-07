package obsquery

import (
	"fmt"
	"time"
)

// AC-11 — THE SURVIVAL PLANE (the Prometheus half; obsquery is Loki-first for everything else).
//
// A load scenario can pass every assertion while the SUT's own pod is in distress — throttled or
// restarting under the load. Loki tells us what the SUT SAID; this tells us what the SUT'S OWN
// INFRASTRUCTURE did. Best-effort, exactly like every other obsquery read (VR-C6): a Prometheus
// that cannot be reached answers Available:false and never blocks or fails a run.
//
// WINDOWED DELTA (F7 / AC-11): a pod that was ALREADY throttling before the run started is not
// "degraded BY the run" — the read compares the signal's movement DURING the run window against
// the SAME signal's movement over an immediately-preceding baseline window of equal length, and
// only a positive EXCESS (never negative — a quieter run is not evidence of anything) counts.

// PromQuery is the minimal Prometheus read the survival plane needs: the most recent value of a
// query AT a point in time (an instant query). Swappable in tests; PrometheusClient (prometheus.go)
// is the real HTTP implementation.
type PromQuery interface {
	InstantValue(query string, at time.Time) (value float64, ok bool, err error)
}

// SurvivalWindow is the scenario's own run window and the baseline window to compare it against.
type SurvivalWindow struct {
	RunStart, RunEnd           time.Time
	BaselineStart, BaselineEnd time.Time
}

// SurvivalRead is the Prometheus-side verdict for one SUT pod during a load scenario.
type SurvivalRead struct {
	Available bool // false = Prometheus unreachable/unconfigured — best-effort, never blocks the run
	Degraded  bool
	Reason    string
}

// DefaultThrottleQuery / DefaultRestartQuery are the bundled cAdvisor / kube-state-metrics families
// for a throttled or restarting pod — the two signals a struggling SUT pod exposes on a k8s tier.
// %s is the pod-label selector (RO-07's SUTProject, the same value the compose/k8s tier already
// scrapes the SUT under).
const (
	DefaultThrottleQuery = `sum(increase(container_cpu_cfs_throttled_periods_total{pod=~"%s"}[1m]))`
	DefaultRestartQuery  = `sum(increase(kube_pod_container_status_restarts_total{pod=~"%s"}[1m]))`
)

// SurvivalPlaneRead reads the SUT pod's throttling + restart signals over the scenario's run
// window and compares each against its own baseline-window delta (windowed delta). DEGRADED on
// either signal moving MORE during the run than during the baseline — throttling is checked first
// (the more common, less disruptive signal); a restart is reported when throttling did not fire.
//
// ⛔ NEVER SWALLOWS ANYTHING ELSE: this is called by the caller ONLY on a scenario that already
// PASSED its own assertions (internal/argus wires it that way) — the existing throttled-`errored`
// (HTTP 429) case and a Rate Limiting scenario's legitimate `passed` never reach here changed,
// because neither of them is what this function decides; it only ever turns a `passed` INTO
// `degraded`, and only when q reports real movement.
func SurvivalPlaneRead(q PromQuery, podSelector string, w SurvivalWindow) SurvivalRead {
	if q == nil || podSelector == "" {
		return SurvivalRead{}
	}
	throttleDelta, tOK := windowedDelta(q, fmt.Sprintf(DefaultThrottleQuery, podSelector), w)
	restartDelta, rOK := windowedDelta(q, fmt.Sprintf(DefaultRestartQuery, podSelector), w)
	if !tOK && !rOK {
		return SurvivalRead{Available: false}
	}
	switch {
	case throttleDelta > 0:
		return SurvivalRead{Available: true, Degraded: true, Reason: fmt.Sprintf(
			"pod %q throttled during the run window (+%.0f CFS-throttled periods vs baseline)", podSelector, throttleDelta)}
	case restartDelta > 0:
		return SurvivalRead{Available: true, Degraded: true, Reason: fmt.Sprintf(
			"pod %q restarted during the run window (+%.0f restarts vs baseline)", podSelector, restartDelta)}
	default:
		return SurvivalRead{Available: true, Degraded: false}
	}
}

// windowedDelta answers "how much MORE did this signal move during the run window than during the
// baseline window" — clamped at 0 (a baseline that moved more than the run is not the run's fault).
// ok is false only when the RUN read itself failed/was unavailable; a failed baseline read degrades
// to "no prior activity" (0) rather than making the whole comparison unavailable — a fresh instance
// with no baseline history is the common case, not a fault.
func windowedDelta(q PromQuery, query string, w SurvivalWindow) (float64, bool) {
	runV, ok, err := q.InstantValue(query, w.RunEnd)
	if err != nil || !ok {
		return 0, false
	}
	baseV, bok, berr := q.InstantValue(query, w.BaselineEnd)
	if berr != nil || !bok {
		baseV = 0
	}
	d := runV - baseV
	if d < 0 {
		d = 0
	}
	return d, true
}
