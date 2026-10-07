package runner

import (
	"context"
	"fmt"
)

// UC069's SELF-RESTART UPDATE WAS DELETED HERE (VR-V3 / V17-010, owner-ruled 2026-08-13).
//
// What it did: when the control plane reported an executor outdated, a k8s-tier executor DELETED ITS
// OWN POD so the Deployment would recreate it current, with a loop guard to stop it thrashing when no
// newer image existed.
//
// WHY IT WENT — and this is the part worth keeping, because "we removed a feature" invites someone to
// put it back. It was ALREADY INERT wherever we recommend running:
//
//   - deleting a pod only yields a NEWER executor if the Deployment's image reference has moved
//     underneath it. Every local tier renders imagePullPolicy: IfNotPresent, because the image is
//     imported into the node's own store and is NOT PULLABLE. The recreate returns the byte-identical
//     image. Self-update could never work on k3d at all.
//   - any DIGEST-PINNED executor recreates as itself, forever. Digest pinning is what the control
//     plane recommends "by contract, never a tag", and what the update button installs.
//   - so it worked only on AKS running a MOVING TAG, and when it failed the loop guard
//     parked the executor as blocked. Its own reason string said "no newer image on the tag" — the
//     author assumed a tag.
//
// The owner's ruling made the argument moot anyway: updating an executor is ALWAYS something a person
// does, on the Environments page. The two published floors decide only whether the executor keeps
// working meanwhile. Deleting this also deleted deferral, staged rollout and the mid-run-rolling
// problem, all of which existed only to make automatic updating safe.
//
// KEPT, below: NewK8sImageUpdate — the button's path, which PATCHES the Deployment image and therefore
// works on a tag and a digest alike. That is the mechanism the deleted one should have been.

// ── F15 / UC069 on request (CP-M3-III-71) ────────────────────────────────────────────────────────
//
// UC069's self-delete answers "the image behind my tag moved": the Deployment recreates the pod and
// it pulls the new layer. It CANNOT answer "run a different image", because deleting a pod recreates
// it from the same Deployment spec — the spec is what has to change.
//
// So an on-request update patches the executor's OWN Deployment image and lets the rollout do the
// rest. RBAC pins that to resourceNames:[executor]: it can re-image itself and nothing else.
//
// Compose deliberately has no equivalent. The compose executor's only mounts are the results volume,
// its config, its scenarios and its identity key — it has NO DOCKER SOCKET and physically cannot
// recreate its own container. Handing every executor the operator's Docker daemon is a far larger
// grant than an update button is worth, so compose gets a pre-filled command instead.
func NewK8sImageUpdate(namespace, deployment string, log func(string, ...any)) func(ctx context.Context, image string) error {
	if namespace == "" || deployment == "" {
		return nil
	}
	return newK8sImageUpdateFunc(nil, nil, namespace, deployment, log)
}

// newK8sImageUpdateFunc is NewK8sImageUpdate's real body, with the k8s connection and the actual
// patch call both injectable. Production goes through NewK8sImageUpdate, which passes nil for both:
// each request re-resolves the in-cluster connection fresh (a read failure must not be treated as
// permission to proceed) and, once the guard clears, calls the real patchDeploymentImage. Tests call
// this directly with a connection and/or patch func pointed at a fake server — an httptest.Server has
// no ServiceAccount to read from disk, the same problem internal/envcapture/client.go solves the same
// way — so the SAME closure logic is what gets exercised, not a reimplementation of it.
func newK8sImageUpdateFunc(
	connOverride *k8sAPIConn,
	patch func(ctx context.Context, namespace, deployment, image string) error,
	namespace, deployment string,
	log func(string, ...any),
) func(ctx context.Context, image string) error {
	if patch == nil {
		patch = patchDeploymentImage
	}
	return func(ctx context.Context, image string) error {
		// VR5-U2/VR5-T2 (V19-005), extended by V19-006/V19-007: decide whether this can possibly
		// succeed BEFORE patching. The Deployment is rendered maxSurge:0/maxUnavailable:1
		// (k8srender.go:363-365), so at replicas:1 the running pod is terminated FIRST — an image the
		// cluster cannot obtain leaves no executor and nothing able to roll it back. Measured on k3d:
		// the button took the executor down. V19-006 measured a second surface: a pod that references
		// SOME pull secret was treated as "can pull ANYTHING" — an executor onboarded against the private registry,
		// asked to move to the now-released GHCR image, passed the old check and was still stranded.
		// V19-006's own fix (reading the pull secret's `auths` keys over the API) was itself withdrawn
		// in V19-007: it required granting the executor `get` on its own image-pull Secret, which is a
		// credential the executor — a test-workload runner — should never be able to read, key-only or
		// not. V19-007 answers the same question from a render-time annotation on this Deployment
		// instead (registryhost.PullSecretRegistriesAnnotation) — one read, the Deployment this guard
		// already has RBAC for, no Secret named or fetched.
		//
		// A read failure is NOT treated as permission to proceed. "I could not check" and "the check
		// passed" are different answers, and only one of them may re-image a live executor.
		conn := connOverride
		if conn == nil {
			var err error
			conn, err = newInClusterConn()
			if err != nil {
				return fmt.Errorf("refusing to re-image: could not establish an in-cluster API "+
					"connection to check whether this cluster can obtain %s (%w). Proceeding blind "+
					"would terminate the running executor before discovering the new one cannot start",
					image, err)
			}
		}
		cur, pullSecretNames, annotatedRegistries, rerr := deploymentImageAndPullSecretNames(ctx, conn, namespace, deployment, "executor")
		if rerr != nil {
			return fmt.Errorf("refusing to re-image: could not read %s/%s to check whether this "+
				"cluster can obtain %s (%w). Proceeding blind would terminate the running executor "+
				"before discovering the new one cannot start", namespace, deployment, image, rerr)
		}
		if gerr := guardImageObtainable(cur, image, pullSecretNames, annotatedRegistries); gerr != nil {
			return gerr
		}
		if log != nil {
			log("update requested by the control plane: re-imaging %s/%s onto %s", namespace, deployment, image)
		}
		return patch(ctx, namespace, deployment, image)
	}
}
