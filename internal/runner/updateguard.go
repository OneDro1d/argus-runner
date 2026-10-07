package runner

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/OneDro1d/argus-runner/internal/registryhost"
)

// k8sAPIConn bundles the three things every in-cluster API call in this file needs: the API server's
// base URL, a bearer token, and an http.Client trusting the cluster CA. Production code builds one
// from the mounted ServiceAccount (newInClusterConn); tests build one pointed at an httptest.Server
// directly — the same seam internal/envcapture/client.go already uses for this exact problem, because
// an httptest.Server has no ServiceAccount to read from disk.
type k8sAPIConn struct {
	baseURL string
	token   string
	hc      *http.Client
}

// newInClusterConn reads the mounted ServiceAccount token + CA, the same way every other in-cluster
// caller in this package (deploymentStatus, patchDeploymentImage, …) does, and resolves the API
// server address from KUBERNETES_SERVICE_HOST/PORT (or the conventional in-cluster DNS name).
func newInClusterConn() (*k8sAPIConn, error) {
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
	return &k8sAPIConn{
		baseURL: fmt.Sprintf("https://%s:%s", host, port),
		token:   string(token),
		hc: &http.Client{
			Timeout:   15 * time.Second,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}},
		},
	}, nil
}

// do issues one bearer-authenticated request against an absolute API path and returns the raw body
// plus the HTTP status code. A non-2xx status is NOT turned into an error here — callers decide what
// each status means (a 404 on a Secret is "covers nothing", not "could not check").
func (c *k8sAPIConn) do(ctx context.Context, method, path, contentType string, body []byte) ([]byte, int, error) {
	var rd io.Reader
	if body != nil {
		rd = strings.NewReader(string(body))
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rd)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return out, resp.StatusCode, nil
}

func (c *k8sAPIConn) get(ctx context.Context, path string) ([]byte, int, error) {
	return c.do(ctx, http.MethodGet, path, "", nil)
}

// registryHostOfImage and normalizeRegistryHost live in internal/registryhost now (V19-007) — both
// internal/k8srender (which WRITES the pull-secret-registries annotation at render time) and this
// package (which READS it back) need to compute the same host from the same rules, and duplicating
// the logic per-package is how the two would eventually disagree. Kept as unexported wrappers here so
// the existing tests below (TestRegistryHostOfImage, TestNormalizeRegistryHost) and every other
// caller in this file keep their original, unqualified names.
func registryHostOfImage(ref string) string    { return registryhost.HostOfImage(ref) }
func normalizeRegistryHost(host string) string { return registryhost.Normalize(host) }

