package runner

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/OneDro1d/argus-runner/internal/rollout"
)

// Right-sizing on the k8s tiers (UC196 / §D-3.1.1 ADR-11). An idle-parked instance scales its
// executor Deployment DOWN to a cheap floor (keeping its runner__* endpoint reachable); a triggered
// run scales it back to min-3 (active instances honour min-3). Default posture = scale-idle-down.
//
// The executor scales its OWN Deployment via the same in-cluster ServiceAccount that UC069 uses for
// self-delete (one more namespaced right: patch the deployment's scale subresource). The scale calls
// are IDEMPOTENT (set replicas to N), and every replica runs this loop.
//
// ⛔ AC-D43 / issue #207: "every replica computes the same desired value" WAS FALSE, and this comment
// used to assert it. Whether a run is in flight lived in each process's memory, so the two pods a
// scale-up created started with a zero lastActive, judged the instance idle on their first tick
// (20 s), and patched it back to 1 — and Kubernetes, preferring to empty the busier node, deleted the
// pod running the test. The run ended `abandoned`, 0 of 33, with nothing saying why. Measured on
// orderservice-k3d at 0.3.37-dev+a6e360e.
//
// The instance-level fact now lives on the Deployment itself: the ACTIVE-UNTIL LEASE, an annotation on
// its metadata (not its pod template, so writing it rolls nothing). The replica holding a run writes it
// before scaling up, renews it every tick, and sets it one cooldown ahead when the run ends. A
// scale-DOWN is refused while the lease is in the future — and refused when the lease cannot be READ,
// because "I could not look" is not "nobody is running". A crashed holder stops renewing, so the lease
// lapses within one cooldown and the instance still parks. No new RBAC: the executor already has get
// and patch on its own Deployment (F15/UC069).

const (
	defaultActiveReplicas = 3               // min-3 for an ACTIVE instance
	defaultIdleReplicas   = 1               // cheap floor for a parked instance; endpoint still reachable
	defaultIdleCooldown   = 3 * time.Minute // how long after the last run before scaling down
	defaultScaleTick      = 20 * time.Second
)

