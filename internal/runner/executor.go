package runner

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/OneDro1d/argus-runner/internal/argus"
	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/federation"
	"github.com/OneDro1d/argus-runner/internal/updatecmd"
)

// RunFunc executes one picked-up assignment and returns the results push. The real implementation
// (execute.go) materializes the scenario set and drives the M2.5 runner core; tests inject a stub so
// the poll loop is exercisable without JMeter/a SUT.
type RunFunc func(ctx context.Context, a *federation.RunAssignment) (federation.ResultsPush, error)

// Executor is the federation poll loop: poll → pickup → run → files-first push (D-FED.4), forever.
type Executor struct {
	Client *Client
	Run    RunFunc
	// Commands (AC-17), when set, executes a relayed builder command envelope through the same
	// in-process code the local router's runner__* tools use (toolcore) — never a shell. nil = this
	// executor answers every relayed command with an error rather than dropping it silently (a
	// deployment that never wired NewCommandFunc simply cannot serve the builder relay yet).
	Commands      CommandFunc
	RunnerVersion string
	// VR9-C1 rule 5 — what this executor needs to render its OWN update command. It used to print a
	// RELATIVE path and an `<id>` PLACEHOLDER because it had none of these to hand.
	//
	// ⚠ KitDir is legitimately EMPTY on a deployment that was never told it: foldersEnvBlock omits
	// ARGUS_KIT_DIR_HOST entirely when empty, so absent means "not told", never "blank". The
	// renderer returns "" for that, and the log line says so rather than printing a template.
	Tier     string
	Instance string
	KitDir   string
	// VR10-U1 (V28-020) — the cluster this instance was onboarded against, so the block this
	// executor logs names it instead of leaving update.sh to act on whatever context the operator's
	// kubectl happens to point at. From ARGUS_KUBE_CONTEXT_HOST / ARGUS_KUBECONFIG_HOST, which
	// onboarding writes; empty on compose, and empty on anything onboarded before 0.3.29 — for the
	// latter the block carries the one `<KUBE_CONTEXT>` placeholder and says so.
	KubeContext string
	Kubeconfig  string
	PollGap     time.Duration // client wait between empty polls (the ~25s hold is server-side)
	SlowRetry   time.Duration // on unknown_instance / outdated-blocked
	// Floors is the executor's DURABLE copy of the published version floors (VR-V5). Refreshed on
	// every poll and read at startup, so a restart while the control plane is unreachable does not
	// leave it inventing a verdict from an absence. Nil-safe: a nil store simply knows nothing.
	Floors      *FloorStore
	Backoff     time.Duration // on transient errors
	PushRetries int
	// HeartbeatEvery is the mid-run heartbeat interval (W1/3, §D-3.1.2): a status='running' push
	// every tick keeps the watchdog's heartbeat-staleness cutoff from reaping a live long run. The
	// CP watchdog cutoff MUST comfortably exceed this interval (default 30s vs the CP's 3m default
	// cutoff = 6 missed heartbeats of margin).
	HeartbeatEvery time.Duration
	// DeployWatch, when set, auto-detects a SUT redeploy each loop iteration and pushes a deployment
	// marker (UC073). nil = no auto-detect (manual `mark-deployment`, UC074, remains the fallback).
	DeployWatch *DeploymentWatcher
	// Autoscale, when set (k8s tiers — UC196), right-sizes this instance's executor Deployment: an
	// idle-parked instance scales to a cheap floor, a triggered run scales it back to min-3. nil →
	// no autoscaling (compose is single-container; a k8s tier without the scale RBAC stays fixed).
	Autoscale *Autoscaler
	// Outbox, when set, makes terminal results pushes DURABLE (F1, UC065/UC060): the payload is
	// written to the results volume before the first network call and removed only on CP
	// acknowledgement, so a control plane that is down when a run finishes no longer costs the
	// ledger that run's results. nil → the old behaviour (retry a few times, then lose them).
	Outbox *Outbox
	// AfterPush, when set, runs once after a results push the control plane ACCEPTED (never after a
	// failed attempt, never before): ARGUS-CMP-3's retention of old compare outputs hangs here, so a run
	// whose results never reached the ledger keeps every directory it could still be asked about.
	AfterPush func(federation.ResultsPush)
	// ImageUpdate, when set (k8s tiers — F15/UC069), re-images this instance's own executor
	// Deployment on an operator's request delivered through the poll response. nil on compose, which
	// has no docker socket and physically cannot recreate its own container.
	ImageUpdate func(ctx context.Context, image string) error
	Log         func(format string, args ...any)
	// SUTConfig, when set, turns on the SUT reachability probe (VR6-W1). nil leaves every poll
	// reporting NOT MEASURED, which is the honest answer for an executor that was never told what its
	// SUT is — and which the page renders as grey rather than as a verdict.
	SUTConfig *config.Config
	// ConfigPath (T5.4 follow-up) is the SUT's argus-config.yaml path — the same
	// ExecConfig.ConfigPath registerRequestFor already reads via declaredTargets/moneyHandlingFor.
	// Carried here (rather than re-reached through ExecConfig, which Loop does not hold) so the poll
	// loop can recompute money_handling on every tick the same way declaredTargets is recomputed at
	// register: a config edited between polls (money_handling flipped on) must reach the control
	// plane without a restart. "" → moneyHandlingFor reports nil (not reported), never false.
	ConfigPath string

	// sutProbe is built ONCE in defaults() and reused, because the CADENCE lives inside it. A probe
	// rebuilt per poll would have an empty `last` every time and dial the SUT on every return — the
	// exact §0.4 failure the gate exists to prevent.
	sutProbe *sutProbe
}

