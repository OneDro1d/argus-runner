// Package registryhost normalises image references and dockerconfigjson `auths` keys into the same
// registry-host strings, so RENDER time (internal/k8srender) and GUARD time (internal/runner) never
// disagree about what a host string means. This is the one thing both sides must agree on: render
// writes which registry a pull secret was made for, using the host of the image it is rendering;
// the guard later reads that record back and compares it against the host of a REQUESTED image.
//
// Moved out of internal/runner/updateguard.go (where V19-006 first wrote it) when V19-007 replaced a
// runtime Secret read (the executor holding `get` on its own image-pull credential) with a
// render-time Deployment annotation that both packages need to compute the same host from — see
// PullSecretRegistriesAnnotation.
package registryhost

import "strings"

// PullSecretRegistriesAnnotation is the executor Deployment annotation key that records which
// registry hosts its ImagePullSecret is believed to cover. internal/k8srender writes it at render
// time (the registry of the image being rendered, via HostOfImage); internal/runner's update guard
// reads it back from the Deployment it already has RBAC for (get/patch on its OWN Deployment) — no
// Secret read, and no secrets RBAC of any kind (V19-007, withdrawing V19-006's Secret-read design).
const PullSecretRegistriesAnnotation = "argus.onedroid.ai/pull-secret-registries"

// HostOfImage returns the registry host an image reference points at, by Docker's own rule: the
// first path component is a registry only if it contains a '.' or ':' or is "localhost". Otherwise
// it is a Docker Hub namespace and the registry is docker.io. This mirrors
// onboarding/lib/pull-secret.sh's pull_secret_registry() — keep the two in sync.
func HostOfImage(ref string) string {
	i := strings.Index(ref, "/")
	if i < 0 {
		return "docker.io"
	}
	first := ref[:i]
	if first == "localhost" || strings.HasPrefix(first, "localhost:") || strings.ContainsAny(first, ".:") {
		return Normalize(first)
	}
	return "docker.io"
}

// Normalize folds the small set of differences docker itself treats as "the same registry" that a
// raw string compare would miss: a URL scheme and any path some `auths` keys carry (legacy Docker Hub
// entries look like "https://index.docker.io/v1/"), the historical Docker Hub aliases, letter case
// (registry hosts are not case sensitive), and an explicit default HTTPS port. It is deliberately NOT
// a full URL parser — every registry host this repo ever renders (ghcr.io, registry.example.com,
// docker.io) is plain, and a parser would only add ways for a malformed key to be silently misread as
// "safe".
func Normalize(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "http://")
	if i := strings.Index(s, "/"); i >= 0 {
		s = s[:i]
	}
	s = strings.ToLower(s)
	switch s {
	case "docker.io", "registry-1.docker.io", "index.docker.io":
		return "docker.io"
	}
	return strings.TrimSuffix(s, ":443")
}