// atoiEnv reads a small non-negative int from the environment; ok=false when absent or unparseable.
func atoiEnv(name string) (int, bool) {
	v := os.Getenv(name)
	if v == "" {
		return 0, false
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// activeReplicasForVolume (#215) caps the active floor at 1 when the results volume is DECLARED to be
// anything but ReadWriteMany: extra replicas land on other nodes and cannot attach it, so they sit in
// ContainerCreating for the whole run. An EMPTY mode keeps today's behaviour: it is any instance rendered
// before the renderer passed ARGUS_RESULTS_ACCESS_MODE, and those include ReadWriteMany ones that scale
// fine — capping them would take away their min-3 without telling anyone. An old ReadWriteOnce instance
// gets the cap at its next render, or by setting the env on its Deployment.
func activeReplicasForVolume(accessMode string, activeN int) (int, bool) {
	if accessMode == "" || accessMode == "ReadWriteMany" {
		return activeN, false
	}
	return 1, true
}

// newAutoscalerFromEnv builds the executor's autoscaler from its environment. An explicit
// ARGUS_SCALE_ACTIVE wins over the #215 cap: the cap is the safe default, not a lock.
func newAutoscalerFromEnv(podNS string, log func(string, ...any)) *Autoscaler {
	as := &Autoscaler{Namespace: podNS, Deployment: "executor", Log: log}
	if n, ok := atoiEnv("ARGUS_SCALE_IDLE"); ok {
		as.IdleReplicas = n
	}
	mode := os.Getenv("ARGUS_RESULTS_ACCESS_MODE")
	if n, capped := activeReplicasForVolume(mode, defaultActiveReplicas); capped {
		as.ActiveReplicas = n
		log("autoscale: capped at %d replica — results volume access mode %q is not ReadWriteMany, so extra replicas could not attach it (#215)", n, mode)
	}
	if n, ok := atoiEnv("ARGUS_SCALE_ACTIVE"); ok {
		as.ActiveReplicas = n
	}
	if d, err := time.ParseDuration(os.Getenv("ARGUS_IDLE_COOLDOWN")); err == nil && d > 0 {
		as.IdleCooldown = d
	}
	if d, err := time.ParseDuration(os.Getenv("ARGUS_SCALE_TICK")); err == nil && d > 0 {
		as.Tick = d
	}
	return as
}

// computeDesiredReplicas is the pure right-sizing decision. Active (a run in flight) OR active within
// the cooldown → the active floor (min-3); otherwise the idle floor. IO-free so the rule is testable.
func computeDesiredReplicas(active bool, lastActive, now time.Time, cooldown time.Duration, idleN, activeN int) int {
	if active || now.Sub(lastActive) < cooldown {
		return activeN
	}
	return idleN
}

// Autoscaler right-sizes ONE executor Deployment. MarkActive is called by the run path the moment a
// run begins (federated pickup or direct run); Run() is the reconcile loop.
type Autoscaler struct {
	Namespace, Deployment string
	IdleReplicas          int
	ActiveReplicas        int
	IdleCooldown          time.Duration
	Tick                  time.Duration
	// Scale applies a replica count; defaults to the real in-cluster scale patch. Injectable for tests.
	Scale func(ctx context.Context, ns, name string, replicas int) error
	// Status reads the Deployment back: (spec.replicas, status.readyReplicas, status.updatedReplicas).
	// nil = no observation (unit tests, or a cluster where the SA cannot GET deployments) and
	// Observed() stays ok=false.
	//
	// updatedReplicas (VR-F3) is the count running the CURRENT pod template. It is the ONLY one of the
	// three that catches a half-landed rollout: live 2026-08-10 two k3d instances read desired=1
	// ready=1 while serving the PREVIOUS image, because the old pod was ready and the new one was
	// Pending on a node that could not pull it.
	Status func(ctx context.Context, ns, name string) (desired, total, ready, updated int, err error)
	// ReadLease / WriteLease are the SHARED run state (AC-D43): the active-until lease on the Deployment.
	// ReadLease returns the zero time when no lease was ever written. Both default to the in-cluster
	// API; injectable for tests.
	ReadLease  func(ctx context.Context, ns, name string) (time.Time, error)
	WriteLease func(ctx context.Context, ns, name string, until time.Time) error
	Log        func(string, ...any)

	mu          sync.Mutex
	active      bool
	lastActive  time.Time
	lastApplied int // last replica count we patched (-1 = none yet), to avoid spamming the API
	// F10/UC196: what the cluster last SAID, as opposed to what we asked for.
	observed   bool
	obsDesired int
	obsReady   int
	obsUpdated int // VR-F3: replicas on the CURRENT pod template
	obsTotal   int // status.replicas: TOTAL non-terminated — the one that sees a stall (021)
}

func (a *Autoscaler) defaults() {
	if a.ActiveReplicas == 0 {
		a.ActiveReplicas = defaultActiveReplicas
	}
	if a.IdleReplicas == 0 {
		a.IdleReplicas = defaultIdleReplicas
	}
	if a.IdleCooldown == 0 {
		a.IdleCooldown = defaultIdleCooldown
	}
	if a.Tick == 0 {
		a.Tick = defaultScaleTick
	}
	if a.Scale == nil {
		a.Scale = scaleDeployment
	}
	if a.Status == nil {
		a.Status = deploymentStatus // F10/UC196: read back what we actually got
	}
	if a.ReadLease == nil {
		a.ReadLease = readActiveLease
	}
	if a.WriteLease == nil {
		a.WriteLease = writeActiveLease
	}
	if a.Log == nil {
		a.Log = func(string, ...any) {}
	}
	a.lastApplied = -1
}

// MarkActive records that a run is in flight and immediately scales UP to the active floor, so the
// active instance honours min-3 without waiting for the next tick.
//
// The lease is written BEFORE the scale-up: the pods the scale-up creates read it on their first tick,
// and a lease written after them would race the very pods it exists to stop.
func (a *Autoscaler) MarkActive(ctx context.Context) {
	a.mu.Lock()
	a.active = true
	a.lastActive = time.Now().UTC()
	a.mu.Unlock()
	a.renewLease(ctx)
	a.reconcile(ctx)
}

// MarkDone records that the run finished; the instance stays "recently active" for the cooldown, then
// the reconcile loop scales it down. The lease carries that cooldown to every replica.
func (a *Autoscaler) MarkDone(ctx context.Context) {
	a.mu.Lock()
	a.active = false
	a.lastActive = time.Now().UTC()
	a.mu.Unlock()
	a.renewLease(ctx)
}

// renewLease sets the shared active-until lease one cooldown ahead. A failure is LOUD: without the
// lease a freshly created replica cannot see this run and may scale the instance down under it.
func (a *Autoscaler) renewLease(ctx context.Context) {
	if a.WriteLease == nil {
		return
	}
	until := time.Now().UTC().Add(a.IdleCooldown)
	if err := a.WriteLease(ctx, a.Namespace, a.Deployment, until); err != nil {
		a.Log("autoscale: could NOT write the active lease on %s (%v) — other replicas cannot see this run and may scale down under it", a.Deployment, err)
	}
}

// Observed returns the last (desired, ready) this autoscaler READ BACK from the cluster, and whether
// it has read anything yet (F10/UC196).
//
// The autoscaler used to PATCH spec.replicas and never look again, so every surface that mentioned
// replicas was quoting the REQUEST. On the managed tier the request is 3 and the reality is 2 — the
// per-instance ResourceQuota cannot hold a third — so the product claimed min-3 while running two.
// A number that is always the number you asked for is not a measurement.
func (a *Autoscaler) Observed() (desired, total, ready, updated int, ok bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.obsDesired, a.obsTotal, a.obsReady, a.obsUpdated, a.observed
}

// observe reads the Deployment back. Best-effort: a failure leaves the previous observation in place
// rather than replacing it with a zero, because "I could not look" is not "there are none".
func (a *Autoscaler) observe(ctx context.Context) {
	if a.Status == nil {
		return
	}
	d, tot, r, u, err := a.Status(ctx, a.Namespace, a.Deployment)
	if err != nil {
		a.Log("autoscale: could not read %s back (%v) — keeping the previous observation", a.Deployment, err)
		return
	}
	a.mu.Lock()
	changed := !a.observed || a.obsDesired != d || a.obsReady != r || a.obsUpdated != u || a.obsTotal != tot
	a.obsDesired, a.obsTotal, a.obsReady, a.obsUpdated, a.observed = d, tot, r, u, true
	a.mu.Unlock()
	if !changed {
		return
	}
	v := rollout.Classify(d, tot, r, u)
	// AC-D51: a SHORTFALL only — fewer ready than desired. This used to fire on `d != r`, so the same
	// park that tripped the half-landed line below also printed "3/1 replicas READY — the shortfall is
	// reported": a surplus still being torn down, announced as a shortfall.
	if v.ReadinessShortfall {
		a.Log("autoscale: %s has %d/%d replicas READY — the shortfall is reported, not rounded up", a.Deployment, r, d)
	}
	// VR-F3 / AC-D51 (issue #315): ready-but-not-updated is the state that looks healthy and is not —
	// said separately from the readiness shortfall above, because the remedy is different: this one is
	// an image the node cannot resolve, not a resource shortfall. Classify is the SAME rule the control
	// plane's rolloutState uses (internal/rollout), so the two can no longer disagree.
	//
	// Before internal/rollout existed this condition was `u < d || tot > d`, which is exactly the bug
	// #315 reports: `tot > d` is true for about a second on every ordinary post-run scale-down (3 -> 1)
	// — Kubernetes has not finished tearing down the surplus pods yet, even though every one of them
	// already runs the current template — and it printed "HALF-LANDED — 0 pod(s) still run the PREVIOUS
	// image" on a perfectly healthy park. Classify's StalePods (total-updated) is exactly the count of
	// pods NOT on the current template, so it is 0 on that tuple and the line does not fire; it is > 0
	// only when an old pod genuinely remains (measured 2026-08-10: spec=1 total=2 ready=1 updated=1).
	if v.State == rollout.HalfLanded {
		a.Log("autoscale: %s is a HALF-LANDED rollout — %d pod(s) still run the PREVIOUS image (spec=%d total=%d ready=%d updated=%d; ready does not mean upgraded)", a.Deployment, v.StalePods, d, tot, r, u)
	}
}

func (a *Autoscaler) reconcile(ctx context.Context) {
	now := time.Now().UTC()
	a.mu.Lock()
	desired := computeDesiredReplicas(a.active, a.lastActive, now, a.IdleCooldown, a.IdleReplicas, a.ActiveReplicas)
	// The same test computeDesiredReplicas makes, kept for the log: the floors can be equal, so the
	// count alone cannot say which branch chose it.
	active := a.active || now.Sub(a.lastActive) < a.IdleCooldown
	if desired == a.lastApplied {
		a.mu.Unlock()
		return
	}
	a.mu.Unlock()
	// AC-D43: THIS replica's memory says idle — which is also what a pod created one tick ago says while
	// another pod is mid-run. Before scaling DOWN, ask the instance through the shared lease.
	if desired < a.ActiveReplicas && a.ReadLease != nil {
		until, err := a.ReadLease(ctx, a.Namespace, a.Deployment)
		if err != nil {
			a.Log("autoscale: could not read the active lease on %s (%v) — NOT scaling down; will retry next tick", a.Deployment, err)
			return
		}
		if until.After(now) {
			desired = a.ActiveReplicas
			active = true // another replica's run holds the lease
			a.mu.Lock()
			same := desired == a.lastApplied
			a.mu.Unlock()
			if same {
				return
			}
		}
	}
	if err := a.Scale(ctx, a.Namespace, a.Deployment, desired); err != nil {
		a.Log("autoscale: scale to %d failed (%v) — will retry next tick", desired, err)
		return
	}
	a.mu.Lock()
	a.lastApplied = desired
	a.mu.Unlock()
	a.Log("autoscale: %s scaled to %d replicas (%s)", a.Deployment, desired, scaleReason(active, a.ActiveReplicas))
}

// scaleReason names WHY a replica count was chosen, from the STATE, never from the count. The count
// cannot tell: with the #215 cap (or ARGUS_SCALE_ACTIVE) the active and idle floors can be equal, and
// a count-based label logged a parked executor as "active/min-3" while it ran one replica with no run
// in flight (argus-inst-shop-uni-arb and -shop-dev, 2026-09-24).
func scaleReason(active bool, activeN int) string {
	if active {
		return fmt.Sprintf("active, floor %d", activeN)
	}
	return "idle-parked"
}

// Run reconciles on a ticker until ctx is cancelled — the scale-DOWN path (MarkActive covers scale-up
// immediately). Safe to call once from the Loop.
func (a *Autoscaler) Run(ctx context.Context) {
	a.defaults()
	t := time.NewTicker(a.Tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.tick(ctx)
		}
	}
}

// tick is one pass of the loop: renew the lease if this replica holds a run, reconcile, then observe.
func (a *Autoscaler) tick(ctx context.Context) {
	a.mu.Lock()
	holding := a.active
	a.mu.Unlock()
	if holding {
		a.renewLease(ctx) // the run holder keeps the instance-level fact fresh for the whole run
	}
	a.reconcile(ctx)
	a.observe(ctx) // AFTER scaling, so the observation reflects the request just made
}

// activeLeaseAnnotation carries the instance-level "a run is in flight until" fact (AC-D43), RFC 3339
// UTC. On the Deployment's METADATA: an annotation on the pod template would roll every pod on write.
const activeLeaseAnnotation = "argus.onedroid.ai/active-until"

// deploymentAPI makes one in-cluster request against the executor's own Deployment with the mounted
// ServiceAccount — the same pattern (and the same get/patch rights) as deploymentStatus and
// patchDeploymentImage.
func deploymentAPI(ctx context.Context, method, namespace, name, contentType string, body []byte) (*http.Response, error) {
	const saDir = "/var/run/secrets/kubernetes.io/serviceaccount"
	token, err := os.ReadFile(filepath.Join(saDir, "token"))
	if err != nil {
		return nil, fmt.Errorf("read SA token: %w", err)
	}
	caPEM, err := os.ReadFile(filepath.Join(saDir, "ca.crt"))
	if err != nil {
		return nil, fmt.Errorf("read SA CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("SA CA is not valid PEM")
	}
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" {
		host, port = "kubernetes.default.svc", "443"
	}
	url := fmt.Sprintf("https://%s:%s/apis/apps/v1/namespaces/%s/deployments/%s", host, port, namespace, name)
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+string(token))
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	hc := &http.Client{
		Timeout:   15 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}},
	}
	return hc.Do(req)
}

