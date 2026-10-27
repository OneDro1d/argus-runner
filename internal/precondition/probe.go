package precondition

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// Labels on everything a probe creates. LabelProbe marks "created by a precondition probe" (the stale sweep
// at the start of a probe uses it); LabelRun scopes one probe's own objects (the cleanup sweep uses it).
const (
	LabelProbe = "argus.onedroid.ai/precondition-probe"
	LabelRun   = "argus.onedroid.ai/precondition-run"
	LabelRole  = "argus.onedroid.ai/precondition-role"
	// LabelIsolated marks the INGRESS listener. internal/k8srender excludes pods carrying it from the
	// namespace's allow-same-namespace policy, so the namespace's own default-deny-ingress applies to the
	// listener and the probe's explicit allow/deny policies are the only thing deciding its ingress.
	LabelIsolated = "argus.onedroid.ai/probe-isolated"
)

// Config holds the probe's image and bounds. Zero values take the defaults (withDefaults).
type Config struct {
	// Image runs the listener and the client. It needs busybox's `nc` (listen with -ll -e, connect with -z -w).
	Image string
	// Port the listener binds.
	Port int
	// ReadyTimeout bounds waiting for a pod to run (listener) or to finish (client).
	ReadyTimeout time.Duration
	// ConnectTimeout is one connection attempt; Attempts of them make one control. A control is "connected" on the
	// first success and "not connected" only when every attempt failed - a listener that is slow to bind must not
	// read as "refused".
	ConnectTimeout time.Duration
	Attempts       int
	// Settle is the pause after a NetworkPolicy changes and before the next client starts, for the policy
	// engine to program the change. A control is only as good as this wait; docs/preconditions.md says so.
	Settle time.Duration
	// Poll is how often a pod's state is read.
	Poll time.Duration
}

// Defaults.
const (
	// DefaultImage is pinned by DIGEST (the multi-arch index of busybox:1.36.1, read from Docker Hub
	// 2026-10-13 with `docker buildx imagetools inspect`). The probe's verdict reaches a certificate, so the
	// image that produced it must not be swappable under a moving tag (TestDefaultImageIsDigestPinned).
	DefaultImage          = "docker.io/library/busybox:1.36.1@sha256:73aaf090f3d85aa34ee199857f03fa3a95c8ede2ffd4cc2cdb5b94e566b11662"
	DefaultPort           = 8080
	DefaultReadyTimeout   = 90 * time.Second
	DefaultConnectTimeout = 2 * time.Second
	DefaultAttempts       = 4
	DefaultSettle         = 5 * time.Second
	DefaultPoll           = 1 * time.Second
	cleanupTimeout        = 30 * time.Second
)

func (c Config) withDefaults() Config {
	if c.Image == "" {
		c.Image = DefaultImage
	}
	if c.Port == 0 {
		c.Port = DefaultPort
	}
	if c.ReadyTimeout <= 0 {
		c.ReadyTimeout = DefaultReadyTimeout
	}
	if c.ConnectTimeout <= 0 {
		c.ConnectTimeout = DefaultConnectTimeout
	}
	if c.Attempts <= 0 {
		c.Attempts = DefaultAttempts
	}
	if c.Settle < 0 {
		c.Settle = 0
	}
	if c.Poll <= 0 {
		c.Poll = DefaultPoll
	}
	return c
}

// Prober runs the built-in probes against ONE namespace: the executor's own.
type Prober struct {
	Kube Kube
	// NS is the executor's own instance namespace. Every object is created, listed and deleted there and
	// nowhere else.
	NS  string
	Cfg Config
	// token mints a run token; tests pin it.
	token func() string
}

func newToken() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Probe runs one precondition and returns its result. It never returns an error: every failure is a `broken`
// result with the reason, so the caller has one shape to handle. It removes everything it created on every
// path, including a cancelled ctx (cleanup runs on a fresh, bounded context).
func (p *Prober) Probe(ctx context.Context, pc scenario.Precondition) (res Result) {
	cfg := p.Cfg.withDefaults()
	res = Result{ID: pc.ID, Kind: pc.Kind, Verdict: Broken, Positive: "not run", Negative: "not run"}
	if p.NS == "" || p.Kube == nil {
		res.Positive = "not run: the executor's own namespace is unknown (not running in a pod?)"
		return res
	}
	tok := newToken
	if p.token != nil {
		tok = p.token
	}
	run := tok()
	r := &probeRun{p: p, cfg: cfg, run: run}

	// Always remove what this probe created, whatever happens below. A fresh context: a cancelled run context
	// must not be the reason a pod is left behind.
	defer func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		if left := r.cleanup(cctx); left != "" {
			res.Residue = left
		}
	}()

	// A probe that crashed on an earlier run may have left objects; they are ours (the label) and in our own
	// namespace, so they go first and cannot collide with this run's.
	_ = r.sweep(ctx, LabelProbe)

	switch pc.Kind {
	case scenario.PreconditionIngressEnforced:
		return r.ingress(ctx, res)
	case scenario.PreconditionEgressEnforced:
		return r.egress(ctx, res)
	default:
		res.Positive = fmt.Sprintf("not run: %q is not a built-in precondition kind", pc.Kind)
		return res
	}
}