// deploymentImageAndPullSecretNames reads the facts guardImageObtainable needs from the executor's
// own Deployment — the ONLY k8s object this guard ever reads, and the executor already has RBAC
// (get/patch, resourceNames-pinned to itself) for it:
//
//   - the currently-running image of `container`
//   - the NAMES of every imagePullSecret the pod spec references (never their contents — this guard
//     grants and reads NO Secret, of any kind, ever; see k8srender.go's Role comment, V19-007)
//   - the registry hosts RECORDED in the Deployment's own registryhost.PullSecretRegistriesAnnotation
//     annotation, written by internal/k8srender at render time (pullSecretRegistryAnnotationBlock)
//     from the registry the pull secret was actually made for — this is what replaced reading the
//     Secret's `auths` map keys over the API (V19-006, withdrawn)
//
// `container` is the container whose image is under management; the strategic-merge patch that
// follows is keyed on the same name, so reading a different one would guard the wrong thing.
func deploymentImageAndPullSecretNames(ctx context.Context, conn *k8sAPIConn, namespace, name, container string) (image string, pullSecretNames []string, annotatedRegistries map[string]bool, err error) {
	path := fmt.Sprintf("/apis/apps/v1/namespaces/%s/deployments/%s", namespace, name)
	body, status, err := conn.get(ctx, path)
	if err != nil {
		return "", nil, nil, err
	}
	if status != http.StatusOK {
		return "", nil, nil, fmt.Errorf("k8s API GET deployment returned %d", status)
	}
	var d struct {
		Metadata struct {
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
		Spec struct {
			Template struct {
				Spec struct {
					Containers []struct {
						Name  string `json:"name"`
						Image string `json:"image"`
					} `json:"containers"`
					ImagePullSecrets []struct {
						Name string `json:"name"`
					} `json:"imagePullSecrets"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(body, &d); err != nil {
		return "", nil, nil, err
	}
	for _, c := range d.Spec.Template.Spec.Containers {
		if c.Name == container {
			image = c.Image
			break
		}
	}
	for _, s := range d.Spec.Template.Spec.ImagePullSecrets {
		if s.Name != "" {
			pullSecretNames = append(pullSecretNames, s.Name)
		}
	}
	annotatedRegistries = parsePullSecretRegistriesAnnotation(d.Metadata.Annotations[registryhost.PullSecretRegistriesAnnotation])
	return image, pullSecretNames, annotatedRegistries, nil
}

// parsePullSecretRegistriesAnnotation turns the Deployment's comma-separated
// registryhost.PullSecretRegistriesAnnotation value into a normalised host set. An absent or empty
// annotation (rendered with no ImagePullSecret, or a Deployment from before V19-007 that has never
// been re-rendered) yields an empty, non-nil set — "recorded nothing", handled the same as "recorded
// but does not list this host" by guardImageObtainable, never as an error.
func parsePullSecretRegistriesAnnotation(v string) map[string]bool {
	out := map[string]bool{}
	for _, host := range strings.Split(v, ",") {
		host = strings.TrimSpace(host)
		if host == "" {
			continue
		}
		out[registryhost.Normalize(host)] = true
	}
	return out
}

// sortedRegistryList renders a covered-registries set as a deterministic, human-readable list for
// error messages ("no registry" when empty, so the message never reads "(they cover )").
func sortedRegistryList(covered map[string]bool) string {
	if len(covered) == 0 {
		return "no registry"
	}
	keys := make([]string, 0, len(covered))
	for k := range covered {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}

// guardImageObtainable decides whether patching this executor's Deployment to `requested` can
// possibly succeed — BEFORE the patch happens (VR5-U2 / VR5-T2, V19-005; extended by V19-006/V19-007
// to check which registry a pull secret actually covers, not merely whether one is referenced).
//
// ── WHY A PRE-FLIGHT AND NOT A WATCH-AND-REVERT ──────────────────────────────────────────────
//
// k8srender.go:363-365 renders the executor Deployment with:
//
//	strategy: RollingUpdate, rollingUpdate: {maxSurge: 0, maxUnavailable: 1}
//
// At replicas: 1 that terminates the OLD pod FIRST and then creates the new one. So when the new image
// cannot be obtained there is no executor left at all — and nothing can roll it back, because the
// component that would do the reverting is the one that just died. INT-032 chose maxSurge: 0
// deliberately (headroom; "a brief gap at replicas: 1, the correct trade") without anticipating an
// image the cluster cannot get. A post-hoc recovery design is therefore impossible here; the only
// place a guard can work is in front of the patch.
//
// ── HOW "CAN THE CLUSTER OBTAIN IT?" IS ANSWERED — FROM THE DEPLOYMENT ALONE, NO SECRET READ ────
//
// The executor cannot enumerate the nodes' containerd stores. It does not need to. There are exactly
// two ways a pod gets an image, and both are visible from its own Deployment:
//
//   - PULL — requires a credential for the REQUESTED image's registry host, specifically. Referencing
//     SOME secret is not enough: released executors pull from GHCR; an executor onboarded earlier may
//     carry a secret that covers the private registry ONLY, and an the private registry→GHCR re-image with that secret would pass a
//     check that only asked "is there a secret" (V19-006 — the guard's prior version had exactly this
//     gap). V19-006 closed that gap by having the guard read the Secret's `auths` keys itself, which
//     meant granting the executor `get` on its own image-pull credential — the orchestrator withdrew
//     that trade (the executor runs test workloads and should hold no Secret access at all, and the
//     ghcr-pull token is shared across every tester). V19-007 answers the SAME question — does a
//     referenced pull secret cover the requested registry? — from two Deployment-only facts instead:
//     (1) requesting the SAME registry the executor is already running from, with any pull secret
//     referenced at all, is unchanged from before V19-006 (onboarding only ever wires a secret for the
//     registry it deploys from, so "same registry + a secret is referenced" is already proven safe);
//     (2) a DIFFERENT registry is allowed only when `annotatedRegistries` — the
//     registryhost.PullSecretRegistriesAnnotation internal/k8srender wrote on this Deployment at
//     render time — explicitly lists it. Both facts come from the ONE Deployment read this guard
//     already has RBAC for; no Secret is ever named, fetched, or referenced by content.
//   - IMPORT — a host-side `ctr images import` into every node, which onboarding performs and which
//     nothing inside the cluster can either verify or trigger.
//
// So: no pull secret referenced at all, or a cross-registry move the Deployment's own annotation does
// not vouch for, means the only remaining route is an import this executor can neither confirm nor
// perform. That is the exact state V19-005/V19-006/V19-007 measured, and refusing is the whole fix.
//
// Requesting the image ALREADY running is always allowed — it is a no-op rollout that cannot strand
// anything, and refusing it would break the idempotent re-request the control plane may legitimately
// make.
func guardImageObtainable(current, requested string, pullSecretNames []string, annotatedRegistries map[string]bool) error {
	if requested == "" {
		return fmt.Errorf("refusing to re-image: the control plane requested an empty image reference")
	}
	if requested == current {
		return nil // no-op rollout; nothing to strand
	}
	if len(pullSecretNames) == 0 {
		return fmt.Errorf("refusing to re-image %s -> %s: this executor's pod spec carries no "+
			"imagePullSecrets, so its cluster cannot PULL that image, and an image it has not been given "+
			"cannot be obtained from inside. On a local tier the image must be imported into every node "+
			"first (onboarding does this; `docker save <img> | ctr -n k8s.io images import -` per node). "+
			"Proceeding would terminate the running executor before discovering the new one cannot start "+
			"(the Deployment is maxSurge:0), leaving no executor and nothing able to roll it back",
			current, requested)
	}
	currentHost := registryHostOfImage(current)
	wantHost := registryHostOfImage(requested)
	if wantHost == currentHost {
		return nil // same-registry update with a pull secret referenced; unchanged since before V19-006
	}
	if annotatedRegistries[wantHost] {
		return nil // the executor's own Deployment records a pull secret covering this registry
	}
	return fmt.Errorf("refusing to re-image %s -> %s: this executor's Deployment does not record %s as "+
		"a covered pull-secret registry (recorded: %s), so its cluster may not be able to PULL the "+
		"requested image from that registry, and an image the cluster cannot obtain cannot be obtained "+
		"from inside. Proceeding would terminate the running executor before discovering the new one "+
		"cannot start (the Deployment is maxSurge:0), leaving no executor and nothing able to roll it "+
		"back. Create a pull secret that covers %s, reference it from the executor Deployment's "+
		"imagePullSecrets, record it, then retry, e.g.:\n"+
		"  kubectl -n <ns> create secret docker-registry ghcr-pull --docker-server=%s "+
		"--docker-username=<user> --docker-password=<token>\n"+
		"  # add {name: ghcr-pull} to the executor Deployment's imagePullSecrets\n"+
		"  kubectl -n <ns> annotate deployment/executor %s=%s --overwrite\n"+
		"  # then retry the update",
		current, requested, wantHost, sortedRegistryList(annotatedRegistries), wantHost, wantHost,
		registryhost.PullSecretRegistriesAnnotation, wantHost)
}
