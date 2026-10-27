package obsquery

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPushLogStreams_BodyOrderAndTarget(t *testing.T) {
	var gotBody lokiPushBody
	var gotHdr http.Header
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		gotHdr, gotPath = r.Header.Clone(), r.URL.Path
		w.WriteHeader(204)
	}))
	defer srv.Close()
	base := time.Date(2026, 10, 11, 10, 0, 0, 0, time.UTC)
	l := &Loki{BaseURL: srv.URL, Tenant: "t1", Credential: "u:p"}
	err := PushLogStreams(context.Background(), l, []LogStream{
		{Labels: map[string]string{"service": "a"}, Entries: []LogEntry{{base.Add(2 * time.Second), "second"}, {base, "first  with  spaces\r"}}},
		{Labels: map[string]string{"service": "empty"}},
	}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/loki/api/v1/push" || gotHdr.Get("X-Scope-OrgID") != "t1" || gotHdr.Get("Content-Type") != "application/json" {
		t.Errorf("path %q headers %v", gotPath, gotHdr)
	}
	if len(gotBody.Streams) != 1 {
		t.Fatalf("streams = %d, want 1 (the empty one is not sent)", len(gotBody.Streams))
	}
	v := gotBody.Streams[0].Values
	if len(v) != 2 || v[0][1] != "first  with  spaces\r" || v[1][1] != "second" || v[0][0] >= v[1][0] {
		t.Errorf("values = %v", v)
	}
}

func TestPushLogStreams_NoopsAndStatusError(t *testing.T) {
	if err := PushLogStreams(context.Background(), nil, []LogStream{{Entries: []LogEntry{{time.Now(), "x"}}}}, time.Second); err != nil {
		t.Errorf("nil Loki: %v", err)
	}
	if err := PushLogStreams(context.Background(), &Loki{BaseURL: "http://x.invalid"}, nil, time.Second); err != nil {
		t.Errorf("nothing to send: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		_, _ = w.Write([]byte("rate limited"))
	}))
	defer srv.Close()
	err := PushLogStreams(context.Background(), &Loki{BaseURL: srv.URL}, []LogStream{{Labels: map[string]string{"a": "b"}, Entries: []LogEntry{{time.Now(), "x"}}}}, time.Second)
	var pse *PushStatusError
	if !errors.As(err, &pse) || pse.Status != 429 || !strings.Contains(err.Error(), "loki push returned 429: rate limited") {
		t.Errorf("err = %v", err)
	}
}
