package onboard

import "testing"

// ControlPlaneForDisplay and ControlPlaneForCommand share one cleaning core: they drop the
// same things and refuse the same values. They differ in exactly one way: the command form is shell-quoted
// when the shell would split or glob it, the display form never is.
func TestControlPlaneForDisplay_SameCleaningNeverQuoted(t *testing.T) {
	for _, tc := range []struct{ name, raw, display, command string }{
		{"plain", "https://argus-dev.onedroid.ai", "https://argus-dev.onedroid.ai", "https://argus-dev.onedroid.ai"},
		{"userinfo, query and fragment dropped", "https://someuser:PWCANARY@host.example/base?token=QCANARY#FRAGCANARY", "https://host.example/base", "https://host.example/base"},
		{"a path the shell would split", "https://host.example/a b", "https://host.example/a%20b", "https://host.example/a%20b"},
		{"a path the shell would glob", "https://host.example/a*b", "https://host.example/a*b", "'https://host.example/a*b'"},
		{"empty", "", "<control-plane-url>", "<control-plane-url>"},
		{"schemeless", "//host.example/base", "<control-plane-url>", "<control-plane-url>"},
		{"a bare word that may be a secret", "PWCANARY", "<control-plane-url>", "<control-plane-url>"},
		{"does not parse", "https://host.example/%zz?token=QCANARY", "<control-plane-url>", "<control-plane-url>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ControlPlaneForDisplay(tc.raw); got != tc.display {
				t.Errorf("ControlPlaneForDisplay(%q) = %q, want %q", tc.raw, got, tc.display)
			}
			if got := ControlPlaneForCommand(tc.raw); got != tc.command {
				t.Errorf("ControlPlaneForCommand(%q) = %q, want %q", tc.raw, got, tc.command)
			}
		})
	}
}