// UpdateBlock renders THE BLOCK an operator pastes to update THIS executor (VR10-U1, V28-020), and
// reports whether it still carries the one `<KUBE_CONTEXT>` placeholder.
//
// ── WHY THE EXECUTOR HAS A METHOD AND NOT A CALL TO THE RENDERER AT THE LOG SITE ─────────────────
//
// The renderer cannot drift — there is one of it. The two CALL SITES can: each assembles the
// renderer's input from a different source (the control plane from the stored row, this executor from
// the environment onboarding wrote), and a field one of them forgets to pass renders a block that
// names the wrong cluster while every unit test of the renderer stays green. Naming the assembly here
// gives the both-sites test something to drive that is the product's own path, rather than two names
// for one function.
//
// "" when this executor was not told enough to render a complete one — most often the kit directory,
// which foldersEnvBlock omits entirely when empty so that unknown is ABSENT rather than blank.
func (e *Executor) UpdateBlock(image string) (block string, placeholder bool) {
	return updatecmd.RenderBlock(updatecmd.Instance{
		Tier:        e.Tier,
		InstanceID:  e.Instance,
		Image:       image,
		KitDir:      e.KitDir,
		KubeContext: e.KubeContext,
		Kubeconfig:  e.Kubeconfig,
	})
}

func (e *Executor) log(f string, a ...any) {
	if e.Log != nil {
		e.Log(f, a...)
	}
}

func (e *Executor) defaults() {
	if e.sutProbe == nil {
		e.sutProbe = newSUTProbe(e.SUTConfig)
		e.sutProbe.logf = e.log
	}
	if e.PollGap == 0 {
		e.PollGap = time.Second
	}
	if e.SlowRetry == 0 {
		e.SlowRetry = 5 * time.Minute
	}
	if e.Backoff == 0 {
		e.Backoff = 3 * time.Second
	}
	if e.PushRetries == 0 {
		e.PushRetries = 5
	}
	if e.HeartbeatEvery == 0 {
		e.HeartbeatEvery = 30 * time.Second
	}
}

// startHeartbeat begins the mid-run heartbeat ticker for a run (W1/3): a best-effort
// status='running' push every HeartbeatEvery until the returned stop func is called. A push failure
// is logged and never interrupts the run (the next tick re-arms; the watchdog cutoff spans several
// intervals).
func (e *Executor) startHeartbeat(ctx context.Context, runID, rrID, scope string) (stop func()) {
	if e.Client == nil {
		return func() {}
	}
	// The ticker interval MUST be positive. defaults() sets HeartbeatEvery, but only the poll-loop
	// path (Loop) calls defaults() — the run-direct path (DirectRun) builds the Executor and reaches
	// here with HeartbeatEvery still 0, and time.NewTicker(0) panics ("non-positive interval").
	// Found 2026-07-24: the winning replica of the UC161 fence race crashed exactly here. Guard the
	// invariant where the ticker is created so any caller is safe, not just the two that exist today.
	if e.HeartbeatEvery <= 0 {
		e.HeartbeatEvery = 30 * time.Second
	}
	started := time.Now().UTC()
	hctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(e.HeartbeatEvery)
		defer t.Stop()
		for {
			select {
			case <-hctx.Done():
				return
			case <-t.C:
				pctx, pcancel := context.WithTimeout(context.Background(), 10*time.Second)
				if err := e.Client.PushHeartbeat(pctx, runID, rrID, scope, started); err != nil {
					e.log("heartbeat push failed (harmless; next tick re-arms): %v", err)
				}
				pcancel()
			}
		}
	}()
	return func() { cancel(); <-done }
}

