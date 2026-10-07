package buildinfo

// F12 / UC071 (CP-M3-III-68): a version that is the same for every build is not a version.
//
// Every executor in the estate reported "0.1.0" — not because they were the same build, but because
// the number came from a hardcoded default in the deployment templates and nothing derived it from
// the binary. That silently disabled UC071's outdated-blocked mode (nothing can be BELOW a minimum
// when everything reports the same number), the EXECUTOR row's current-vs-behind state, and any
// ability for update.sh to tell whether an update changed anything.

import "testing"

func withStamp(t *testing.T, version, commit, date string) {
	t.Helper()
	ov, oc, od := Version, Commit, Date
	Version, Commit, Date = version, commit, date
	t.Cleanup(func() { Version, Commit, Date = ov, oc, od })
}

func TestResolve_PrecedenceOrder(t *testing.T) {
	withStamp(t, "m3-iii27", "aaaa111", "2026-08-06T21:00:00Z")

	if got := Resolve(""); got != "m3-iii27" {
		t.Errorf("Resolve(\"\") = %q, want the LINK-TIME stamp", got)
	}
	// The override is first on purpose: an operator sometimes needs to pin during an incident. It is
	// a deliberate act with a visible cause — unlike the template default it replaces, which nobody
	// chose and nobody could see.
	if got := Resolve("pinned-9.9.9"); got != "pinned-9.9.9" {
		t.Errorf("Resolve(override) = %q, want the override to win", got)
	}
	if got := Resolve("   "); got != "m3-iii27" {
		t.Errorf("Resolve(whitespace) = %q — blank is not an override, it is an empty env var", got)
	}
}

// The heart of it: two builds must be able to disagree.
func TestResolve_DifferentBuildsReportDifferentVersions(t *testing.T) {
	withStamp(t, "m3-iii27", "aaaa111", "")
	a := Resolve("")
	withStamp(t, "m3-iii28", "bbbb222", "")
	b := Resolve("")
	if a == b {
		t.Fatalf("both builds report %q — this is exactly the defect: nothing can be judged stale when every build reports the same number", a)
	}
}

func TestResolve_UnstampedIsHonestlyUnknown(t *testing.T) {
	withStamp(t, "", "", "")
	got := Resolve("")
	// Either Go's own VCS stamp (a real checkout) or the dev sentinel — never something that looks
	// like a chosen release number. "0.1.0" reads like a decision; that is the confusion being ended.
	if got == "0.1.0" {
		t.Fatalf("an unstamped build reported %q — a plausible-looking release number is worse than an obviously-unknown one", got)
	}
	if got != devVersion && len(got) < len("0.0.0-src+") {
		t.Errorf("Resolve() = %q, want %q or a 0.0.0-src+<rev> VCS stamp", got, devVersion)
	}
}

func TestFull_CarriesCommitAndDateWhenKnown(t *testing.T) {
	withStamp(t, "m3-iii27", "aaaa111", "2026-08-06T21:00:00Z")
	full := Full("")
	for _, want := range []string{"m3-iii27", "aaaa111", "2026-08-06T21:00:00Z"} {
		if !contains(full, want) {
			t.Errorf("Full() = %q, missing %q", full, want)
		}
	}
	withStamp(t, "m3-iii27", "", "")
	if got := Full(""); got != "m3-iii27" {
		t.Errorf("Full() with no commit/date = %q, want just the version — no empty parentheses", got)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
