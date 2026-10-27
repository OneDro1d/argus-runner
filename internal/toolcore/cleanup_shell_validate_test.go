package toolcore

import (
	"encoding/json"
	"strings"
	"testing"
)

// (#709) — the bash-only-syntax warning reaches the author_validate_scenario answer
// (ValidateScenario is what author_validate_scenario and author_write_scenario both call), and the
// scenario stays valid.
func TestValidateScenario_BashOnlyCleanupWarnsAndStaysValid(t *testing.T) {
	md := strings.Replace(mcpScenarioWith(), "N/A — a unit-test fixture; it creates nothing.",
		"```bash\nread -r X <<< \"$Y\"\n```", 1)
	p, invalid, err := ValidateScenario(md)
	if err != nil || invalid {
		t.Fatalf("a warning must not make the scenario invalid: invalid=%v err=%v %+v", invalid, err, p)
	}
	b, _ := json.Marshal(p)
	if !strings.Contains(string(b), `"valid":true`) || !strings.Contains(string(b), "here-string") || !strings.Contains(string(b), "dash") {
		t.Errorf("the answer must be valid:true with the sh/dash warning: %s", b)
	}
}
