package config

import "testing"

// (amended): CredentialValues is the ONE source of the scrub list for a recorded output,
// so a declared check_env value has to be in it.
func TestCredentialValues_IncludesEveryDeclaredCheckEnvValue(t *testing.T) {
	t.Setenv("SCRUB_DECL_ONE", "canary-declared-one-81")
	t.Setenv("SCRUB_DECL_TWO", "canary-declared-two-82")
	t.Setenv("SCRUB_DECL_EMPTY", "")
	c := &Config{CheckEnv: []string{"SCRUB_DECL_ONE", "SCRUB_DECL_TWO", "SCRUB_DECL_ONE", "SCRUB_DECL_EMPTY", "SCRUB_DECL_UNSET"}}
	got := map[string]bool{}
	for _, v := range c.CredentialValues() {
		got[v] = true
	}
	for _, want := range []string{"canary-declared-one-81", "canary-declared-two-82"} {
		if !got[want] {
			t.Errorf("CredentialValues is missing a declared value %q (got %d values)", want, len(got))
		}
	}
	if got[""] {
		t.Error("an empty value must never be in the scrub list")
	}
	if len(got) != 2 {
		t.Errorf("want exactly the two set values, got %d", len(got))
	}
}
