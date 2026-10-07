package config

import (
	"os"
	"path/filepath"
	"testing"
)

// The k8s trap, pinned. `localhost` is correct on compose and k3d and WRONG on a k8s tier, where it
// is not the operator's machine — that single fact is what INT-008 cost weeks to notice, because a
// wrong link is indistinguishable from a right one until somebody clicks it.
func TestBundledExamples_ManagedTierIsNeverLocalhost(t *testing.T) {
	root := repoRoot(t)
	for _, rel := range []string{
		"examples/memstore/argus-config.yaml",
		"examples/social/argus-config.yaml",
		"examples/argus-selftest/argus-config.yaml",
		"examples/order-service/codebase/argus-config.yaml",
		"examples/order-service/demo-packaging/argus-config.yaml",
		"examples/starter-packs/http-db/argus-config.yaml",
		"onboarding/argus-config.template.yaml",
	} {
		c, err := ParseUnresolved(filepath.Join(root, rel))
		if err != nil {
			continue // reported by the test above
		}
		got, gerr := c.GrafanaPublicURL("managed")
		if gerr != nil {
			continue // reported by the test above
		}
		if containsLocalhost(got) {
			t.Errorf("%s: managed tier resolves to %q — on a k8s tier localhost is not the operator's machine, so this link cannot open", rel, got)
		}
	}
}

func containsLocalhost(u string) bool {
	for _, bad := range []string{"localhost", "127.0.0.1"} {
		if len(u) >= len(bad) && indexOf(u, bad) >= 0 {
			return true
		}
	}
	return false
}

func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}

// repoRoot walks up from the package dir to the module root, so the test does not depend on where
// `go test` was invoked from.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("could not find the module root (go.mod) above the package dir")
	return ""
}
