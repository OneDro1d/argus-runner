package precondition

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// fakeKube is a tiny cluster: it keeps pods and policies and answers a client pod's exit code from `mode`.
type fakeKube struct {
	mu       sync.Mutex
	mode     string // enforcing | none | dead
	pods     map[string]Pod
	policies map[string]Policy
	ambient  []Policy
	// failCreate returns an error when a created object's name has this suffix ("" = never).
	failCreate string
	// onClient is called when a client pod's state is first read (tests cancel contexts from it).
	onClient   func()
	everPods   []string
	everPolicy []string
	namespaces map[string]bool
}

func newFake(mode string) *fakeKube {
	return &fakeKube{mode: mode, pods: map[string]Pod{}, policies: map[string]Policy{}, namespaces: map[string]bool{}}
}

func (f *fakeKube) CreatePod(_ context.Context, ns string, p Pod) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.namespaces[ns] = true
	if f.failCreate != "" && strings.HasSuffix(p.Name, f.failCreate) {
		return errors.New("boom")
	}
	f.pods[p.Name] = p
	f.everPods = append(f.everPods, p.Name)
	return nil
}

func (f *fakeKube) CreatePolicy(_ context.Context, ns string, p Policy) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.namespaces[ns] = true
	if f.failCreate != "" && strings.HasSuffix(p.Name, f.failCreate) {
		return errors.New("boom")
	}
	f.policies[p.Name] = p
	f.everPolicy = append(f.everPolicy, p.Name)
	return nil
}

func (f *fakeKube) PodStatus(ctx context.Context, ns, name string) (PodStatus, error) {
	f.mu.Lock()
	p, ok := f.pods[name]
	f.mu.Unlock()
	if !ok {
		return PodStatus{}, errors.New("not found")
	}
	if p.Labels[LabelRole] == "listener" {
		return PodStatus{Phase: "Running", IP: "10.0.0.9"}, nil
	}
	if f.onClient != nil {
		f.onClient()
	}
	if err := ctx.Err(); err != nil {
		return PodStatus{}, err
	}
	if f.connects(p) {
		return PodStatus{Phase: "Succeeded", Terminated: true, ExitCode: 0}, nil
	}
	return PodStatus{Phase: "Failed", Terminated: true, ExitCode: 1}, nil
}

// connects decides whether a client pod reaches the listener, given the policies present right now.
func (f *fakeKube) connects(client Pod) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch f.mode {
	case "none":
		return true
	case "dead":
		return false
	}
	// enforcing: ingress is refused unless an Ingress policy WITH rules selects the listener; egress is refused
	// when an Egress policy selects the client.
	allowed := false
	for _, pol := range f.policies {
		for _, ty := range pol.Types {
			if ty == "Ingress" && len(pol.Ingress) > 0 {
				allowed = true
			}
			if ty == "Egress" && pol.PodSelector.Matches(client.Labels) {
				return false
			}
		}
	}
	for _, p := range f.pods {
		if p.Labels[LabelIsolated] == "true" {
			return allowed
		}
	}
	return true
}

func (f *fakeKube) ListPods(_ context.Context, _ string, selector string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for n, p := range f.pods {
		if selMatches(selector, p.Labels) {
			out = append(out, n)
		}
	}
	return out, nil
}

func (f *fakeKube) ListPolicies(_ context.Context, _ string, selector string) ([]Policy, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Policy
	for _, p := range f.policies {
		if selMatches(selector, p.Labels) {
			out = append(out, p)
		}
	}
	if selector == "" {
		out = append(out, f.ambient...)
	}
	return out, nil
}

func selMatches(sel string, labels map[string]string) bool {
	if sel == "" {
		return true
	}
	k, v, eq := strings.Cut(sel, "=")
	if !eq {
		_, has := labels[k]
		return has
	}
	return labels[k] == v
}

func (f *fakeKube) DeletePod(ctx context.Context, _ string, n string) error {
	if err := ctx.Err(); err != nil { // like the real client: a cancelled context sends nothing
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.pods, n)
	return nil
}

func (f *fakeKube) DeletePolicy(ctx context.Context, _ string, n string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.policies, n)
	return nil
}

func (f *fakeKube) left() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.pods) + len(f.policies)
}

func prober(f *fakeKube) *Prober {
	return &Prober{Kube: f, NS: "argus-inst-x", Cfg: Config{Settle: time.Nanosecond, Poll: time.Millisecond, ReadyTimeout: time.Second}, token: func() string { return "t0" }}
}

