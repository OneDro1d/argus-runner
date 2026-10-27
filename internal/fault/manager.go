// Package fault is the `fault` chain step: break a connection or a deployment during a
// run, and put it back on EVERY path.
//
// THE FOUR RULES, in the order they are enforced:
//
//  1. THE ALLOWLIST IS THE DOOR. A fault names a deployment or a link; both must be listed under the
//     top-level `fault_allowed_targets` of argus-config.yaml, or the step is refused before anything is
//     read, journaled or applied.
//  2. THE UNDO IS DURABLE BEFORE THE FAULT EXISTS. The undo is written to the results volume (fsync) and only
//     then applied; if the write fails nothing is applied. The entry is removed only after the undo
//     succeeded.
//  3. EVERY PATH ENDS IN A RESTORE. Finish undoes whatever is still active, in reverse order, whether the
//     chain passed, failed, errored, timed out or panicked; Replay does the same for a run whose executor
//     died, before the next executor takes new work.
//  4. RESTORE NEVER CHANGES THE VERDICT (cleanup rule 2), tolerates "already restored" (rule 3), has its own
//     budget per undo (rule 4), has no off switch (rule 6), and captures no output (it records outcomes
//     and sanitised causes, never a body, a URL or an address).
package fault

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/OneDro1d/argus-runner/internal/report"
)

const (
	OpStop      = "stop"
	OpStart     = "start"
	OpScale     = "scale"
	OpWaitReady = "wait_ready"
	OpDelay     = "delay"
	OpDrop      = "drop"
	OpTruncate  = "truncate"
	OpRestore   = "restore"
)

// Targets is the allowlist as the manager sees it. internal/argus adapts config.Config to it.
type Targets interface {
	Deployment(name string) (namespace string, ok bool)
	Link(name string) (toxiproxyAPI, proxy string, ok bool)
}

// Spec is one fault step, already validated for shape (scenario.faultStepErrors).
type Spec struct {
	Step, Op            string
	Deployment, Link    string
	Replicas            *int
	Timeout             time.Duration // wait_ready
	LatencyMs, JitterMs int
	Bytes               int
}

// Config wires a Manager. Kube and Toxi are factories so a chain with no fault step builds nothing.
type Config struct {
	Run     string // the correlation id; toxic names are argus-<Run>-<n>
	Journal *Journal
	Targets Targets
	Owner   string // this executor's identity on the shared results volume
	Kube    func() (Kube, error)
	Toxi    func(api string) Toxi
	Now     func() time.Time
	// HeartbeatEvery is how often the entries and locks are touched while the run lives (default 10s);
	// StaleAfter how long an untouched one is presumed dead (default 60s, six missed beats).
	HeartbeatEvery time.Duration
	StaleAfter     time.Duration
	// UndoTimeout is one undo's own budget (default 30s, cleanup rule 4).
	UndoTimeout time.Duration
}

// RestoreBackoff and PollEvery are vars only so tests can shorten them.
var (
	RestoreBackoff = time.Second
	PollEvery      = 2 * time.Second
)

const (
	defaultWait = 120 * time.Second
	maxWait     = 600 * time.Second
	undoTries   = 3
)

type activeFault struct {
	u   Undo
	rec int // index into Manager.recs
	key string
}

// Manager applies and undoes one chain's faults. Safe for the abandoned-step case: a step whose call was
// abandoned may still be running when the chain ends, so Apply and Finish serialise on one mutex and Apply
// refuses once Finish has run.
type Manager struct {
	cfg Config

	mu      sync.Mutex
	closed  bool
	seq     int64
	n       int
	active  []*activeFault
	recs    []report.Fault
	locks   map[string]bool
	beat    chan struct{}
	beatEnd sync.WaitGroup
	kube    Kube
}

func New(cfg Config) *Manager {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.HeartbeatEvery <= 0 {
		cfg.HeartbeatEvery = 10 * time.Second
	}
	if cfg.StaleAfter <= 0 {
		cfg.StaleAfter = 60 * time.Second
	}
	if cfg.UndoTimeout <= 0 {
		cfg.UndoTimeout = 30 * time.Second
	}
	return &Manager{cfg: cfg, locks: map[string]bool{}}
}

