package argus

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/config"
)

// (amended): a value of a name declared under `check_env` is a secret exactly like a value
// under targets.auth, so it must never reach a STORED output (ARGUS-CMP-3, design 1.5). The only source of
// the scrub list is config.CredentialValues; these tests drive the real run loop (runCmp ->
// RunAllWithMode, the path the executor uses) against a real HTTP server that echoes the request body, so
// a SUT that reflects a secret back is the case under test.

func echoBodyServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func storedChainOutput(t *testing.T, run cmpRun, step string) string {
	t.Helper()
	b, err := os.ReadFile(outputFilePath(run, "CHN-C1__"+step+".1.json"))
	if err != nil {
		t.Fatalf("stored output of step %s: %v", step, err)
	}
	return string(b)
}

func TestCompareMode_DeclaredCheckEnvValueIsScrubbedFromTheStoredOutput(t *testing.T) {
	const canary = "Cnry-declared-check-env-6107"
	const other = "Cnry-second-declared-name-3392"
	t.Setenv("ECHO_CHECK_PASSWORD", canary)
	t.Setenv("ECHO_CHECK_OTHER", other)
	srv := echoBodyServer(t)
	trig := `{"steps":[{"type":"http","name":"echo","method":"POST","url":"` + srv.URL + `/echo","body":{"note1":"${ECHO_CHECK_PASSWORD}","note2":"${ECHO_CHECK_OTHER}","keep":"hello"}}]}`
	expect := "### Runnable\n- step echo: status=200\n"
	block := "\n## COMPARE\n- **Reference**: measured\n- **Output**: status, body\n"
	c := &config.Config{CheckEnv: []string{"ECHO_CHECK_PASSWORD", "ECHO_CHECK_OTHER"}}
	run := runCmp(t, c, map[string]string{"CHN-C1": cmpChainMD("CHN-C1", trig, expect, block)}, "compare", nil)
	if row := run.row(t, "CHN-C1"); row.Status != "passed" {
		t.Fatalf("status %q %+v", row.Status, row.Failure)
	}
	got := storedChainOutput(t, run, "echo")
	if !strings.Contains(got, `"keep":"hello"`) {
		t.Fatalf("the echoed body was not stored, so this test proves nothing: %s", got)
	}
	for _, leak := range []string{canary, other} {
		if strings.Contains(got, leak) {
			t.Errorf("the stored output carries a declared check_env value %q:\n%s", leak, got)
		}
	}
}

// The parked form (a name used as a credential field, `targets.auth.bearer_token`) stays scrubbed.
func TestCompareMode_ParkedBearerTokenValueIsStillScrubbed(t *testing.T) {
	const canary = "Cnry-parked-bearer-5521"
	t.Setenv("ECHO_PARKED_TOKEN", canary)
	srv := echoBodyServer(t)
	trig := `{"steps":[{"type":"http","name":"echo","method":"POST","url":"` + srv.URL + `/echo","body":{"note":"${ECHO_PARKED_TOKEN}","keep":"hello"}}]}`
	expect := "### Runnable\n- step echo: status=200\n"
	block := "\n## COMPARE\n- **Reference**: measured\n- **Output**: status, body\n"
	c := &config.Config{}
	c.Targets.Auth = &config.AuthTarget{BearerToken: canary}
	run := runCmp(t, c, map[string]string{"CHN-C1": cmpChainMD("CHN-C1", trig, expect, block)}, "compare", nil)
	got := storedChainOutput(t, run, "echo")
	if !strings.Contains(got, `"keep":"hello"`) {
		t.Fatalf("the echoed body was not stored, so this test proves nothing: %s", got)
	}
	if strings.Contains(got, canary) {
		t.Errorf("the stored output carries the parked bearer_token value:\n%s", got)
	}
}
