package artifactmeasure

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"

	"github.com/OneDro1d/argus-runner/internal/envcapture"
)

// versionKeyDomain separates a version key from every other hash this package makes.
const versionKeyDomain = "argus-version-key/v1"

// VersionKey names WHICH VERSION of a system is running, as a sha256 hex over the SET of
// its running image digests: sorted, unique, one per line after a domain line. It depends on nothing else:
// not on the order the containers were listed in, not on duplicates (replicas of one image), not on
// resource limits or pod names, none of which a digest set carries.
//
// A digest that is not an OCI digest is ignored, and no usable digest is "" -- never the hash of an empty
// set, which would read as a version that every unreadable namespace shares. The format is stored and
// compared across releases, so it is pinned by a golden vector: changing it needs a new domain.
func VersionKey(digests []string) string {
	set := map[string]bool{}
	for _, d := range digests {
		if n, ok := NormalizeDigest(d); ok {
			set[n] = true
		}
	}
	if len(set) == 0 {
		return ""
	}
	sorted := make([]string, 0, len(set))
	for d := range set {
		sorted = append(sorted, d)
	}
	sort.Strings(sorted)
	h := sha256.New()
	h.Write([]byte(versionKeyDomain + "\n"))
	for _, d := range sorted {
		h.Write([]byte(d + "\n"))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// NamespaceFunc reads the running image digests of ONE Kubernetes namespace (or, on the compose tier, of the
// executor's compose project: the namespace is then ignored). Like Func it never errors; every way of not
// knowing is a Reading with a Reason.
type NamespaceFunc func(ctx context.Context, namespace string) Reading

// NamespaceReader is the NamespaceFunc for the executor's tier. It reuses K8s and Compose, so it reads pods
// only, through the same in-cluster service account, with the same bounded timeout; there is no new
// Kubernetes client. cfg.Namespace is NOT used: the namespace is the argument.
func NamespaceReader(cfg SystemConfig) NamespaceFunc {
	switch cfg.Tier {
	case "compose":
		measure := System(cfg)
		return func(ctx context.Context, _ string) Reading { return measure(ctx) }
	case "k3d", "managed":
		return func(ctx context.Context, ns string) Reading {
			ns = strings.TrimSpace(ns)
			if ns == "" {
				return Reading{Source: "k8s", Reason: "no namespace to read"}
			}
			newClient := cfg.NewClient
			if newClient == nil {
				newClient = envcapture.NewInClusterClient
			}
			cl, err := newClient()
			if err != nil {
				return Reading{Source: "k8s", Reason: "no in-cluster Kubernetes credentials: " + oneLine(err.Error())}
			}
			return K8s(cl, ns)(ctx)
		}
	}
	return func(context.Context, string) Reading {
		return Reading{Reason: "unknown deployment tier " + `"` + cfg.Tier + `"` + ": the executor does not know where the SUT runs"}
	}
}