func (m *Manager) stamp() string { return m.cfg.Now().UTC().Format(time.RFC3339Nano) }

func (m *Manager) kubeClient() (Kube, error) {
	if m.kube != nil {
		return m.kube, nil
	}
	if m.cfg.Kube == nil {
		return nil, errors.New("no Kubernetes client is configured")
	}
	k, err := m.cfg.Kube()
	if err != nil {
		return nil, err
	}
	m.kube = k
	return k, nil
}

func refusal(what string) error {
	return fmt.Errorf("fault refused: %s - nothing was applied", what)
}

// Apply runs one fault step. For start/restore/wait_ready it changes or reads state without opening a new
// fault; for the others it journals the undo, applies the fault and records it.
func (m *Manager) Apply(ctx context.Context, sp Spec) error {
	switch sp.Op {
	case OpRestore:
		if errs := m.Restore(ctx); len(errs) > 0 {
			return errors.Join(errs...)
		}
		return nil
	case OpWaitReady:
		return m.waitReady(ctx, sp)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return refusal("the chain has already ended")
	}
	switch sp.Op {
	case OpStop, OpScale, OpStart:
		return m.applyDeployment(ctx, sp)
	case OpDelay, OpDrop, OpTruncate:
		return m.applyLink(ctx, sp)
	}
	return refusal("unknown fault op " + fmt.Sprintf("%q", sp.Op))
}

func (m *Manager) nextUndo(sp Spec, kind, target string) Undo {
	m.n++
	seq := m.cfg.Now().UnixNano()
	if seq <= m.seq {
		seq = m.seq + 1
	}
	m.seq = seq
	return Undo{ID: fmt.Sprintf("%s-%d", m.cfg.Run, m.n), Seq: seq, Run: m.cfg.Run, Owner: m.cfg.Owner,
		Step: sp.Step, Op: sp.Op, Target: target, Kind: kind, AppliedAt: m.stamp()}
}

func (m *Manager) lock(key string) error {
	if m.locks[key] {
		return nil
	}
	if err := m.cfg.Journal.Lock(key, m.cfg.Owner, m.cfg.Run, m.cfg.StaleAfter, m.cfg.Now()); err != nil {
		if errors.Is(err, ErrLocked) {
			return refusal("this target is being faulted by another run right now (two runs would each record the other's fault as the normal state)")
		}
		return refusal("the undo journal is not writable (" + shortCause(err) + ")")
	}
	m.locks[key] = true
	m.startBeat()
	return nil
}

func (m *Manager) startBeat() {
	if m.beat != nil {
		return
	}
	stop := make(chan struct{})
	m.beat = stop
	m.beatEnd.Add(1)
	go func() {
		defer m.beatEnd.Done()
		t := time.NewTicker(m.cfg.HeartbeatEvery)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				m.mu.Lock()
				now := m.cfg.Now()
				for _, a := range m.active {
					m.cfg.Journal.Touch(a.u, now)
				}
				for k := range m.locks {
					m.cfg.Journal.TouchLock(k, now)
				}
				m.mu.Unlock()
			}
		}
	}()
}

func (m *Manager) record(a *activeFault, params map[string]int, toxi bool) {
	m.recs = append(m.recs, report.Fault{Step: a.u.Step, Op: a.u.Op, Target: a.u.Target, Params: params,
		AppliedAt: a.u.AppliedAt, Toxiproxy: toxi})
	a.rec = len(m.recs) - 1
}

