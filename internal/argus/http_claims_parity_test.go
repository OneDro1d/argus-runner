package argus

import (
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// — the validator and the executor parse an http step's claims with ONE parser, so
// they cannot disagree again. For every claim the executor refuses at preflight, the write-time
// validator must refuse with the SAME reason text; for every claim the executor accepts, the
// validator must accept.
func TestHTTPStepClaims_ValidatorAndExecutorAgree(t *testing.T) {
	claims := []string{
		"status=200",
		"body has count > 5",
		"body has count >= ${saved.n}",
		"body contains ${saved.n}",
		"body has count > lots",
		"body has count => 5",
		"body has count > ${saved.n}0",
		"body has count matching (",
		"body has count containing",
		"result.isError == false",
		"something else entirely",
	}
	for _, c := range claims {
		_, _, execErr := httpStepExpect([]string{c})

		md := strings.Join([]string{
			"# Scenario: c", "",
			"## Metadata", "- **ID**: CHN-P1", "- **Layer**: Permissions", "- **Tags**: chain", "",
			"## TRIGGER", "POST `chain`", "",
			"```json",
			`{"steps":[{"type":"http","name":"a","method":"GET","url":"http://x/a","save":{"n":"count"}},` +
				`{"type":"http","name":"b","method":"GET","url":"http://x/b"}]}`,
			"```", "",
			"## EXPECT", "### Runnable", "- step a: status=200", "- step b: " + c, "",
			"## TIMEOUT", "60s", "",
			"## CLEANUP", "N/A — a unit-test fixture; it creates nothing.", "",
		}, "\n")
		_, errs := scenario.Validate(md)

		if execErr == nil {
			if len(errs) != 0 {
				t.Errorf("%q: the executor accepts it but the validator refuses: %v", c, errs)
			}
			continue
		}
		want := `step "b": ` + execErr.Error()
		found := false
		for _, e := range errs {
			if e.Message == want {
				found = true
			}
		}
		if !found {
			t.Errorf("%q: the executor refuses with %q but the validator said %v", c, want, errs)
		}
	}
}
