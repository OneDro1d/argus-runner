package toolcore

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/config"
)

// — every validator that reads a chain step's url must answer the same for a url that
// starts with ${INGESTION_URL}: ValidateAll (the ONE tier-2 answer behind author__validate_scenario,
// author__write_scenario and the control plane's author_write_scenario) and the local
// `validate-config --scenarios` walk (config.Validate).
const ingestionChainScenario = `# Scenario: c

## Metadata
- **ID**: CHN-ING-001
- **Layer**: Permissions
- **Tags**: chain

## TRIGGER
POST ` + "`chain`" + `

` + "```json" + `
{"steps":[
  {"type":"http","name":"login","method":"POST","url":"${INGESTION_URL}/api/v1/login","body":{"password":"${SOME_PASSWORD}"}},
  {"type":"http","name":"read","method":"GET","url":"${INGESTION_URL}/api/v1/me"}
]}
` + "```" + `

## EXPECT
### Runnable
- step login: status=200
- step read: status=200

## TIMEOUT
60s

## CLEANUP
N/A — a unit-test fixture; it creates nothing.
`

func TestValidators_AgreeOnAnIngestionURLLedChainStep(t *testing.T) {
	_, verrs := ValidateAll(ingestionChainScenario)
	if len(verrs) != 0 {
		t.Fatalf("ValidateAll (every author path) refused a ${INGESTION_URL}-led chain http url: %v", verrs)
	}

	dir := t.TempDir()
	sub := filepath.Join(dir, "chain")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "CHN-ING-001.md"), []byte(ingestionChainScenario), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SOME_PASSWORD", "fake-validator-value-1")
	cfgPath := filepath.Join(dir, "argus-config.yaml")
	cfg := "project:\n  name: p\ntargets:\n  http:\n    base_url: http://127.0.0.1:1\ncheck_env:\n  - SOME_PASSWORD\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	errs, n, err := c.Validate(dir)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("the local walk found %d scenarios, want 1", n)
	}
	for _, e := range errs {
		if strings.Contains(e.Message, "INGESTION_URL") || strings.Contains(e.Message, "SOME_PASSWORD") {
			t.Errorf("the local validate-config walk refused the url: %s", e.Message)
		}
	}
	if len(errs) != 0 {
		t.Errorf("the local walk answered differently from ValidateAll: %v", errs)
	}
}
