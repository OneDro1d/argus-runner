package argus

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/report"
)

// What reaches report.json through responseMessage after credentialScrub: a transport-failure text can
// quote a URL (java.net.URISyntaxException carries all of it) holding a short secret in a query or a
// path, which the credential scrub (declared credentials, long or prefixed tokens) does not know.
func TestLoadErrorReasonCarriesNoSecretFromAQuotedURL(t *testing.T) {
	c := &config.Config{}
	scrub := credentialScrub(c)
	msgs := []string{
		"Illegal character in query at index 31: http://sut.internal/api/v1/orders?token=hunter2&x=a b",
		"Illegal character in path at index 20: https://sut/pay/key-sk_live_short/x y",
		"Connect to sut:443 failed: Authorization: Basic dXNlcjpwYXNz",
		"http://user:pass@sut.internal/x unreachable",
		"Illegal character in query at index 40: http://sut/x?api_key=AbC123xyZ9Qw&y=1 z",
	}
	var ss []report.LoadSample
	for _, m := range msgs {
		ss = append(ss, report.LoadSample{Code: "Non HTTP response code: java.lang.IllegalArgumentException", Message: "Non HTTP response message: " + m})
	}
	errs := report.LoadErrorsFrom(ss, scrub)
	if len(errs) != len(msgs) {
		t.Fatalf("want %d distinct reasons, got %+v", len(msgs), errs)
	}
	for _, e := range errs {
		for _, secret := range []string{"hunter2", "sk_live_short", "user:pass", "AbC123xyZ9Qw", "dXNlcjpwYXNz"} {
			if strings.Contains(e.Reason, secret) {
				t.Errorf("secret %q survives in report reason: %s", secret, e.Reason)
			}
		}
		if !strings.HasPrefix(e.Reason, "java.lang.IllegalArgumentException: ") {
			t.Errorf("the class name must survive: %s", e.Reason)
		}
	}
}

// A corrupt .jtl row (a timeStamp that does not parse, a negative elapsed) must not panic the reader or
// the timeline, and the row stays in the load sample set (it has no start time, so only the timeline leaves it out).
func TestReadJTLLoadSamples_CorruptRowsDoNotPanicAndAreKept(t *testing.T) {
	jtl := filepath.Join(t.TempDir(), "run.jtl")
	rows := "timeStamp,elapsed,label,responseCode,responseMessage\n" +
		"garbage,-5000,S-1,200,OK\n" +
		"1000000,-7,S-1,200,OK\n" +
		"1003000,10,S-1,200,OK\n" +
		",x,S-1,500,Err\n"
	if err := os.WriteFile(jtl, []byte(rows), 0o644); err != nil {
		t.Fatal(err)
	}
	ss := readJTLLoadSamples(jtl, "S")
	if len(ss) != 4 {
		t.Fatalf("every row is kept, got %d", len(ss))
	}
	_, b := report.LoadTimelineFrom(ss)
	started := 0
	for _, x := range b {
		started += x.Started
	}
	if started != 2 {
		t.Errorf("only the two rows with a start time are on the timeline, got %d", started)
	}
}