// runCommands (AC-17) answers every relayed command envelope this poll carried, in order, best
// effort: a push failure is logged and never interrupts the loop — the builder's own bounded wait
// simply expires, exactly as it would for an executor that went silent mid-answer.
func (e *Executor) runCommands(ctx context.Context, cmds []federation.CommandEnvelope) {
	for _, cmd := range cmds {
		var result json.RawMessage
		var errStr string
		if e.Commands == nil {
			errStr = "this executor is not wired to answer relayed commands"
		} else if r, err := e.Commands(ctx, cmd.Verb, cmd.Args); err != nil {
			errStr = err.Error()
		} else {
			result = r
		}
		cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if perr := e.Client.CommandResult(cctx, cmd.CommandID, result, errStr); perr != nil {
			e.log("relayed command %s (%s) push failed (harmless; the builder's bounded wait expires): %v", cmd.CommandID, cmd.Verb, perr)
		}
		cancel()
	}
}

// Loop runs poll→pickup→run→push until ctx is cancelled. It NEVER exits on a federation error (a wrong
// clock or a revoked registration self-heals via retry) — only ctx cancellation stops it.
func (e *Executor) Loop(ctx context.Context) error {
	e.defaults()
	// UC196: the autoscaler's scale-DOWN reconcile runs in the background (scale-UP is immediate via
	// MarkActive at run pickup). One goroutine per process. Every replica runs it, which is safe ONLY
	// because a scale-down consults the shared active lease on the Deployment (AC-D43, issue #207):
	// with per-process state alone, a replica created by the scale-up judged the instance idle and
	// scaled it down under a running test.
	if e.Autoscale != nil {
		go e.Autoscale.Run(ctx)
	}
	// UI-7a: the summary-readings reader. One goroutine, its own clock; with no
	// summary_metrics block in the config it does nothing and the poll body is unchanged.
	if e.Client != nil && e.ConfigPath != "" {
		if e.Client.Summary == nil {
			e.Client.Summary = NewSummaryTicker(e.ConfigPath, e.log)
		}
		go e.Client.Summary.Run(ctx)
	}
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		e.checkDeploy(ctx) // UC073: observe a SUT redeploy → auto-push a deployment marker
		// F10/UC196: every poll carries what the executor OBSERVED about its own Deployment, so the
		// control plane reports the replicas it GOT rather than the ones it asked for. nil on compose
		// (no Deployment to observe) — never a fabricated 1/1.
		var repDesired, repTotal, repReady, repUpdated *int
		if e.Autoscale != nil {
			if d, tot, r, u, ok := e.Autoscale.Observed(); ok {
				repDesired, repTotal, repReady, repUpdated = &d, &tot, &r, &u
			}
		}
		// VR-F28/INT-004: announce what the outbox is still holding. The control plane cannot mark a
		// run `results_pending` unless it is told which runs those are, and a poll that RETURNS is the
		// reachability proof that makes this the right moment to say it.
		// VR6-W1: what this executor last learned by DIALLING its SUT. On its own 60 s clock, not the
		// poll's — the poll long-polls, so there is no interval to borrow (SA §0.4), and on a busy
		// instance whose polls return immediately "once per poll" would dial continuously.
		//
		// ⚠ BEST-EFFORT. Observe never errors and cannot panic out. The poll IS the heartbeat, so a
		// flaky SUT able to break it would make its own executor read as STALE — through the very
		// channel the operator needs in order to see the SUT problem.
		sutReach, sutAt := e.sutProbe.Observe()
		// T5.4 follow-up: recomputed every tick, the same way declaredTargets is
		// recomputed at register — a config edited between polls (money_handling flipped on) must
		// reach the control plane without an executor restart, and the poll is the only carrier that
		// self-heals (see wire.go PollRequest.MoneyHandling and moneyHandlingFor's own doc).
		moneyHandling := moneyHandlingFor(e.ConfigPath)
		// money_writes follow-up: recomputed every tick for the same self-heal reason moneyHandling
		// is, immediately above.
		moneyWritesAllow := moneyWritesAllowFor(e.ConfigPath)
		resp, err := e.Client.Poll(ctx, e.RunnerVersion, repDesired, repTotal, repReady, repUpdated, e.Outbox.Held(),
			federation.SUTObservation{Reachable: sutReach, CheckedAt: sutAt}, moneyHandling, moneyWritesAllow,
			// (UI-6): recomputed every tick, for the same self-heal reason as the two above.
			withPollTestTargets(testTargetsFor(e.ConfigPath)),
			// (UI-2): the dashboard link template, recomputed every tick like test_targets.
			withPollDashboardLink(dashboardLinkFor(e.ConfigPath)))
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errors.Is(err, ErrUnknownInstance) {
				e.log("federation: %v — retrying every %s", err, e.SlowRetry)
				if !wait(ctx, e.SlowRetry) {
					return ctx.Err()
				}
				continue
			}
			e.log("federation poll error: %v", err)
			if !wait(ctx, e.Backoff) {
				return ctx.Err()
			}
			continue
		}
		// The poll SUCCEEDED, so the control plane is reachable right now — deliver anything a
		// previous outage left queued (F1). Before the Outdated/HasRun branches on purpose: results
		// already produced are still valid even when this executor is being refused new work.
		e.drainOutbox(ctx)
		// VR-V5: record what the control plane publishes, on EVERY poll. Before the version branches
		// below, so a decision is never made against a staler copy than the one just received.
		// Observe ignores an incomplete pair, so an older control plane cannot erase what we know.
		e.Floors.Observe(federation.FloorsFrom(resp.Versions))
		// F15/UC069: an operator asked for a specific image. Delivered exactly once (the control
		// plane cleared it as it handed it over), so a failure here does NOT retry by itself — that
		// is what stops a bad image becoming a crash-loop. The operator asks again.
		if resp.UpdateTo != "" {
			if e.ImageUpdate == nil {
				// VR9-C1 rule 5 — THE SECOND RENDER SITE, and it was worse than the first: it emitted
				// `onboarding/update.sh` (RELATIVE — the defect VR5-U1 already fixed at the other site,
				// resolving only from inside that instance's kit) and `--instance-id <id>` (a
				// PLACEHOLDER the operator had to repair, which VR5-U3 forbids outright).
				//
				// It now renders through the SAME function the control plane uses, so the two cannot
				// drift — and when it cannot render a complete command it says so instead of printing
				// a broken one. ⛔ kitDir is genuinely unknown on some deployments: foldersEnvBlock
				// omits ARGUS_KIT_DIR_HOST entirely when empty, so absent means "not told", never
				// "blank".
				//
				// 🔄 VR10-U1 (V28-020): it is a five-line BLOCK now, logged VERBATIM across those
				// lines. Folding it onto one line to keep the log tidy would hand the operator
				// something they cannot paste, which is the whole defect this requirement removes.
				if cmd, ph := e.UpdateBlock(resp.UpdateTo); cmd != "" {
					e.log("an update to %s was requested, but this tier cannot self-update — run this on the machine this instance was onboarded from:\n%s", resp.UpdateTo, cmd)
					if ph {
						e.log("that block still carries the <KUBE_CONTEXT> placeholder: this instance was onboarded " +
							"before the cluster name was recorded, so replace it with your kubectl context name " +
							"(for example k3d-argus or example-overlay), or re-onboard on 0.3.29 or later.")
					}
				} else {
					e.log("an update to %s was requested, but this tier cannot self-update, and this "+
						"executor was not told the kit directory it was onboarded from — so the exact "+
						"command cannot be given here, and the Environments page cannot render one either "+
						"(it records the same kit directory). Re-onboard this instance so the kit directory "+
						"is recorded; the page then shows its update block.", resp.UpdateTo)
				}
			} else if err := e.ImageUpdate(ctx, resp.UpdateTo); err != nil {
				e.log("requested update to %s FAILED (%v) — staying on the current image; ask again once the cause is fixed", resp.UpdateTo, err)
			} else {
				e.log("requested update to %s accepted — the Deployment is rolling; this pod will be replaced", resp.UpdateTo)
				if !wait(ctx, e.SlowRetry) {
					return ctx.Err()
				}
			}
			continue
		}
		if resp.Outdated {
			// VR-V3: no self-restart. An executor below the ABSOLUTE floor stays visible and refuses
			// work until a PERSON updates it — identical on every tier now, where this used to branch.
			e.log("executor OUTDATED — update required (CP current=%s); staying visible, refusing work", resp.Versions.CurrentVersion)
			if !wait(ctx, e.SlowRetry) {
				return ctx.Err()
			}
			continue
		}
		// AC-17: relayed builder commands ride BESIDE any run assignment — never a new inbound path —
		// so they are answered here regardless of whether this same poll also carried a run.
		if len(resp.Commands) > 0 {
			e.runCommands(ctx, resp.Commands)
		}
		if !resp.HasRun || resp.Run == nil {
			if !wait(ctx, e.PollGap) {
				return ctx.Err()
			}
			continue
		}
		e.log("picked up run_request %s (scope %s, %d scenarios)", resp.Run.RunRequestID, resp.Run.Scope, len(resp.Run.Scenarios))
		// UC196: a run makes this instance ACTIVE → scale up to min-3 immediately (idempotent). The
		// reconcile loop scales it back down after the idle cooldown once MarkDone fires.
		if e.Autoscale != nil {
			e.Autoscale.MarkActive(ctx)
		}
		// W1/3 (§D-3.1.2): pre-mint the run_id so mid-run heartbeats can address the ledger row
		// (NewRunFunc honors a.RunID); heartbeat for the whole run so a long run is never
		// false-reaped and a dead executor is detected within one cutoff.
		// VR-U1b: the control plane mints the run id at claim time and sends it, so the ledger row it
		// wrote when it handed the work out and the results we push at the end carry the SAME id.
		// Mint only when it sent none — an OLD control plane omits the field, and this executor must
		// keep working against one.
		if resp.Run.RunID == "" {
			resp.Run.RunID = argus.NewRunID()
		}
		stopHB := e.startHeartbeat(ctx, resp.Run.RunID, resp.Run.RunRequestID, resp.Run.Scope)
		push, rerr := e.Run(ctx, resp.Run)
		stopHB()
		if e.Autoscale != nil {
			e.Autoscale.MarkDone(ctx)
		}
		if rerr != nil {
			// The run produced no push — close the request + release the fence NOW with a minimal
			// terminal 'failed' (results stay local; a silent leak would otherwise hold the lock
			// until the watchdog cutoff).
			e.log("run failed: %v (pushing a minimal terminal 'failed' to close the request + release the fence)", rerr)
			now := time.Now().UTC()
			e.pushWithRetry(ctx, federation.ResultsPush{
				RunID: resp.Run.RunID, RunRequestID: resp.Run.RunRequestID, Scope: resp.Run.Scope,
				Status: "failed", SetHash: resp.Run.SetHash, StartedAt: &now, FinishedAt: &now,
				// 019: CARRY THE REASON. This push used to send `failed` and nothing else, so the
				// ledger recorded "status=failed, tallies 0/0" and the web rendered "0/0 passed" —
				// while the sentence that actually explains it sat one line above, in a log nobody
				// reads until they already suspect something.
				//
				// Live 2026-08-10 that cost two k3d instances DAYS of being unable to run anything:
				// their argus-config still had the pre-VR-E4 scalar public_url, and rerr said so
				// precisely — "line 78: this field is now PER-TIER, not a single value. Replace ...".
				// The control plane clamps the length; it never rejects the push for it.
				FailureReason: rerr.Error(),
			})
			continue
		}
		e.pushWithRetry(ctx, push)
	}
}

