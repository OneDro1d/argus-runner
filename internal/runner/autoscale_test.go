package runner

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestComputeDesiredReplicas(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	cd := 3 * time.Minute
	cases := []struct {
		name       string
		active     bool
		lastActive time.Time
		want       int
	}{
		{"active now → min-3", true, now, 3},
		{"idle within cooldown → still min-3", false, now.Add(-1 * time.Minute), 3},
		{"idle past cooldown → scale to 1", false, now.Add(-5 * time.Minute), 1},
		{"never active → idle floor", false, time.Time{}, 1},
	}
	for _, c := range cases {
		if got := computeDesiredReplicas(c.active, c.lastActive, now, cd, 1, 3); got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
	}
}

// A recording scaler to observe what the Autoscaler patches.
type recScaler struct {
	mu   sync.Mutex
	last int
	n    int
}

func (r *recScaler) scale(_ context.Context, _, _ string, replicas int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.last = replicas
	r.n++
	return nil
}
func (r *recScaler) snap() (int, int) { r.mu.Lock(); defer r.mu.Unlock(); return r.last, r.n }

// fakeDeploy is ONE Deployment shared by several Autoscalers — the thing AC-D43 is about: replicas
// that share a Deployment but not their memory.
type fakeDeploy struct {
	mu       sync.Mutex
	replicas int
	lease    time.Time
	leaseErr error
	history  []int // every replica count patched, in order
}

func (f *fakeDeploy) scale(_ context.Context, _, _ string, n int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replicas = n
	f.history = append(f.history, n)
	return nil
}
func (f *fakeDeploy) read(context.Context, string, string) (time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lease, f.leaseErr
}
func (f *fakeDeploy) write(_ context.Context, _, _ string, until time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lease = until
	return nil
}
func (f *fakeDeploy) snap() (int, []int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.replicas, append([]int(nil), f.history...)
}

// replica is one executor process against the shared Deployment. Status is stubbed so defaults()
// wires nothing in-cluster.
func (f *fakeDeploy) replica(cooldown time.Duration) *Autoscaler {
	a := &Autoscaler{Namespace: "ns", Deployment: "executor", Scale: f.scale, ReadLease: f.read, WriteLease: f.write,
		Status: func(context.Context, string, string) (int, int, int, int, error) {
			return 0, 0, 0, 0, errors.New("not observed")
		},
		Log: func(string, ...any) {}}
	a.defaults()
	a.IdleCooldown = cooldown // defaults() only fills a zero; set after, as the tests below need tiny ones
	return a
}

func TestAutoscaler_markActiveScalesUpImmediately(t *testing.T) {
	rs := &recScaler{last: -1}
	a := &Autoscaler{Namespace: "ns", Deployment: "executor", Scale: rs.scale, WriteLease: (&fakeDeploy{}).write}
	a.defaults()
	a.MarkActive(context.Background())
	if got, _ := rs.snap(); got != 3 {
		t.Errorf("MarkActive should scale to the active floor 3, got %d", got)
	}
}

func TestAutoscaler_idleReconcileScalesDown(t *testing.T) {
	rs := &recScaler{last: -1}
	fd := &fakeDeploy{}
	a := &Autoscaler{Namespace: "ns", Deployment: "executor", IdleCooldown: 10 * time.Millisecond, Scale: rs.scale, ReadLease: fd.read, WriteLease: fd.write}
	a.defaults()
	a.IdleCooldown = 10 * time.Millisecond // defaults() reset it; re-apply for the test
	// Was active, then done; after the (tiny) cooldown a reconcile scales down to 1.
	a.MarkActive(context.Background())
	a.MarkDone(context.Background())
	time.Sleep(20 * time.Millisecond)
	a.reconcile(context.Background())
	if got, _ := rs.snap(); got != 1 {
		t.Errorf("after the idle cooldown the reconcile should scale to 1, got %d", got)
	}
}

