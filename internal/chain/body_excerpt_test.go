package chain

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/mcp"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// A chain http step whose STATUS claim missed carries a bounded,
// redacted excerpt of the response body (author-only), and the builder-visible text no longer names the
// wanted status. 299 is the wanted status throughout: no digit run of it can collide with anything else.

const exWant = 299

func exStep(url string, opts ...HTTPOption) Step {
	return HTTPStep("check", "GET", url, nil, "", exWant, nil, nil, nil, false, nil, &scenario.MoneySpendLedger{}, opts...)
}

func exServer(status int, body []byte) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
}

func TestBodyExcerpt_StatusMissCarriesTheMessageAndNotTheWantedStatus(t *testing.T) {
	sut := exServer(422, []byte(`{"error":"name must not be empty"}`))
	defer sut.Close()
	res := Run("tr-x", []Step{exStep(sut.URL)})
	if res.Status != "failed" || len(res.Steps) != 1 {
		t.Fatalf("want a failed one-step chain, got %+v", res)
	}
	st := res.Steps[0]
	if !strings.Contains(st.BodyExcerpt, "name must not be empty") {
		t.Errorf("body_excerpt lacks the SUT's message: %q", st.BodyExcerpt)
	}
	if !strings.Contains(st.Observed, "422") || strings.Contains(st.Observed, "299") {
		t.Errorf("step observed must name 422 and not the wanted 299: %q", st.Observed)
	}
	if res.Failure == nil || !strings.Contains(res.Failure.Observed, "422") || strings.Contains(res.Failure.Observed, "299") {
		t.Errorf("failure.observed must name 422 and not the wanted 299: %+v", res.Failure)
	}
	if len(st.FailedClaims) != 1 || st.FailedClaims[0].Claim != "status = 299" {
		t.Errorf("the wanted status stays in the author-only failed claim: %+v", st.FailedClaims)
	}
}

func TestBodyExcerpt_SavedValueAndCredentialAreRedacted(t *testing.T) {
	sut := exServer(500, []byte(`{"error":"bad token SAVED-VALUE-123456 and key CRED-SECRET-ABCDEF"}`))
	defer sut.Close()
	scrub := func(s string) string { return strings.ReplaceAll(s, "CRED-SECRET-ABCDEF", "[cred]") }
	st := exStep(sut.URL, WithBodyScrub(scrub)).Run("c", map[string]string{"tok": "SAVED-VALUE-123456"})
	if strings.Contains(st.BodyExcerpt, "SAVED-VALUE-123456") || strings.Contains(st.BodyExcerpt, "CRED-SECRET-ABCDEF") {
		t.Fatalf("a secret reached the excerpt: %q", st.BodyExcerpt)
	}
	if !strings.Contains(st.BodyExcerpt, "${saved.tok}") || !strings.Contains(st.BodyExcerpt, "[cred]") {
		t.Errorf("positive control: both redactions should be visible: %q", st.BodyExcerpt)
	}
}

func TestBodyExcerpt_ASecretStraddlingTheCutIsStillRedacted(t *testing.T) {
	secret := "STRADDLING-SECRET-VALUE"
	// the secret starts 5 bytes before the 512 cut, so a cut-then-redact would leave its first 5 bytes
	body := strings.Repeat("a", report.MaxBodyExcerpt-5) + secret + strings.Repeat("b", 100)
	sut := exServer(500, []byte(body))
	defer sut.Close()
	scrub := func(s string) string { return strings.ReplaceAll(s, secret, "[cred]") }
	st := exStep(sut.URL, WithBodyScrub(scrub)).Run("c", map[string]string{})
	if strings.Contains(st.BodyExcerpt, "STRAD") {
		t.Fatalf("part of a secret survived the cut: %q", st.BodyExcerpt[len(st.BodyExcerpt)-40:])
	}
	st = exStep(sut.URL).Run("c", map[string]string{"s": secret})
	if strings.Contains(st.BodyExcerpt, "STRAD") {
		t.Fatalf("part of a saved value survived the cut: %q", st.BodyExcerpt[len(st.BodyExcerpt)-40:])
	}
}

func TestBodyExcerpt_BoundedOnAUTF8Boundary(t *testing.T) {
	sut := exServer(500, []byte(strings.Repeat("é", 2<<20/2))) // 2 MiB of two-byte characters
	defer sut.Close()
	st := exStep(sut.URL).Run("c", map[string]string{})
	if !strings.HasSuffix(st.BodyExcerpt, report.FailedClaimTruncatedSuffix) {
		t.Fatalf("a 2 MiB body must be cut and marked: %q", st.BodyExcerpt[:40])
	}
	text := strings.TrimSuffix(st.BodyExcerpt, report.FailedClaimTruncatedSuffix)
	if len(text) > report.MaxBodyExcerpt || strings.Contains(text, "�") || strings.Trim(text, "é") != "" {
		t.Errorf("cut must be at most %d bytes of whole characters, got %d bytes", report.MaxBodyExcerpt, len(text))
	}
}

func TestBodyExcerpt_BinaryAndEmptyBodies(t *testing.T) {
	bin := exServer(500, []byte{0xff, 0xfe, 0x00, 0x01, 0x02})
	defer bin.Close()
	if got := exStep(bin.URL).Run("c", map[string]string{}).BodyExcerpt; got != "(5 bytes, not text)" {
		t.Errorf("binary body: %q", got)
	}
	empty := exServer(500, nil)
	defer empty.Close()
	if got := exStep(empty.URL).Run("c", map[string]string{}).BodyExcerpt; got != "(empty body)" {
		t.Errorf("empty body: %q", got)
	}
}

func TestBodyExcerpt_NotSetOnAPassABodyMissOrATransportError(t *testing.T) {
	ok := exServer(299, []byte(`{"v":"echo-me"}`))
	defer ok.Close()
	if st := exStep(ok.URL).Run("c", map[string]string{}); st.Status != "passed" || st.BodyExcerpt != "" {
		t.Errorf("a pass carries no excerpt: %+v", st)
	}
	miss := HTTPStep("check", "GET", ok.URL, nil, "", exWant,
		[]mcp.BodyAssert{{Field: "v", Op: mcp.BodyEQOp, Value: "other"}}, nil, nil, false, nil, &scenario.MoneySpendLedger{})
	if st := miss.Run("c", map[string]string{}); st.Status != "failed" || st.BodyExcerpt != "" {
		t.Errorf("a body-claim miss with a matching status carries no excerpt: %+v", st)
	}
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := dead.URL
	dead.Close()
	if st := exStep(url).Run("c", map[string]string{}); st.Status != report.StatusError || st.BodyExcerpt != "" {
		t.Errorf("a transport error has no body and no excerpt: %+v", st)
	}
}

func TestBodyExcerpt_PollKeepsTheLastAttemptsBody(t *testing.T) {
	var n atomic.Int32
	sut := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			w.WriteHeader(500)
			_, _ = w.Write([]byte("body-A-first"))
			return
		}
		w.WriteHeader(422)
		_, _ = w.Write([]byte("body-B-last"))
	}))
	defer sut.Close()
	step := HTTPStep("check", "GET", sut.URL, nil, "", exWant, nil, nil,
		&Poll{Timeout: 300 * time.Millisecond, Interval: 20 * time.Millisecond}, false, nil, &scenario.MoneySpendLedger{})
	st := step.Run("c", map[string]string{})
	if st.BodyExcerpt != "body-B-last" {
		t.Errorf("the excerpt must come from the last attempt, got %q", st.BodyExcerpt)
	}
}
