package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/updatecmd"
)

// AC-D53 (round-6 review BIN-2): discover.sh keeps the executor's version only from an answer that SAYS it was not
// overridden — it matches updatecmd.VersionNotOverridden as text, in bash. The stubs that test discover print a copy of
// that answer, so nothing else ties the match to what the binary really prints: a renamed key or a compact encoding
// would leave every reading empty on every tier, with every test green. This asks the real, stamped binary, the way
// discover asks it: with the override set, and with it cleared.
func TestVersion_ACD53_TheAnswerSaysWhetherItWasOverriddenAsDiscoverMatchesIt(t *testing.T) {
	bin := buildStampedArgusBinary(t, "0.3.33")
	for _, c := range []struct {
		name, override, version string
		overridden              bool
	}{
		{"the override set", "0.9.9-pinned", "0.9.9-pinned", true},
		{"the override cleared, as discover asks", "", "0.3.33", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			cmd := exec.Command(bin, "version")
			cmd.Env = append(os.Environ(), "ARGUS_VERSION="+c.override) // the last value of a key wins
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("argus version: %v\n%s", err, out)
			}
			if got := strings.Contains(string(out), updatecmd.VersionNotOverridden); got == c.overridden {
				t.Errorf("the answer contains %q = %v, want %v — discover would take the wrong answer for a reading:\n%s",
					updatecmd.VersionNotOverridden, got, !c.overridden, out)
			}
			if !strings.Contains(string(out), `  "version": "`+c.version+`"`) {
				t.Errorf("the answer has no line `  \"version\": %q` for discover's sed to read:\n%s", c.version, out)
			}
		})
	}
}