// readActiveLease returns the Deployment's active-until lease; the zero time when none was written.
// A lease that does not parse is an ERROR, not "no lease": reading garbage as idle is the defect.
func readActiveLease(ctx context.Context, namespace, name string) (time.Time, error) {
	resp, err := deploymentAPI(ctx, http.MethodGet, namespace, name, "", nil)
	if err != nil {
		return time.Time{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return time.Time{}, fmt.Errorf("k8s API GET deployment returned %s", resp.Status)
	}
	var d struct {
		Metadata struct {
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&d); err != nil {
		return time.Time{}, err
	}
	v, ok := d.Metadata.Annotations[activeLeaseAnnotation]
	if !ok || v == "" {
		return time.Time{}, nil
	}
	until, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		return time.Time{}, fmt.Errorf("annotation %s=%q is not RFC 3339: %w", activeLeaseAnnotation, v, err)
	}
	return until, nil
}

// writeActiveLease merge-patches the lease onto the Deployment's metadata. Metadata only: it does not
// touch spec, so it bumps no generation and rolls no pod.
func writeActiveLease(ctx context.Context, namespace, name string, until time.Time) error {
	body, err := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]string{
		activeLeaseAnnotation: until.UTC().Format(time.RFC3339Nano)}}})
	if err != nil {
		return err
	}
	resp, err := deploymentAPI(ctx, http.MethodPatch, namespace, name, "application/merge-patch+json", body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	return fmt.Errorf("k8s API PATCH deployment returned %s: %s", resp.Status, b)
}

