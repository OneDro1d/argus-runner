package obslog

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func bufLogger() (*slog.Logger, *bytes.Buffer) {
	var b bytes.Buffer
	return slog.New(slog.NewJSONHandler(&b, &slog.HandlerOptions{Level: slog.LevelDebug})), &b
}

// A request with no correlation header gets one assigned, echoed on the response, and visible on the
// handler's context; Begin/Completed are logged as structured JSON with that id + the status.
func TestMiddleware_AssignsCorrelationAndLogsPair(t *testing.T) {
	lg, buf := bufLogger()
	var seen string
	h := Middleware(lg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = Correlation(r.Context())
		w.WriteHeader(http.StatusTeapot)
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/fed/poll", nil))

	cid := rec.Header().Get(CorrelationHeader)
	if cid == "" || !strings.HasPrefix(cid, "cid_") {
		t.Fatalf("no correlation id echoed: %q", cid)
	}
	if seen != cid {
		t.Fatalf("handler ctx correlation %q != response %q", seen, cid)
	}

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 log lines (Begin+Completed), got %d: %s", len(lines), buf.String())
	}
	var begin, done map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &begin); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &done); err != nil {
		t.Fatal(err)
	}
	if begin["correlation_id"] != cid || done["correlation_id"] != cid {
		t.Fatalf("log correlation mismatch: %v / %v vs %q", begin["correlation_id"], done["correlation_id"], cid)
	}
	if done["msg"] != "http request Completed" {
		t.Fatalf("second line not Completed: %v", done["msg"])
	}
	if got := done["status"]; got != float64(http.StatusTeapot) {
		t.Fatalf("status not captured: got %v want %d", got, http.StatusTeapot)
	}
	if _, ok := done["duration_ms"]; !ok {
		t.Fatalf("no duration_ms on Completed: %s", lines[1])
	}
	// probe-path requests log at DEBUG (still emitted here since the buffer logger is debug-level),
	// but the point is they carry the correlation id too.
}

// An inbound correlation header is PROPAGATED, not replaced (the executor↔CP hop, §5.4 item 3).
func TestMiddleware_PropagatesInboundCorrelation(t *testing.T) {
	lg, _ := bufLogger()
	var seen string
	h := Middleware(lg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = Correlation(r.Context())
	}))
	req := httptest.NewRequest("POST", "/fed/results", nil)
	req.Header.Set(CorrelationHeader, "run_20260714T1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if seen != "run_20260714T1" || rec.Header().Get(CorrelationHeader) != "run_20260714T1" {
		t.Fatalf("inbound correlation not propagated: ctx=%q hdr=%q", seen, rec.Header().Get(CorrelationHeader))
	}
}

func TestNewID_PrefixedAndUnique(t *testing.T) {
	a, b := NewID(), NewID()
	if a == b {
		t.Fatal("NewID collision")
	}
	if !strings.HasPrefix(a, "cid_") || len(a) != len("cid_")+32 {
		t.Fatalf("bad id shape: %q", a)
	}
}

func TestCorrelation_RoundTrip(t *testing.T) {
	ctx := WithCorrelation(context.Background(), "x")
	if Correlation(ctx) != "x" {
		t.Fatal("ctx round-trip failed")
	}
	if Correlation(context.Background()) != "" {
		t.Fatal("absent correlation should be empty")
	}
}
