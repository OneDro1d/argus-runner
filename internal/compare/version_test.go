package compare

import (
	"strings"
	"testing"
)

func TestVersionKey_EmptyWhenNothingWasMeasured(t *testing.T) {
	for _, tc := range []struct {
		digests []string
		env     string
	}{{nil, ""}, {[]string{}, "  "}, {[]string{"", " "}, ""}} {
		if got := VersionKey(tc.digests, tc.env); got != "" {
			t.Errorf("VersionKey(%q, %q) = %q, want empty: nothing was measured", tc.digests, tc.env, got)
		}
	}
}

func TestVersionKey_OrderAndDuplicatesDoNotMatterButContentDoes(t *testing.T) {
	a := "sha256:" + strings.Repeat("a", 64)
	b := "sha256:" + strings.Repeat("b", 64)
	k1 := VersionKey([]string{a, b}, "env-1")
	if len(k1) != 64 {
		t.Fatalf("key = %q, want 64 hex characters", k1)
	}
	if k2 := VersionKey([]string{b, a, a}, "env-1"); k2 != k1 {
		t.Errorf("order or a duplicate changed the key: %s vs %s", k2, k1)
	}
	for name, other := range map[string]string{
		"another digest set":    VersionKey([]string{a}, "env-1"),
		"another fingerprint":   VersionKey([]string{a, b}, "env-2"),
		"digests but no env":    VersionKey([]string{a, b}, ""),
		"a fingerprint alone":   VersionKey(nil, "env-1"),
		"digest moved into env": VersionKey(nil, a+","+b+"\nenv-1"),
	} {
		if other == k1 {
			t.Errorf("%s gave the same key as the original", name)
		}
	}
	if VersionKey(nil, "env-1") == "" || VersionKey([]string{a}, "") == "" {
		t.Errorf("one of the two measured is enough to name a version")
	}
}