func (m *Manager) applyDeployment(ctx context.Context, sp Spec) error {
	ns, ok := m.cfg.Targets.Deployment(sp.Deployment)
	if !ok {
		return refusal(fmt.Sprintf("deployment %q is not listed under fault_allowed_targets.deployments in argus-config.yaml", sp.Deployment))
	}
	k, err := m.kubeClient()
	if err != nil {
		return refusal(err.Error())
	}
	key := "deployment-" + ns + "-" + sp.Deployment
	if sp.Op == OpStart {
		if a := m.findStop(sp.Deployment); a != nil {
			return m.startFromStop(ctx, k, a, ns, sp)
		}
		if sp.Replicas == nil {
			return refusal(fmt.Sprintf("start %q: no earlier stop of it in this chain recorded a replica count, and no `replicas` was given", sp.Deployment))
		}
		sp.Op = OpScale // an explicit count with nothing to return to is just a scale
	}
	if err := m.lock(key); err != nil {
		return err
	}
	cur, err := k.GetScale(ctx, ns, sp.Deployment)
	if err != nil {
		return refusal("reading the current replica count: " + err.Error())
	}
	target, params := 0, map[string]int{"replicas": 0}
	if sp.Op == OpScale {
		target = *sp.Replicas
		params["replicas"] = target
	}
	u := m.nextUndo(sp, KindScale, sp.Deployment)
	u.Namespace, u.Deployment, u.Replicas = ns, sp.Deployment, cur
	u.Params = params
	// ── THE UNDO IS DURABLE BEFORE THE FAULT IS APPLIED ───────────────────────────────────────────
	if err := m.cfg.Journal.Write(u); err != nil {
		return refusal(err.Error())
	}
	a := &activeFault{u: u, key: key}
	m.active = append(m.active, a)
	m.record(a, params, false)
	if err := k.SetScale(ctx, ns, sp.Deployment, target); err != nil {
		m.undoNow(a) // it may have half-applied; put it back at once, or leave it journaled and say so
		return fmt.Errorf("fault %s %s: %w", sp.Op, sp.Deployment, err)
	}
	return nil
}

func (m *Manager) findStop(dep string) *activeFault {
	for i := len(m.active) - 1; i >= 0; i-- {
		if a := m.active[i]; a.u.Op == OpStop && a.u.Kind == KindScale && a.u.Deployment == dep {
			return a
		}
	}
	return nil
}

func (m *Manager) startFromStop(ctx context.Context, k Kube, a *activeFault, ns string, sp Spec) error {
	n := a.u.Replicas
	if sp.Replicas != nil {
		n = *sp.Replicas
	}
	if n == 0 {
		return refusal(fmt.Sprintf("start %q: it was already at 0 replicas when stopped, so there is no count to return to - give `replicas`", sp.Deployment))
	}
	uctx, cancel := context.WithTimeout(ctx, m.cfg.UndoTimeout)
	defer cancel()
	if err := k.SetScale(uctx, ns, sp.Deployment, n); err != nil {
		return fmt.Errorf("fault start %s: %w", sp.Deployment, err)
	}
	m.finishOne(a, report.FaultRestored)
	return nil
}

