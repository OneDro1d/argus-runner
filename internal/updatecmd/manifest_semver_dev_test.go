package updatecmd

import "testing"

// manifest_semver_dev_test.go — AC-D49 (#296): parseSemver/SemverLess/ValidVersion could not order a
// dev build suffix ("0.3.37-dev+9816842"): parseSemver split on "." and Atoi'd each part, so the
// third part ("37-dev+9816842") failed to parse and the whole version was unrankable. That silently
// decided rehash.go's "should previous be recorded" question too (see rehash_previous_test.go).

func TestValidVersion_DevSuffix(t *testing.T) {
	cases := []struct {
		v    string
		want bool
	}{
		{"0.3.37-dev+9816842", true}, // THE version from the issue — must now be orderable
		{"0.3.40", true},
		{"v0.3.40", true},
		{"0.3.40-dev+abc123", true},
		{"0.3.40+abc123", true}, // build metadata alone, no "-"
		{"dev", false},          // no numeric core at all — genuinely unrankable
		{"garbage", false},
		{"", false},
		{"0.3", false}, // missing patch — still refused, unlike control's tolerant parser
		// ⛔ AC-D49 REGRESSION GUARD: "0.0.0-dev" / "0.0.0-src+<rev>" are buildinfo's own
		// NO-RELEASE-IDENTITY sentinels for an unstamped build (cmd/argus/update_phase1.go:116,
		// control.isNoReleaseIdentity) — suffix-stripping must NOT make them look like a real,
		// orderable "version 0.0.0", or the downgrade gate and the shared-folder hold-back switch off
		// for a developer's locally-built executor.
		{"0.0.0-dev", false},
		{"0.0.0-src+abc123", false},
		{"0.0.0", true}, // a BARE 0.0.0 (no suffix) is left alone — only the sentinel spelling is refused
	}
	for _, c := range cases {
		if got := ValidVersion(c.v); got != c.want {
			t.Errorf("ValidVersion(%q) = %v, want %v", c.v, got, c.want)
		}
	}
}

// The exact pair from AC-D49: an update from 0.3.37-dev+9816842 to 0.3.40 must be seen as a forward
// move, not an unrankable one.
func TestSemverLess_TheIssuesOwnVersionPair(t *testing.T) {
	if !SemverLess("0.3.37-dev+9816842", "0.3.40") {
		t.Error(`SemverLess("0.3.37-dev+9816842", "0.3.40") = false, want true — this is the exact ` +
			`comparison AC-D49 measured failing`)
	}
	if SemverLess("0.3.40", "0.3.37-dev+9816842") {
		t.Error(`SemverLess("0.3.40", "0.3.37-dev+9816842") = true, want false`)
	}
}

// AC-D49's chosen precedence rule, stated in SemverLess's doc comment: a suffixed version is BELOW
// the bare release of the SAME numeric core (semver §11.4), and two suffixed versions of the same
// core are not ordered against each other (there is no second axis to compare on here).
func TestSemverLess_PrereleasePrecedenceSameCore(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"0.3.40-dev+abc", "0.3.40", true},          // pre-release < release, same core
		{"0.3.40", "0.3.40-dev+abc", false},         // release is not below its own pre-release
		{"0.3.40-dev+abc", "0.3.40-dev+xyz", false}, // two dev builds of the same core: not ordered
		{"0.3.40-dev+abc", "0.3.41", true},          // differing core still decides first
	}
	for _, c := range cases {
		if got := SemverLess(c.a, c.b); got != c.want {
			t.Errorf("SemverLess(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}