// scaleDeployment PATCHes the Deployment's scale subresource via the in-cluster API using the mounted
// ServiceAccount token (no client-go, same pattern as deleteOwnPod). A merge patch of spec.replicas.
func scaleDeployment(ctx context.Context, namespace, name string, replicas int) error {
	const saDir = "/var/run/secrets/kubernetes.io/serviceaccount"
	token, err := os.ReadFile(filepath.Join(saDir, "token"))
	if err != nil {
		return fmt.Errorf("read SA token: %w", err)
	}
	caPEM, err := os.ReadFile(filepath.Join(saDir, "ca.crt"))
	if err != nil {
		return fmt.Errorf("read SA CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return fmt.Errorf("SA CA is not valid PEM")
	}
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" {
		host, port = "kubernetes.default.svc", "443"
	}
	url := fmt.Sprintf("https://%s:%s/apis/apps/v1/namespaces/%s/deployments/%s/scale", host, port, namespace, name)
	body := []byte(fmt.Sprintf(`{"spec":{"replicas":%d}}`, replicas))
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+string(token))
	req.Header.Set("Content-Type", "application/merge-patch+json")
	req.Header.Set("Accept", "application/json")
	hc := &http.Client{
		Timeout:   15 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}},
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	return fmt.Errorf("k8s API PATCH scale returned %s", resp.Status)
}