func (m *Manager) applyLink(ctx context.Context, sp Spec) error {
	api, proxy, ok := m.cfg.Targets.Link(sp.Link)
	if !ok {
		return refusal(fmt.Sprintf("link %q is not listed under fault_allowed_targets.links in argus-config.yaml", sp.Link))
	}
	if m.cfg.Toxi == nil {
		return refusal("no Toxiproxy client is configured")
	}
	tx := m.cfg.Toxi(api)
	key := "proxy-" + proxy
	if err := m.lock(key); err != nil {
		return err
	}
	u := m.nextUndo(sp, KindToxic, sp.Link)
	u.ToxiAPI, u.Proxy = api, proxy
	params := map[string]int{}
	var toxic Toxic
	switch sp.Op {
	case OpDelay:
		params["latency_ms"] = sp.LatencyMs
		attrs := map[string]int{"latency": sp.LatencyMs}
		if sp.JitterMs > 0 {
			params["jitter_ms"] = sp.JitterMs
			attrs["jitter"] = sp.JitterMs
		}
		toxic = Toxic{Type: "latency", Stream: "downstream", Attributes: attrs}
	case OpTruncate:
		params["bytes"] = sp.Bytes
		toxic = Toxic{Type: "limit_data", Stream: "downstream", Attributes: map[string]int{"bytes": sp.Bytes}}
	case OpDrop:
		was, err := tx.ProxyEnabled(ctx, proxy)
		if err != nil {
			return refusal("reading the proxy: " + err.Error())
		}
		u.Kind, u.RestoreEnabled = KindProxy, was
	}
	if u.Kind == KindToxic {
		// A name Argus owns: restore removes exactly this, never an operator's own toxic.
		toxic.Name = "argus-" + m.cfg.Run + "-" + fmt.Sprint(m.n)
		u.Toxic = toxic.Name
	}
	if len(params) > 0 {
		u.Params = params
	}
	// ── THE UNDO IS DURABLE BEFORE THE FAULT IS APPLIED ───────────────────────────────────────────
	if err := m.cfg.Journal.Write(u); err != nil {
		return refusal(err.Error())
	}
	a := &activeFault{u: u, key: key}
	m.active = append(m.active, a)
	m.record(a, params, true)
	var err error
	if u.Kind == KindProxy {
		err = tx.SetEnabled(ctx, proxy, false)
	} else {
		err = tx.AddToxic(ctx, proxy, toxic)
	}
	if err != nil {
		m.undoNow(a)
		return fmt.Errorf("fault %s %s: %w", sp.Op, sp.Link, err)
	}
	return nil
}

func (m *Manager) waitReady(ctx context.Context, sp Spec) error {
	ns, ok := m.cfg.Targets.Deployment(sp.Deployment)
	if !ok {
		return refusal(fmt.Sprintf("deployment %q is not listed under fault_allowed_targets.deployments in argus-config.yaml", sp.Deployment))
	}
	m.mu.Lock()
	closed := m.closed
	k, err := m.kubeClient()
	m.mu.Unlock()
	if closed {
		return refusal("the chain has already ended")
	}
	if err != nil {
		return refusal(err.Error())
	}
	to := sp.Timeout
	if to <= 0 {
		to = defaultWait
	}
	if to > maxWait {
		to = maxWait
	}
	wctx, cancel := context.WithTimeout(ctx, to)
	defer cancel()
	var spec, ready int
	for {
		s, r, settled, rerr := k.Ready(wctx, ns, sp.Deployment)
		if rerr == nil {
			spec, ready = s, r
			if settled && r == s {
				return nil
			}
		} else if wctx.Err() == nil {
			return fmt.Errorf("wait_ready %s: %w", sp.Deployment, rerr)
		}
		select {
		case <-wctx.Done():
			return fmt.Errorf("wait_ready %s: not ready after %s (%d of %d replicas ready)", sp.Deployment, to, ready, spec)
		case <-time.After(PollEvery):
		}
	}
}

// undoNow undoes one entry under the lock (Apply's failure path). A failure leaves it active and journaled.
func (m *Manager) undoNow(a *activeFault) {
	outcome, _ := m.undoWithRetries(context.Background(), a.u)
	if outcome == report.FaultRestoreFailed {
		m.recs[a.rec].RestoreOutcome = outcome
		return
	}
	m.finishOne(a, outcome)
}

// finishOne closes an entry whose undo succeeded: journal entry gone, record stamped, lock freed when the
// target has nothing else active.
func (m *Manager) finishOne(a *activeFault, outcome string) {
	_ = m.cfg.Journal.Remove(a.u)
	m.recs[a.rec].RestoredAt, m.recs[a.rec].RestoreOutcome = m.stamp(), outcome
	for i, x := range m.active {
		if x == a {
			m.active = append(m.active[:i], m.active[i+1:]...)
			break
		}
	}
	for _, x := range m.active {
		if x.key == a.key {
			return
		}
	}
	m.cfg.Journal.Unlock(a.key)
	delete(m.locks, a.key)
}