var (
	ingress = scenario.Precondition{ID: "np-in", Kind: scenario.PreconditionIngressEnforced}
	egress  = scenario.Precondition{ID: "np-out", Kind: scenario.PreconditionEgressEnforced}
)

func TestProbe_ThreeVerdicts(t *testing.T) {
	for _, tc := range []struct {
		name string
		pc   scenario.Precondition
		mode string
		want string
	}{
		{"ingress enforced", ingress, "enforcing", Holds},
		{"ingress not enforced", ingress, "none", Absent},
		{"ingress dead", ingress, "dead", Broken},
		{"egress enforced", egress, "enforcing", Holds},
		{"egress not enforced", egress, "none", Absent},
		{"egress dead", egress, "dead", Broken},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(tc.mode)
			r := prober(f).Probe(context.Background(), tc.pc)
			if r.Verdict != tc.want {
				t.Fatalf("verdict = %s, want %s (%s)", r.Verdict, tc.want, r.Reason())
			}
			if tc.want == Absent && !strings.Contains(r.Negative, "NOT refused") {
				t.Errorf("an absent mechanism must show the negative control NOT refused: %q", r.Negative)
			}
			if tc.want == Broken && !strings.Contains(r.Positive, "FAILED") {
				t.Errorf("a dead positive control must say so: %q", r.Positive)
			}
			if tc.want == Broken && r.Negative != "not run" {
				t.Errorf("the negative control must not run when the positive failed: %q", r.Negative)
			}
			if n := f.left(); n != 0 {
				t.Fatalf("%d objects left behind", n)
			}
		})
	}
}

// Every object is created in the executor's own namespace and nowhere else.
func TestProbe_OnlyTheExecutorsOwnNamespace(t *testing.T) {
	f := newFake("enforcing")
	prober(f).Probe(context.Background(), ingress)
	if len(f.namespaces) != 1 || !f.namespaces["argus-inst-x"] {
		t.Fatalf("namespaces touched: %v", f.namespaces)
	}
}

// A probe with no namespace (not in a pod) never touches anything and is broken, not absent.
func TestProbe_NoNamespaceIsBroken(t *testing.T) {
	f := newFake("none")
	p := prober(f)
	p.NS = ""
	if r := p.Probe(context.Background(), ingress); r.Verdict != Broken || len(f.everPods) != 0 {
		t.Fatalf("%+v pods=%v", r, f.everPods)
	}
}

// Cleanup on error: a failure part-way through leaves nothing, and the result is broken, never absent.
func TestProbe_CleanupOnSetupError(t *testing.T) {
	for _, suffix := range []string{"-deny", "-allow", "-listener", "-client2"} {
		for _, pc := range []scenario.Precondition{ingress, egress} {
			f := newFake("none")
			f.failCreate = suffix
			r := prober(f).Probe(context.Background(), pc)
			if r.Verdict == Absent || r.Verdict == Holds {
				t.Errorf("%s %s: a setup failure must be broken, got %s (%s)", pc.Kind, suffix, r.Verdict, r.Reason())
			}
			if n := f.left(); n != 0 {
				t.Errorf("%s %s: %d objects left behind", pc.Kind, suffix, n)
			}
		}
	}
}

// Cleanup on context cancel: the run context is cancelled in the middle of the negative control; the probe
// still removes everything (cleanup uses a fresh context) and is broken.
func TestProbe_CleanupOnContextCancel(t *testing.T) {
	f := newFake("enforcing")
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	f.onClient = func() {
		calls++
		if calls == 2 { // inside the second client's wait
			cancel()
		}
	}
	r := prober(f).Probe(ctx, ingress)
	if r.Verdict != Broken {
		t.Fatalf("a cancelled probe is broken, got %s (%s)", r.Verdict, r.Reason())
	}
	if n := f.left(); n != 0 {
		t.Fatalf("%d objects left behind after cancel", n)
	}
}

// A leftover from a crashed earlier probe is removed at the start.
func TestProbe_StaleObjectsAreSwept(t *testing.T) {
	f := newFake("enforcing")
	f.pods["argus-pc-old-listener"] = Pod{Name: "argus-pc-old-listener", Labels: map[string]string{LabelProbe: "true", LabelRun: "old"}}
	f.policies["argus-pc-old-deny"] = Policy{Name: "argus-pc-old-deny", Labels: map[string]string{LabelProbe: "true", LabelRun: "old"}}
	prober(f).Probe(context.Background(), ingress)
	if n := f.left(); n != 0 {
		t.Fatalf("%d objects left behind", n)
	}
}