// checkDeploy runs the UC073 deployment-marker auto-detect once. Best-effort: a probe error or a failed
// marker push is logged and never breaks the poll loop.
func (e *Executor) checkDeploy(ctx context.Context) {
	if e.DeployWatch == nil {
		return
	}
	marker, changed, err := e.DeployWatch.Check(ctx)
	if err != nil {
		e.log("deployment probe: %v", err)
		return
	}
	if !changed {
		return
	}
	if err := e.Client.MarkDeployment(ctx, marker); err != nil {
		e.log("auto deployment marker push failed: %v", err)
		return
	}
	e.log("SUT redeploy detected — pushed deployment marker %s", marker)
}

// pushWithRetry delivers a TERMINAL results push. The retry budget is now an optimisation, not a
// deadline: the payload is queued durably FIRST (F1), so exhausting the attempts means "not yet",
// not "lost". Every later poll that reaches the CP drains the queue.
//
// The old version ended with `e.log("push gave up ... will re-push on a later run")`. Nothing
// re-pushed. That log line is the reason this defect survived to the Stage III gate: it read as a
// handled case.
func (e *Executor) pushWithRetry(ctx context.Context, push federation.ResultsPush) {
	// BEFORE the first network call — the ordering is the guarantee.
	if err := e.Outbox.Put(push); err != nil {
		e.log("WARNING: could not queue results for run %s durably (%v) — this push is best-effort only", push.RunID, err)
	}
	for attempt := 0; attempt <= e.PushRetries; attempt++ {
		if err := e.Client.Push(ctx, push); err == nil {
			e.log("pushed results for run %s (%d/%d passed)", push.RunID, push.Tallies.Passed, push.Tallies.Total)
			if e.AfterPush != nil {
				e.AfterPush(push) // ARGUS-CMP-3 retention: only after a push the control plane accepted
			}
			if aerr := e.Outbox.Ack(push.RunID); aerr != nil {
				e.log("outbox: run %s delivered but its queue entry survived (%v) — a duplicate push may follow, which the ledger upsert absorbs", push.RunID, aerr)
			}
			return
		} else {
			e.log("push attempt %d/%d failed: %v", attempt+1, e.PushRetries+1, err)
		}
		if !wait(ctx, e.Backoff) {
			return // ctx cancelled — the entry stays queued and the next executor start delivers it
		}
	}
	if e.Outbox.enabled() {
		e.log("results for run %s are QUEUED (%d pending) — they land on the next poll that reaches the control plane; nothing is lost",
			push.RunID, e.Outbox.Pending())
		return
	}
	e.log("push gave up for run %s and NO durable outbox is configured — these results will not reach the ledger", push.RunID)
}