func (m *Manager) undoWithRetries(ctx context.Context, u Undo) (string, error) {
	var last error
	for try := 0; try < undoTries; try++ {
		if try > 0 {
			time.Sleep(RestoreBackoff * time.Duration(try))
		}
		uctx, cancel := context.WithTimeout(ctx, m.cfg.UndoTimeout)
		outcome, err := m.undoOnce(uctx, u)
		cancel()
		if err == nil {
			return outcome, nil
		}
		last = err
	}
	return report.FaultRestoreFailed, last
}

func (m *Manager) undoOnce(ctx context.Context, u Undo) (string, error) {
	return undoEntry(ctx, u, m.kubeClient, m.cfg.Toxi)
}

// undoEntry is the one undo, shared by Finish and Replay. "Already restored" is a success.
func undoEntry(ctx context.Context, u Undo, kube func() (Kube, error), toxi func(string) Toxi) (string, error) {
	switch u.Kind {
	case KindScale:
		k, err := kube()
		if err != nil {
			return "", err
		}
		cur, err := k.GetScale(ctx, u.Namespace, u.Deployment)
		if err != nil {
			return "", err
		}
		if cur == u.Replicas {
			return report.FaultAlreadyRestored, nil
		}
		if err := k.SetScale(ctx, u.Namespace, u.Deployment, u.Replicas); err != nil {
			return "", err
		}
		return report.FaultRestored, nil
	case KindToxic:
		// Only a toxic Argus named. A journal entry cannot make Argus delete an operator's toxic.
		if !strings.HasPrefix(u.Toxic, "argus-") {
			return "", errors.New("the journal names a toxic Argus does not own; refusing to delete it")
		}
		found, err := toxi(u.ToxiAPI).DeleteToxic(ctx, u.Proxy, u.Toxic)
		if err != nil {
			return "", err
		}
		if !found {
			return report.FaultAlreadyRestored, nil
		}
		return report.FaultRestored, nil
	case KindProxy:
		if !u.RestoreEnabled {
			return report.FaultAlreadyRestored, nil // it was disabled before we touched it: leave it as found
		}
		tx := toxi(u.ToxiAPI)
		on, err := tx.ProxyEnabled(ctx, u.Proxy)
		if err != nil {
			return "", err
		}
		if on {
			return report.FaultAlreadyRestored, nil
		}
		if err := tx.SetEnabled(ctx, u.Proxy, true); err != nil {
			return "", err
		}
		return report.FaultRestored, nil
	}
	return "", fmt.Errorf("unknown undo kind %q", u.Kind)
}

// Restore undoes every active fault now, newest first (the `restore` step). It returns one error per undo
// that FAILED; the entry stays journaled and active so Finish and the next executor start try again.
func (m *Manager) Restore(ctx context.Context) []error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.restoreLocked(ctx)
}

func (m *Manager) restoreLocked(ctx context.Context) []error {
	var errs []error
	for i := len(m.active) - 1; i >= 0; i-- {
		a := m.active[i]
		outcome, err := m.undoWithRetries(ctx, a.u)
		if outcome == report.FaultRestoreFailed {
			m.recs[a.rec].RestoreOutcome = outcome
			errs = append(errs, fmt.Errorf("fault restore FAILED for %s %s (%s): %v", a.u.Op, a.u.Target, a.u.Kind, err))
			continue
		}
		m.finishOne(a, outcome)
	}
	return errs
}

// Finish is the END-OF-CHAIN restore: it closes the manager (a late Apply from an abandoned step is
// refused), undoes whatever is still active, and stops the heartbeat. Call it on EVERY path.
func (m *Manager) Finish(ctx context.Context) []error {
	m.mu.Lock()
	m.closed = true
	errs := m.restoreLocked(ctx)
	beat := m.beat
	m.beat = nil
	m.mu.Unlock()
	if beat != nil {
		close(beat)
		m.beatEnd.Wait()
	}
	return errs
}

// Records is the faults applied so far, in order. A copy.
func (m *Manager) Records() []report.Fault {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]report.Fault, len(m.recs))
	copy(out, m.recs)
	return out
}

// Hostname is the owner identity on the shared results volume: the pod name.
func Hostname() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "executor"
	}
	return h
}
