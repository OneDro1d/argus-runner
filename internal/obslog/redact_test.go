package obslog

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The public view's slug IS its credential (see redactPath). This proves the middleware never writes
// one to the log, at ANY level — bufLogger here is at LevelDebug precisely because demoting /public
// to Debug would NOT have protected it: this package is debug-on by default, so a Debug line is
// still emitted and still shipped.
func TestMiddleware_RedactsPublicSlugFromLogs(t *testing.T) {
	const slug = "s3cret-slug-value-that-must-never-be-logged"

	lg, buf := bufLogger()
	h := Middleware(lg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The HANDLER still sees the real path — redaction is for the log record only.
		if got := r.URL.Path; got != "/public/"+slug {
			t.Errorf("handler saw path %q, want the unredacted /public/%s", got, slug)
		}
		w.WriteHeader(http.StatusOK)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/public/"+slug, nil))

	out := buf.String()
	if strings.Contains(out, slug) {
		t.Fatalf("the slug reached the log:\n%s", out)
	}
	// Both lines (Begin and Completed) must carry the redacted path, not just the first.
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 log lines, got %d:\n%s", len(lines), out)
	}
	for i, ln := range lines {
		var m map[string]any
		if err := json.Unmarshal([]byte(ln), &m); err != nil {
			t.Fatalf("line %d is not JSON: %v", i, err)
		}
		if m["path"] != "/public/<redacted>" {
			t.Errorf("line %d path = %v, want /public/<redacted>", i, m["path"])
		}
	}
}

// Redaction is scoped to the slug segment: every other path is logged verbatim, so the change cannot
// quietly blind the request log anywhere else.
func TestRedactPath_OnlyTouchesTheSlugSegment(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/public/abc", "/public/<redacted>"},
		{"/public/abc/def", "/public/<redacted>"}, // a multi-segment near-miss is still hidden
		{"/public/", "/public/"},                  // no slug present to hide
		{"/public", "/public"},
		{"/publicity/report", "/publicity/report"}, // prefix collision, not a public path
		{"/api/v1/runs", "/api/v1/runs"},
		{"/healthz", "/healthz"},
		{"/", "/"},
	}
	for _, c := range cases {
		if got := redactPath(c.in); got != c.want {
			t.Errorf("redactPath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