// drainOutbox is called on every poll that reached the control plane. A successful poll IS the
// reachability proof, so it is the right moment — and the only one that costs nothing.
//
// When a delivery fails HERE, the control plane is reachable and rejected this particular payload —
// which is a different situation from an outage, and one the ledger can be told about. So a failed
// delivery is followed by a results_pending marker (012): "this run finished, its results exist,
// they are not here yet". Without it the row would sit as `running` until the watchdog called it
// abandoned, which says far less than we actually know.
func (e *Executor) drainOutbox(ctx context.Context) {
	if !e.Outbox.enabled() {
		return
	}
	delivered, remaining := e.Outbox.Drain(ctx, func(c context.Context, p federation.ResultsPush) error {
		err := e.Client.Push(c, p)
		if err != nil {
			e.markResultsPending(c, p)
		} else if e.AfterPush != nil {
			e.AfterPush(p) // a queued run that finally landed counts as a successful push too
		}
		return err
	})
	if delivered > 0 {
		e.log("outbox: delivered %d queued run(s) to the control plane, %d still pending", delivered, remaining)
	}
}

// markResultsPending tells the ledger the run is over and its results exist but have not arrived.
//
// The marker is the SAME push with the per-scenario array stripped. That matters twice over: the
// scenario list is what makes a payload big enough to be rejected in the first place, and the
// TALLIES are the part an operator actually needs — so a run whose full payload will not fit still
// shows its real 30/33 rather than the 0/0/0/0 that started this whole defect.
func (e *Executor) markResultsPending(ctx context.Context, p federation.ResultsPush) {
	marker := p
	marker.Scenarios = nil
	marker.Status = "results_pending"
	if err := e.Client.Push(ctx, marker); err != nil {
		e.log("could not mark run %s results_pending (harmless; the queued entry is unaffected): %v", p.RunID, err)
		return
	}
	e.log("run %s marked results_pending — the results are here and still queued for delivery", p.RunID)
}

// wait sleeps d respecting ctx; returns false if ctx was cancelled during the wait.
func wait(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
