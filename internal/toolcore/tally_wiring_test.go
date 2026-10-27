package toolcore

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/argus"
)

// tallyChain is a one-step http chain whose step expects status 200 from url.
func tallyChain(id, url string) string {
	return strings.Join([]string{
		"# Scenario: " + id, "", "## Metadata", "- **ID**: " + id, "- **Layer**: Permissions", "- **Tags**: chain", "",
		"## TRIGGER", "POST `chain`", "", "```json",
		`{"steps":[{"type":"http","name":"read","method":"GET","url":"` + url + `"}]}`, "```", "",
		"## EXPECT", "### Runnable", "- step read: status=200", "", "## TIMEOUT", "60s", "",
		"## CLEANUP", "N/A — a unit-test fixture; it creates nothing.", "",
	}, "\n")
}

// Env.Tally is the one wire from the executor's heartbeat to the run loop: toolcore.Run hands it to
// argus.RunAllWithTally, so a real run through the real core feeds the counts the heartbeat reads.
func TestRun_FeedsEnvTally(t *testing.T) {
	t.Setenv(probeDisableEnv, "1")
	sut := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/bad") {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer sut.Close()
	e := rlEnv(t, "project:\n  name: t\n", map[string]string{
		"CHN-OK":  tallyChain("CHN-OK", sut.URL+"/ok"),
		"CHN-BAD": tallyChain("CHN-BAD", sut.URL+"/bad"),
	})
	e.ResultsRoot = t.TempDir()
	e.Instance = "local"
	e.Tally = argus.NewTally()
	if _, _, err := Run(e, "", "", "", ""); err != nil {
		t.Fatal(err)
	}
	got := e.Tally.Snapshot()
	if got.Planned != 2 || got.Total != 2 || got.Passed != 1 || got.Failed != 1 {
		t.Fatalf("tally after a real run = %+v; want planned 2, total 2, passed 1, failed 1", got)
	}
}

// A nil Env.Tally is today's run: it completes and reports.
func TestRun_NilEnvTallyRunsAsBefore(t *testing.T) {
	t.Setenv(probeDisableEnv, "1")
	sut := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer sut.Close()
	e := rlEnv(t, "project:\n  name: t\n", map[string]string{"CHN-OK": tallyChain("CHN-OK", sut.URL+"/ok")})
	e.ResultsRoot = t.TempDir()
	e.Instance = "local"
	if _, _, err := Run(e, "", "", "", ""); err != nil {
		t.Fatal(err)
	}
}