func TestAutoscaler_idempotent_noRepeatedPatches(t *testing.T) {
	rs := &recScaler{last: -1}
	fd := &fakeDeploy{}
	a := &Autoscaler{Namespace: "ns", Deployment: "executor", Scale: rs.scale, ReadLease: fd.read, WriteLease: fd.write}
	a.defaults()
	a.MarkActive(context.Background()) // → 3, 1 patch
	a.reconcile(context.Background())  // still active-recent → 3, no new patch
	a.reconcile(context.Background())  // ditto
	if _, n := rs.snap(); n != 1 {
		t.Errorf("desired unchanged must not re-patch: patch count = %d, want 1", n)
	}
}

// ── F10 / UC196 (CP-M3-III-66): report the replicas we GOT, not the ones we asked for ────────────
//
// The autoscaler PATCHed spec.replicas and never looked again, so every surface quoting a replica
// count was quoting the REQUEST. On the managed tier the request is 3 and the reality is 2 — the
// per-instance ResourceQuota cannot hold a third — so the product claimed min-3 while running two,
// and the shortfall was visible only to somebody who ran `kubectl get deploy`.

func TestAutoscaler_ObservesWhatItActuallyGot(t *testing.T) {
	a := &Autoscaler{
		Namespace: "ns", Deployment: "executor",
		Scale: func(context.Context, string, string, int) error { return nil },
		// desired=3 total=3 ready=2 updated=2. A shortfall AND a half-landed rollout at once — both
		// true, and neither implies the other.
		Status: func(context.Context, string, string) (int, int, int, int, error) { return 3, 3, 2, 2, nil },
		Log:    func(string, ...any) {},
	}
	if _, _, _, _, ok := a.Observed(); ok {
		t.Fatal("Observed() must report ok=false before anything has been read — \"not measured\" is not zero")
	}
	a.observe(context.Background())
	d, tot, r, _, ok := a.Observed()
	if !ok || d != 3 || r != 2 || tot != 3 {
		t.Fatalf("Observed() = (%d, %d, %v), want (3, 2, true) — the SHORTFALL is the whole point", d, r, ok)
	}
}

func TestAutoscaler_AFailedReadKeepsTheLastObservation(t *testing.T) {
	fail := false
	a := &Autoscaler{
		Namespace: "ns", Deployment: "executor",
		Scale: func(context.Context, string, string, int) error { return nil },
		Status: func(context.Context, string, string) (int, int, int, int, error) {
			if fail {
				return 0, 0, 0, 0, errors.New("apiserver unreachable")
			}
			return 3, 3, 3, 3, nil
		},
		Log: func(string, ...any) {},
	}
	a.observe(context.Background())
	fail = true
	a.observe(context.Background())
	d, tot, r, _, ok := a.Observed()
	if !ok || d != 3 || r != 3 || tot != 3 {
		t.Errorf("Observed() = (%d, %d, %v), want the PREVIOUS (3, 3, true) — \"I could not look\" must not be reported as \"there are none\"", d, r, ok)
	}
}

func TestAutoscaler_NoStatusReaderMeansNotReported(t *testing.T) {
	// compose, and any cluster whose SA cannot GET deployments: nothing is claimed at all.
	a := &Autoscaler{Namespace: "ns", Deployment: "executor",
		Scale: func(context.Context, string, string, int) error { return nil }, Log: func(string, ...any) {}}
	a.observe(context.Background())
	if _, _, _, _, ok := a.Observed(); ok {
		t.Error("Observed() must stay ok=false with no Status reader — a surface must say \"not reported\", never invent 1/1")
	}
}

// ── AC-D43 / issue #207: a replica must not scale the instance down under another replica's run ─────

// scaledDownAfterUp reports whether the history ever went back below the active floor after reaching it.
func scaledDownAfterUp(h []int) bool {
	up := false
	for _, n := range h {
		if n == 3 {
			up = true
		}
		if up && n < 3 {
			return true
		}
	}
	return false
}