type probeRun struct {
	p   *Prober
	cfg Config
	run string
	// created tracks names for the direct-delete half of cleanup; the label sweep is the other half.
	pods, policies []string
}

func (r *probeRun) name(role string) string { return "argus-pc-" + r.run + "-" + role }

func (r *probeRun) labels(role string, extra ...string) map[string]string {
	m := map[string]string{LabelProbe: "true", LabelRun: r.run, LabelRole: role}
	for i := 0; i+1 < len(extra); i += 2 {
		m[extra[i]] = extra[i+1]
	}
	return m
}

func (r *probeRun) sweep(ctx context.Context, key string) error {
	sel := key
	var firstErr error
	if names, err := r.p.Kube.ListPods(ctx, r.p.NS, sel); err != nil {
		firstErr = err
	} else {
		for _, n := range names {
			if err := r.p.Kube.DeletePod(ctx, r.p.NS, n); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	if pols, err := r.p.Kube.ListPolicies(ctx, r.p.NS, sel); err != nil {
		if firstErr == nil {
			firstErr = err
		}
	} else {
		for _, pol := range pols {
			if err := r.p.Kube.DeletePolicy(ctx, r.p.NS, pol.Name); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// cleanup deletes by name (what this run created) and then sweeps by this run's label (anything it created
// that a failed create call left half-made). It returns "" when nothing is left behind.
func (r *probeRun) cleanup(ctx context.Context) string {
	var bad []string
	for _, n := range r.pods {
		if err := r.p.Kube.DeletePod(ctx, r.p.NS, n); err != nil {
			bad = append(bad, "pod "+n)
		}
	}
	for _, n := range r.policies {
		if err := r.p.Kube.DeletePolicy(ctx, r.p.NS, n); err != nil {
			bad = append(bad, "networkpolicy "+n)
		}
	}
	if err := r.sweep(ctx, LabelRun+"="+r.run); err != nil {
		bad = append(bad, "label sweep: "+err.Error())
	}
	if len(bad) == 0 {
		return ""
	}
	return "may remain in the executor namespace: " + strings.Join(bad, "; ")
}

func (r *probeRun) createPod(ctx context.Context, role string, labels map[string]string, cmd []string) (string, error) {
	n := r.name(role)
	r.pods = append(r.pods, n)
	return n, r.p.Kube.CreatePod(ctx, r.p.NS, Pod{Name: n, Labels: labels, Image: r.cfg.Image, Command: cmd})
}

func (r *probeRun) createPolicy(ctx context.Context, pol Policy) error {
	r.policies = append(r.policies, pol.Name)
	return r.p.Kube.CreatePolicy(ctx, r.p.NS, pol)
}

func (r *probeRun) deletePolicy(ctx context.Context, name string) error {
	return r.p.Kube.DeletePolicy(ctx, r.p.NS, name)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// waitListener waits until the listener pod is running with an IP, and returns the IP.
func (r *probeRun) waitListener(ctx context.Context, name string) (string, error) {
	deadline := time.Now().Add(r.cfg.ReadyTimeout)
	last := "pending"
	for {
		st, err := r.p.Kube.PodStatus(ctx, r.p.NS, name)
		if err != nil {
			return "", err
		}
		switch {
		case st.Phase == "Running" && st.IP != "":
			return st.IP, nil
		case st.Phase == "Failed" || st.Phase == "Succeeded":
			return "", fmt.Errorf("the listener pod ended (%s)", st.Phase)
		}
		if st.WaitingReason != "" {
			last = st.WaitingReason
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("the listener pod did not run within %s (%s)", r.cfg.ReadyTimeout, last)
		}
		if err := sleepCtx(ctx, r.cfg.Poll); err != nil {
			return "", err
		}
	}
}

// connect runs ONE client pod to completion and reports whether it connected. err means the CLIENT could not
// be run or did not finish: that is an instrument fault (broken), not "not connected".
func (r *probeRun) connect(ctx context.Context, role string, labels map[string]string, ip string) (connected bool, obs string, err error) {
	// Up to Attempts tries of `nc -z -w <secs>`: connected on the first success, "not connected" only when all
	// fail. busybox nc exits 1 on a refused or timed-out connect; anything else is the client itself failing.
	secs := int(r.cfg.ConnectTimeout.Seconds())
	if secs < 1 {
		secs = 1
	}
	script := fmt.Sprintf("i=0; while [ $i -lt %d ]; do i=$((i+1)); if nc -z -w %d %s %d; then echo connected on attempt $i; exit 0; fi; sleep 1; done; exit 1",
		r.cfg.Attempts, secs, ip, r.cfg.Port)
	name, err := r.createPod(ctx, role, labels, []string{"sh", "-c", script})
	if err != nil {
		return false, "", fmt.Errorf("could not start the client pod: %w", err)
	}
	deadline := time.Now().Add(r.cfg.ReadyTimeout + time.Duration(r.cfg.Attempts)*(r.cfg.ConnectTimeout+time.Second))
	for {
		st, err := r.p.Kube.PodStatus(ctx, r.p.NS, name)
		if err != nil {
			return false, "", err
		}
		if st.Terminated {
			switch st.ExitCode {
			case 0:
				return true, "connected", nil
			case 1:
				return false, fmt.Sprintf("not connected in %d attempts of %s (refused or dropped)", r.cfg.Attempts, r.cfg.ConnectTimeout), nil
			default:
				return false, "", fmt.Errorf("the client pod failed with exit code %d (not a connection result)", st.ExitCode)
			}
		}
		if time.Now().After(deadline) {
			why := st.WaitingReason
			if why == "" {
				why = st.Phase
			}
			return false, "", fmt.Errorf("the client pod did not finish within the bound (%s)", why)
		}
		if err := sleepCtx(ctx, r.cfg.Poll); err != nil {
			return false, "", err
		}
	}
}

func broken(res Result, positive, negative string) Result {
	res.Verdict = Broken
	res.Positive, res.Negative = positive, negative
	return res
}

func (r *probeRun) listenerPod(ctx context.Context, isolated bool) (ip string, err error) {
	extra := []string{}
	if isolated {
		extra = append(extra, LabelIsolated, "true")
	}
	cmd := []string{"nc", "-ll", "-p", strconv.Itoa(r.cfg.Port), "-e", "/bin/true"}
	n, err := r.createPod(ctx, "listener", r.labels("listener", extra...), cmd)
	if err != nil {
		return "", fmt.Errorf("could not start the listener pod: %w", err)
	}
	return r.waitListener(ctx, n)
}

// settled waits for the policy engine to program a change.
func (r *probeRun) settled(ctx context.Context) error { return sleepCtx(ctx, r.cfg.Settle) }

// ingress: a listener that is isolated by the namespace's default-deny; the positive control opens it with an
// explicit allow policy and connects; the negative control removes the allow, applies an explicit deny that
// selects the listener, and connects again. A cluster that enforces NetworkPolicy refuses the second.
func (r *probeRun) ingress(ctx context.Context, res Result) Result {
	ip, err := r.listenerPod(ctx, true)
	if err != nil {
		return broken(res, "not run: "+err.Error(), "not run")
	}
	clientLabels := r.labels("client")
	allow := r.name("allow")
	if err := r.createPolicy(ctx, Policy{
		Name: allow, Labels: r.labels("allow"), PodSelector: LabelSelector{MatchLabels: map[string]string{LabelRun: r.run, LabelRole: "listener"}},
		Types: []string{"Ingress"}, Ingress: []Rule{{Peers: []Peer{{PodSelector: &LabelSelector{MatchLabels: map[string]string{LabelRun: r.run, LabelRole: "client"}}}}, Ports: []int{r.cfg.Port}}},
	}); err != nil {
		return broken(res, "not run: could not create the allow policy: "+err.Error(), "not run")
	}
	if err := r.settled(ctx); err != nil {
		return broken(res, "not run: "+err.Error(), "not run")
	}
	ok, obs, err := r.connect(ctx, "client", clientLabels, ip)
	if err != nil {
		return broken(res, "positive control could not be run: "+err.Error(), "not run")
	}
	if !ok {
		return broken(res, "positive control FAILED: "+obs+" although the probe's own allow policy admits it", "not run")
	}
	res.Positive = obs
	listenerLabels := r.labels("listener", LabelIsolated, "true")
	// The negative control is only trustworthy when nothing else in the namespace admits the client: a
	// NetworkPolicy is additive, so an ambient allow-all policy would keep the connection open on a cluster
	// that DOES enforce, and that would read as "absent" - excusing checks on a lie.
	if name, err := r.ambientAdmitting(ctx, "Ingress", listenerLabels, clientLabels, r.cfg.Port); err != nil {
		return broken(res, res.Positive, "not run: could not read the namespace's NetworkPolicies: "+err.Error())
	} else if name != "" {
		return broken(res, res.Positive, fmt.Sprintf("not run: NetworkPolicy %q in the executor namespace admits the probe client regardless of a deny, so ingress enforcement cannot be told apart here", name))
	}
	if err := r.deletePolicy(ctx, allow); err != nil {
		return broken(res, res.Positive, "not run: could not remove the allow policy: "+err.Error())
	}
	if err := r.createPolicy(ctx, Policy{
		Name: r.name("deny"), Labels: r.labels("deny"), PodSelector: LabelSelector{MatchLabels: map[string]string{LabelRun: r.run, LabelRole: "listener"}},
		Types: []string{"Ingress"}, // no ingress rules: nothing is admitted
	}); err != nil {
		return broken(res, res.Positive, "not run: could not create the deny policy: "+err.Error())
	}
	return r.negative(ctx, res, "client2", clientLabels, ip)
}

// egress: the mirror. The client's egress is open (positive control); then an explicit deny selects the client
// and it connects again (negative control).
func (r *probeRun) egress(ctx context.Context, res Result) Result {
	ip, err := r.listenerPod(ctx, false)
	if err != nil {
		return broken(res, "not run: "+err.Error(), "not run")
	}
	clientLabels := r.labels("client")
	// The listener must admit the client whatever the namespace's ingress policy is, or the positive control
	// would fail for an ingress reason and read as an egress one.
	if err := r.createPolicy(ctx, Policy{
		Name: r.name("allow"), Labels: r.labels("allow"), PodSelector: LabelSelector{MatchLabels: map[string]string{LabelRun: r.run, LabelRole: "listener"}},
		Types: []string{"Ingress"}, Ingress: []Rule{{Peers: []Peer{{PodSelector: &LabelSelector{MatchLabels: map[string]string{LabelRun: r.run, LabelRole: "client"}}}}, Ports: []int{r.cfg.Port}}},
	}); err != nil {
		return broken(res, "not run: could not create the allow policy: "+err.Error(), "not run")
	}
	if err := r.settled(ctx); err != nil {
		return broken(res, "not run: "+err.Error(), "not run")
	}
	ok, obs, err := r.connect(ctx, "client", clientLabels, ip)
	if err != nil {
		return broken(res, "positive control could not be run: "+err.Error(), "not run")
	}
	if !ok {
		return broken(res, "positive control FAILED: "+obs+" with no egress policy of the probe's own in the way", "not run")
	}
	res.Positive = obs
	listenerLabels := r.labels("listener")
	if name, err := r.ambientAdmitting(ctx, "Egress", clientLabels, listenerLabels, r.cfg.Port); err != nil {
		return broken(res, res.Positive, "not run: could not read the namespace's NetworkPolicies: "+err.Error())
	} else if name != "" {
		return broken(res, res.Positive, fmt.Sprintf("not run: NetworkPolicy %q in the executor namespace admits the probe's egress regardless of a deny, so egress enforcement cannot be told apart here", name))
	}
	if err := r.createPolicy(ctx, Policy{
		Name: r.name("deny"), Labels: r.labels("deny"), PodSelector: LabelSelector{MatchLabels: map[string]string{LabelRun: r.run, LabelRole: "client"}},
		Types: []string{"Egress"}, // no egress rules: nothing may leave
	}); err != nil {
		return broken(res, res.Positive, "not run: could not create the deny policy: "+err.Error())
	}
	return r.negative(ctx, res, "client2", clientLabels, ip)
}

// negative settles, connects once more and classifies: refused => holds; connected => absent.
func (r *probeRun) negative(ctx context.Context, res Result, role string, labels map[string]string, ip string) Result {
	if err := r.settled(ctx); err != nil {
		return broken(res, res.Positive, "not run: "+err.Error())
	}
	ok, obs, err := r.connect(ctx, role, labels, ip)
	if err != nil {
		return broken(res, res.Positive, "negative control could not be run: "+err.Error())
	}
	res.Negative = obs
	if ok {
		res.Verdict = Absent
		res.Negative = "NOT refused: " + obs + " although a deny policy selects it"
	} else {
		res.Verdict = Holds
	}
	return res
}
