package chain

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// http_basicauth_test.go — item 25(a): the basic-auth helper on a chain `http` step. The scenario
// never carries a literal secret; it names a username and the NAME of an env var holding the
// password, and the `Authorization: Basic <base64>` header is built here, in the executor, at run
// time (chain.HTTPStep's buildRequest, called on every attempt).

// TestHTTPStep_BasicAuthHelperSendsEncodedHeader is the positive control: given
// `Authorization: Basic ${basic_auth:alice:ARGUS_I25_PASS}` and ARGUS_I25_PASS=s3cr3t in the
// environment, the SUT must receive the CORRECT standard-form Basic header, and neither the
// password nor the encoded header may appear anywhere in the step's own report.
func TestHTTPStep_BasicAuthHelperSendsEncodedHeader(t *testing.T) {
	const user, pass = "alice", "s3cr3t-basic-auth-value"
	t.Setenv("ARGUS_I25_PASS", pass)

	var gotAuth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(200)
	}))
	defer ts.Close()

	headers := map[string]string{"Authorization": "Basic ${basic_auth:" + user + ":ARGUS_I25_PASS}"}
	step := HTTPStep("login", "GET", ts.URL+"/x", headers, "", 200, nil, nil, nil, false,
		nil, &scenario.MoneySpendLedger{})
	res := step.Run("cid", map[string]string{})

	wantHeader := "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
	if gotAuth != wantHeader {
		t.Fatalf("SUT received Authorization %q, want %q", gotAuth, wantHeader)
	}
	if res.Status != "passed" {
		t.Fatalf("status = %q, want passed: %+v", res.Status, res)
	}

	// VR-C8 / item 25: never a secret in the report. Grep the WHOLE serialized StepResult.
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	if strings.Contains(string(b), pass) {
		t.Fatalf("the raw password leaked into the report: %s", b)
	}
	if strings.Contains(string(b), wantHeader) {
		t.Fatalf("the encoded Authorization header leaked into the report: %s", b)
	}
}

// TestHTTPStep_BasicAuthHelperUnsetEnvVarFailsWithClearReason: an unset password env var must
// refuse the step BY NAME — never silently send `Basic <base64("alice:")>` (an empty credential).
func TestHTTPStep_BasicAuthHelperUnsetEnvVarFailsWithClearReason(t *testing.T) {
	os.Unsetenv("ARGUS_I25_MISSING_PASS")

	var hit bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		w.WriteHeader(200)
	}))
	defer ts.Close()

	headers := map[string]string{"Authorization": "Basic ${basic_auth:alice:ARGUS_I25_MISSING_PASS}"}
	step := HTTPStep("login", "GET", ts.URL+"/x", headers, "", 200, nil, nil, nil, false,
		nil, &scenario.MoneySpendLedger{})
	res := step.Run("cid", map[string]string{})

	if hit {
		t.Fatal("the SUT was hit despite an unset password env var")
	}
	if res.Status != "failed" {
		t.Fatalf("status = %q, want failed: %+v", res.Status, res)
	}
	if !strings.Contains(res.Observed, "ARGUS_I25_MISSING_PASS") {
		t.Errorf("observed must name the missing env var: %q", res.Observed)
	}
	if !strings.Contains(res.Observed, "not set") {
		t.Errorf("observed must give a clear reason, not just the var name: %q", res.Observed)
	}
}