// The measured sequence: the run holder scales 1→3, and a pod the scale-up created ticks with empty
// memory. Before the fix its first tick patched the Deployment back to 1 — 21 s after the scale-up.
func TestAC_D43_AFreshReplicaDoesNotScaleDownUnderARun(t *testing.T) {
	fd := &fakeDeploy{replicas: 1}
	holder := fd.replica(3 * time.Minute)
	holder.MarkActive(context.Background())
	fresh := fd.replica(3 * time.Minute) // created by that scale-up: active=false, lastActive zero
	fresh.tick(context.Background())
	fresh.tick(context.Background())
	if n, h := fd.snap(); n != 3 || scaledDownAfterUp(h) {
		t.Fatalf("a fresh replica scaled the instance down while another replica holds a run: replicas=%d history=%v", n, h)
	}
}

// A run longer than the cooldown: seeding a fresh replica's clock at start-up (proposal 1 alone) would
// only move the scale-down to one cooldown later. The holder's renewals keep the lease ahead of it.
func TestAC_D43_ARunLongerThanTheCooldownIsNotCutShort(t *testing.T) {
	cd := 40 * time.Millisecond
	fd := &fakeDeploy{replicas: 1}
	holder := fd.replica(cd)
	holder.MarkActive(context.Background())
	fresh := fd.replica(cd)
	for i := 0; i < 8; i++ { // 8 × 15 ms = 3 cooldowns
		time.Sleep(15 * time.Millisecond)
		holder.tick(context.Background())
		fresh.tick(context.Background())
	}
	if n, h := fd.snap(); n != 3 || scaledDownAfterUp(h) {
		t.Fatalf("the instance was scaled down during a run three cooldowns long: replicas=%d history=%v", n, h)
	}
	// …and once the run ends and the cooldown passes, the instance still parks.
	holder.MarkDone(context.Background())
	time.Sleep(cd + 20*time.Millisecond)
	fresh.tick(context.Background())
	if n, _ := fd.snap(); n != 1 {
		t.Errorf("after the run ended and the cooldown passed, the instance must park at 1, got %d", n)
	}
}

// "I could not look" is not "nobody is running": an unreadable lease never licenses a scale-down.
func TestAC_D43_AnUnreadableLeaseNeverScalesDown(t *testing.T) {
	fd := &fakeDeploy{replicas: 3, leaseErr: errors.New("apiserver unreachable")}
	fresh := fd.replica(3 * time.Minute)
	fresh.tick(context.Background())
	if n, h := fd.snap(); n != 3 || len(h) != 0 {
		t.Errorf("a replica that could not read the lease must not scale down: replicas=%d history=%v", n, h)
	}
}

// A holder that dies stops renewing; the lease lapses and the instance parks — no permanent min-3.
func TestAC_D43_ACrashedHoldersLeaseLapses(t *testing.T) {
	cd := 20 * time.Millisecond
	fd := &fakeDeploy{replicas: 1}
	holder := fd.replica(cd)
	holder.MarkActive(context.Background()) // …and then the process dies: no more ticks, no MarkDone
	fresh := fd.replica(cd)
	time.Sleep(cd + 20*time.Millisecond)
	fresh.tick(context.Background())
	if n, _ := fd.snap(); n != 1 {
		t.Errorf("a lease nobody renews must lapse within one cooldown and let the instance park, got replicas=%d", n)
	}
}

// Order matters: the pods a scale-up creates read the lease on their first tick, so it must already be
// there when the scale-up is requested.
func TestAC_D43_TheLeaseIsWrittenBeforeTheScaleUp(t *testing.T) {
	fd := &fakeDeploy{replicas: 1}
	var leaseAtScale time.Time
	a := fd.replica(3 * time.Minute)
	a.Scale = func(ctx context.Context, ns, name string, n int) error {
		leaseAtScale, _ = fd.read(ctx, ns, name)
		return fd.scale(ctx, ns, name, n)
	}
	a.MarkActive(context.Background())
	if !leaseAtScale.After(time.Now()) {
		t.Errorf("the scale-up to 3 was requested before the lease was written (lease at scale time: %v)", leaseAtScale)
	}
}