// deploymentStatus GETs the Deployment and returns (spec.replicas, status.readyReplicas) — the
// read-back half of F10/UC196. Same in-cluster ServiceAccount pattern as scaleDeployment; it needs
// one additional namespaced right (get on deployments) beyond the patch the scaler already has.
//
// readyReplicas is ABSENT from the JSON when it is zero, which is precisely the case worth reporting
// (a quota shortfall where nothing came up), so it must decode to 0 rather than be treated as missing
// data. Go's encoding/json does that for a plain int; it is called out because the opposite reading —
// "absent means unknown" — is what would turn a real shortfall back into silence.
func deploymentStatus(ctx context.Context, namespace, name string) (desired, total, ready, updated int, err error) {
	const saDir = "/var/run/secrets/kubernetes.io/serviceaccount"
	token, err := os.ReadFile(filepath.Join(saDir, "token"))
	if err != nil {
		return 0, 0, 0, 0, fmt.Errorf("read SA token: %w", err)
	}
	caPEM, err := os.ReadFile(filepath.Join(saDir, "ca.crt"))
	if err != nil {
		return 0, 0, 0, 0, fmt.Errorf("read SA CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return 0, 0, 0, 0, fmt.Errorf("SA CA is not valid PEM")
	}
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" {
		host, port = "kubernetes.default.svc", "443"
	}
	url := fmt.Sprintf("https://%s:%s/apis/apps/v1/namespaces/%s/deployments/%s", host, port, namespace, name)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+string(token))
	req.Header.Set("Accept", "application/json")
	hc := &http.Client{
		Timeout:   15 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}},
	}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, 0, 0, 0, fmt.Errorf("k8s API GET deployment returned %s", resp.Status)
	}
	var d struct {
		Spec struct {
			Replicas *int `json:"replicas"`
		} `json:"spec"`
		Status struct {
			// Replicas is status.replicas — TOTAL non-terminated pods, and the ONLY number that
			// separates a stalled rollout from a healthy one (021). A PENDING pod counts as
			// "updated", so updatedReplicas cannot see the stall by itself.
			Replicas      int `json:"replicas"`
			ReadyReplicas int `json:"readyReplicas"`
			// VR-F3. Like readyReplicas it is ABSENT from the JSON when zero — which is exactly the
			// case worth reporting (nothing has taken the new template yet), so it must decode to 0
			// rather than read as missing data.
			UpdatedReplicas int `json:"updatedReplicas"`
		} `json:"status"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&d); err != nil {
		return 0, 0, 0, 0, err
	}
	desired = 1 // a Deployment with no explicit spec.replicas defaults to 1 (named return)
	if d.Spec.Replicas != nil {
		desired = *d.Spec.Replicas
	}
	return desired, d.Status.Replicas, d.Status.ReadyReplicas, d.Status.UpdatedReplicas, nil
}

// patchDeploymentImage sets the executor container's image on the Deployment (F15/UC069 on request).
// Same in-cluster ServiceAccount pattern as scaleDeployment and deploymentStatus; the rollout that
// follows is Kubernetes' own, with the readiness probe as the gate.
//
// A strategic-merge patch keyed on the container NAME, so it touches only that container's image and
// leaves every other field of the pod spec — env, mounts, resources, probes — exactly as rendered.
func patchDeploymentImage(ctx context.Context, namespace, name, image string) error {
	const saDir = "/var/run/secrets/kubernetes.io/serviceaccount"
	token, err := os.ReadFile(filepath.Join(saDir, "token"))
	if err != nil {
		return fmt.Errorf("read SA token: %w", err)
	}
	caPEM, err := os.ReadFile(filepath.Join(saDir, "ca.crt"))
	if err != nil {
		return fmt.Errorf("read SA CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return fmt.Errorf("SA CA is not valid PEM")
	}
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" {
		host, port = "kubernetes.default.svc", "443"
	}
	url := fmt.Sprintf("https://%s:%s/apis/apps/v1/namespaces/%s/deployments/%s", host, port, namespace, name)
	body, err := json.Marshal(map[string]any{"spec": map[string]any{"template": map[string]any{
		"spec": map[string]any{"containers": []map[string]any{{"name": "executor", "image": image}}}}}})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+string(token))
	req.Header.Set("Content-Type", "application/strategic-merge-patch+json")
	req.Header.Set("Accept", "application/json")
	hc := &http.Client{
		Timeout:   20 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}},
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	return fmt.Errorf("k8s API PATCH deployment returned %s: %s", resp.Status, b)
}
