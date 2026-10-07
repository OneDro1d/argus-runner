package artifactmeasure

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/envcapture"
)

// the version key is a hash over the SET of running image digests, nothing else.

func TestVersionKey_DependsOnlyOnTheSetOfDigests(t *testing.T) {
	base := VersionKey([]string{dA, dB})
	if base == "" || len(base) != 64 {
		t.Fatalf("key %q, want a 64-character sha256 hex", base)
	}
	for name, in := range map[string][]string{
		"reordered":  {dB, dA},
		"duplicates": {dA, dB, dA, dB, dB}, // replica counts are not part of the version
		"upper case": {strings.ToUpper(dA[:7]) + dA[7:], dB},
	} {
		if got := VersionKey(in); got != base {
			t.Errorf("%s: key %q != %q, a version key must not depend on order, duplicates or replicas", name, got, base)
		}
	}
	if VersionKey([]string{dA}) == base {
		t.Error("a different set of digests gave the same key")
	}
	if VersionKey([]string{dA, dB, dC}) == base {
		t.Error("a set with one more digest gave the same key")
	}
}

func TestVersionKey_NoDigestIsNoKey(t *testing.T) {
	if k := VersionKey(nil); k != "" {
		t.Fatalf("key %q for no digests, want none: an empty set must never read as a version", k)
	}
	if k := VersionKey([]string{"", "not-a-digest"}); k != "" {
		t.Fatalf("key %q for unusable digests, want none", k)
	}
}

func TestVersionKey_IsPinnedByAGoldenVector(t *testing.T) {
	// The key is stored and compared across releases: changing the canonical form is a format change.
	got := VersionKey([]string{dB, dA})
	// computed outside Go: sha256 of "argus-version-key/v1\n" + dA + "\n" + dB + "\n"
	const want = "6e2be783f2d3fcf23ba5afcbfe2c894b12bf345265d6f4c03ab1fb5df6723402"
	if got != want {
		t.Fatalf("golden key moved: got %s", got)
	}
}

func TestNamespaceReader_ReadsTheNamedNamespaceNotAFixedOne(t *testing.T) {
	cl := k8sClient(t, 200, []any{pod("app-1", cs("app", "ghcr.io/x/app@"+dA))})
	read := NamespaceReader(SystemConfig{Tier: "k3d", NewClient: func() (*envcapture.Client, error) { return cl, nil }})
	if r := read(context.Background(), "sut"); r.Reason != "" || len(r.Digests) != 1 {
		t.Fatalf("namespace sut: %+v", r)
	}
	// the fake serves only /namespaces/sut/pods: any other namespace is a 404, a reason, never a digest
	if r := read(context.Background(), "other"); r.Reason == "" || len(r.Digests) != 0 {
		t.Fatalf("namespace other: %+v, want a reason and no digest", r)
	}
}

func TestNamespaceReader_EveryWayOfNotReadingIsAReason(t *testing.T) {
	noClient := func() (*envcapture.Client, error) { return nil, errors.New("no token mounted") }
	r := NamespaceReader(SystemConfig{Tier: "managed", NewClient: noClient})(context.Background(), "sut")
	if r.Reason == "" || !strings.Contains(r.Reason, "credentials") {
		t.Fatalf("no credentials: %+v", r)
	}
	r = NamespaceReader(SystemConfig{Tier: "vm"})(context.Background(), "sut")
	if r.Reason == "" || !strings.Contains(r.Reason, "tier") {
		t.Fatalf("unknown tier: %+v", r)
	}
	r = NamespaceReader(SystemConfig{Tier: "k3d", NewClient: noClient})(context.Background(), "")
	if r.Reason == "" {
		t.Fatalf("no namespace: %+v", r)
	}
}

func TestNamespaceReader_ComposeIgnoresTheNamespaceAndCallsNoKubernetes(t *testing.T) {
	called := 0
	run := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		called++
		return nil, errors.New("docker is not here")
	}
	newClient := func() (*envcapture.Client, error) {
		t.Fatal("the compose tier built a Kubernetes client")
		return nil, nil
	}
	r := NamespaceReader(SystemConfig{Tier: "compose", Run: run, NewClient: newClient,
		ComposeProject: func() string { return "p" }})(context.Background(), "")
	if called == 0 || r.Source != "compose" || r.Reason == "" {
		t.Fatalf("reading %+v after %d docker calls", r, called)
	}
}