// The label sweep catches an object a failed create left half-made and the by-name list never saw.
func TestProbe_LabelSweepCatchesWhatNamesMissed(t *testing.T) {
	f := newFake("enforcing")
	p := prober(f)
	orig := f.CreatePod
	_ = orig
	// A pod created under the run's label but not recorded in r.pods: only the label sweep can find it.
	f.pods["stray"] = Pod{Name: "stray", Labels: map[string]string{LabelProbe: "true", LabelRun: "t0"}}
	r := &probeRun{p: p, cfg: p.Cfg.withDefaults(), run: "t0"}
	if left := r.cleanup(context.Background()); left != "" {
		t.Fatal(left)
	}
	if f.left() != 0 {
		t.Fatal("the label sweep left the stray pod")
	}
}

// An ambient policy that admits the probe client would keep the connection open on an ENFORCING cluster. The
// probe must say broken, never absent: reading it as absent would excuse checks on a lie.
func TestProbe_AmbientAllowPolicyIsBrokenNotAbsent(t *testing.T) {
	for _, tc := range []struct {
		pc   scenario.Precondition
		pol  Policy
		mode string
	}{
		{ingress, Policy{Name: "allow-same-namespace", PodSelector: LabelSelector{}, Types: []string{"Ingress"}, Ingress: []Rule{{Peers: []Peer{{PodSelector: &LabelSelector{}}}}}}, "none"},
		{egress, Policy{Name: "allow-all-egress", PodSelector: LabelSelector{}, Types: []string{"Egress"}, Egress: []Rule{{}}}, "none"},
	} {
		f := newFake(tc.mode)
		f.ambient = []Policy{tc.pol}
		r := prober(f).Probe(context.Background(), tc.pc)
		if r.Verdict != Broken || !strings.Contains(r.Negative, tc.pol.Name) {
			t.Errorf("%s: ambient admit must be broken naming the policy, got %s / %q", tc.pc.Kind, r.Verdict, r.Negative)
		}
		if f.left() != 0 {
			t.Errorf("%s: objects left", tc.pc.Kind)
		}
	}
}

// The namespace's allow-same-namespace as k8srender renders it (excluding the isolated listener) does NOT trip
// the guard, so ingress is probeable on a rendered instance.
func TestProbe_RenderedAllowSameNamespaceDoesNotTripTheGuard(t *testing.T) {
	f := newFake("enforcing")
	f.ambient = []Policy{
		{Name: "default-deny-ingress", PodSelector: LabelSelector{}, Types: []string{"Ingress"}},
		{Name: "allow-same-namespace", PodSelector: LabelSelector{MatchExpressions: []Expr{{Key: LabelIsolated, Operator: "DoesNotExist"}}}, Types: []string{"Ingress"}, Ingress: []Rule{{Peers: []Peer{{PodSelector: &LabelSelector{}}}}}},
	}
	if r := prober(f).Probe(context.Background(), ingress); r.Verdict != Holds {
		t.Fatalf("%s", r.Reason())
	}
}

func TestCollect_ConflictingKindsAreReported(t *testing.T) {
	all, conflicts := Collect([][]scenario.Precondition{
		{{ID: "a", Kind: scenario.PreconditionIngressEnforced}},
		{{ID: "a", Kind: scenario.PreconditionEgressEnforced}, {ID: "b", Kind: scenario.PreconditionEgressEnforced}},
	})
	if len(conflicts) != 1 || conflicts["a"] == "" || len(all) != 2 {
		t.Fatalf("all=%v conflicts=%v", all, conflicts)
	}
}

func TestUnknownKindIsBroken(t *testing.T) {
	f := newFake("none")
	if r := prober(f).Probe(context.Background(), scenario.Precondition{ID: "x", Kind: "networkpolicy.nope"}); r.Verdict != Broken || len(f.everPods) != 0 {
		t.Fatalf("%+v", r)
	}
}

// The probe's verdict reaches a certificate, so the default image must be pinned by digest: a moving tag would
// let whoever controls it decide what "the cluster enforces NetworkPolicy" means on the next pull.
func TestDefaultImageIsDigestPinned(t *testing.T) {
	_, digest, ok := strings.Cut(DefaultImage, "@sha256:")
	if !ok || len(digest) != 64 || strings.Trim(digest, "0123456789abcdef") != "" {
		t.Fatalf("DefaultImage %q is not pinned by a sha256 digest", DefaultImage)
	}
	if got := (Config{}).withDefaults().Image; got != DefaultImage {
		t.Fatalf("an unset image defaults to %q, want the pinned %q", got, DefaultImage)
	}
}
