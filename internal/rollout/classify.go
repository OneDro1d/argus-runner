// Package rollout is the ONE shared classifier for turning a Deployment's four observed counts —
// desired, total, ready, updated — into a rollout verdict. AC-D51 / issue #315: the executor's
// autoscaler (internal/runner) and the control plane (internal/control) each carried their OWN copy of
// this decision, and the two disagreed in a way that mattered: an ordinary post-run scale-down (3 -> 1)
// left Kubernetes still tearing down the surplus pods for about a second, during which BOTH copies read
// `total > desired` and called a perfectly healthy instance "HALF-LANDED rollout — 0 pod(s) still run
// the PREVIOUS image" — a headline contradicted by its own count, on every park.
//
// It lives in its own leaf package, imported by both internal/runner and internal/control, neither of
// which imports the other in production code — so there is no cycle to create.
//
// ── THE RULE, FROM KUBERNETES' OWN SEMANTICS ──────────────────────────────────────────────────────
//
// status.replicas ("total") is every pod the Deployment owns that is not terminating, across EVERY ReplicaSet
// it has ever created — old ones included. A pod that is already terminating (it has a deletionTimestamp) is
// NOT counted (apps/v1 DeploymentStatus: "non-terminating pods"; from the Kubernetes sources, not measured live),
// so an old pod still draining is invisible to total - updated (AC-D53 round-5 review R5-K8S-1). The ~1 s park
// lag below reads, from the same sources, as the status catching up after the scale-down (reasoned, not
// measured). status.updatedReplicas
// ("updated") counts only the pods whose owning ReplicaSet matches the CURRENT pod template hash — and
// it counts a pod that is merely Pending, because a Pending pod still carries the desired template; it
// simply has not started (measured 2026-08-10: spec=1 total=2 ready=1 updated=1 — one Pending
// current-template pod behind one Running previous-template pod, both ready/updated saying "1" and
// agreeing with desired, which is exactly how it went unnoticed).
//
// total - updated is therefore EXACTLY the count of NON-TERMINATING pods that are NOT on the current
// template — the one number the status gives for "is an old version still out there", regardless of how
// many replicas were asked for (an old pod already terminating, still draining, is not in it):
//
//	park (3 -> 1):            total=3 updated=3  =>  total-updated = 0. total(3) > desired(1) is TRUE,
//	                          but every pod that exists already runs the current template — there is no
//	                          old pod, only a teardown the API server has not finished reporting yet.
//	the 2026-08-10 stall:     total=2 updated=1  =>  total-updated = 1. Half-landed, and it says so
//	                          regardless of what desired is doing.
//	scale-up in progress:     total=1 updated=1  =>  total-updated = 0. Not half-landed — it is a
//	                          readiness shortfall (ready < desired), a different axis entirely.
//
// This needs no comparison against desired at all, which is why it survives a scale up OR down cleanly
// where a `total > desired` or `updated < desired` test does not: both of those compare a COUNT OF PODS
// to a REQUEST, and a request in flight (up or down) routinely makes that comparison true for pods that
// are not wrong, only not-yet-settled. ("the current template has fewer pods than desired while old
// pods remain" is the same condition restated as updated < desired AND total > updated — but
// total > updated already means total - updated > 0, so it collapses to the one rule below; there is no
// second half-landed condition hiding in that phrasing.)
//
// ── READINESS IS A SEPARATE AXIS ──────────────────────────────────────────────────────────────────
//
// A pod can be on the current template and still not be Ready: crash-looping, failing its probe, held
// back by a ResourceQuota, or — under the no-surge strategy the executor Deployment renders with
// (k8srender.go: `rollingUpdate: {maxSurge: 0, maxUnavailable: 1}`) — simply not created yet, because
// the old pod had to be removed FIRST and the new one has not started (desired=1 total=1 ready=0
// updated=1: no old pod counted — one may still be draining — so not half-landed, but definitely short of
// ready). Folding readiness
// into half-landed (the control plane's old `ready < desired` clause) turned a quota or scheduling
// problem into "ROLLOUT STALLED — check the image is resolvable", the wrong remedy for what is actually
// happening. Classify reports the two separately so a caller can never conflate them.
package rollout

// State is the shared rollout verdict.
type State string

const (
	// Complete: every non-terminating pod runs the CURRENT pod template (an old pod may still be draining).
	// Says nothing about readiness — a Complete rollout can still be short of Ready; check
	// ReadinessShortfall for that.
	Complete State = "complete"
	// HalfLanded: at least one non-terminating pod does NOT run the current pod template
	// (total > updated). This is the only condition the status gives that means "an old version is still
	// out there".
	HalfLanded State = "half-landed"
	// Unknown: not observed at all — a compose executor is a container, not a Deployment, and reports
	// no replica counts; neither does an instance registered before this shipped. Absence is not
	// health, and it is not failure either: ClassifyObserved returns this only when a count is
	// genuinely missing, never when it is a real, observed zero.
	Unknown State = "unknown"
)

// Verdict is everything Classify decides from one observation.
type Verdict struct {
	// State is the headline verdict: complete, half-landed, or unknown.
	State State
	// StalePods is total-updated, clamped to >= 0: the number of pods NOT on the current template.
	// Zero whenever State != HalfLanded — a caller can never again print "HALF-LANDED — 0 pod(s)
	// still run the PREVIOUS image", because this is exactly the count that gates State itself.
	StalePods int
	// ReadinessShortfall is true when fewer pods are Ready than are desired. Independent of State: a
	// rollout can be Complete (nothing runs an old template) and still short of Ready — see the
	// no-surge case above — or HalfLanded while every pod that exists IS ready, just outdated.
	ReadinessShortfall bool
	// ReadyGap is desired-ready, clamped to >= 0. Zero whenever ReadinessShortfall is false.
	ReadyGap int
}

// Classify turns four already-observed Deployment counts into a Verdict. Callers that may not have
// observed anything at all (a nil, not a zero) use ClassifyObserved instead.
//
// desired <= 0 is a DELIBERATELY scaled-to-zero instance — never a rollout in progress, whatever
// total/ready/updated say (a teardown can leave any of them briefly nonzero). Reporting it half-landed
// would cry wolf at the operator who scaled it down on purpose; this returns Complete unconditionally,
// with no readiness shortfall (nothing was asked for, so nothing can be short).
func Classify(desired, total, ready, updated int) Verdict {
	if desired <= 0 {
		return Verdict{State: Complete}
	}
	stale := total - updated
	if stale < 0 {
		stale = 0 // never observed in practice (updated cannot exceed total), but never report a negative count
	}
	gap := desired - ready
	if gap < 0 {
		gap = 0 // a surplus of ready pods (mid scale-down) is not a shortfall
	}
	v := Verdict{ReadinessShortfall: gap > 0, ReadyGap: gap}
	if stale > 0 {
		v.State = HalfLanded
		v.StalePods = stale
	} else {
		v.State = Complete
	}
	return v
}

// ClassifyObserved is Classify's nil-tolerant form, for callers whose counts may be UNOBSERVED rather
// than merely zero (the control plane: a compose executor's four counts are all nil — it is a
// container, not a Deployment, and reports none of them — and 0 is a real, valid observation of a
// scaled-to-zero Deployment that must not be read the same way).
func ClassifyObserved(desired, total, ready, updated *int) Verdict {
	if desired == nil || total == nil || ready == nil || updated == nil {
		return Verdict{State: Unknown}
	}
	return Classify(*desired, *total, *ready, *updated)
}
