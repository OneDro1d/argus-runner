package mcpserver

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// VR-H7 / UC-65/76 + the recursive-dogfood transport (UC-51): the legacy HTTP+SSE
// 2-endpoint flow works end-to-end over real HTTP — GET /sse mints a session and
// emits the endpoint event; POSTed JSON-RPC is answered over the correlated SSE stream.
func TestHTTPSSE_EndToEnd(t *testing.T) {
	srv := newSrv(t)
	ts := httptest.NewServer(NewHTTPHandler(srv))
	defer ts.Close()

	sse, err := http.Get(ts.URL + "/sse")
	if err != nil {
		t.Fatalf("GET /sse: %v", err)
	}
	defer sse.Body.Close()
	br := bufio.NewReader(sse.Body)

	ev, data := readSSE(t, br)
	if ev != "endpoint" || !strings.HasPrefix(data, "/message?sessionId=") {
		t.Fatalf("first SSE event must be the endpoint, got %q/%q", ev, data)
	}
	msgURL := ts.URL + data

	post := func(body []byte) {
		req, _ := http.NewRequest(http.MethodPost, msgURL, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+rtok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		resp.Body.Close()
	}

	// handshake
	post(reqBytes(1, "initialize", map[string]any{}))
	if _, d := readSSE(t, br); !strings.Contains(d, ProtocolVersion) {
		t.Fatalf("initialize response missing protocolVersion: %s", d)
	}
	post(notifBytes("notifications/initialized", nil)) // 202, no SSE message

	// tools/list over the wire
	post(reqBytes(2, "tools/list", nil))
	if _, d := readSSE(t, br); !strings.Contains(d, "runner__ping") {
		t.Fatalf("tools/list response missing tools: %s", d)
	}

	// tools/call over the wire
	post(reqBytes(3, "tools/call", callParamsFor("runner__ping")))
	_, d := readSSE(t, br)
	var r Response
	if err := json.Unmarshal([]byte(d), &r); err != nil {
		t.Fatalf("tools/call SSE payload not a JSON-RPC response: %v (%s)", err, d)
	}
	if r.Error != nil || !strings.Contains(d, "pong") {
		t.Fatalf("tools/call did not succeed over SSE: %s", d)
	}
}

// /metrics is exposed on the HTTP transport (VR-H11).
func TestHTTP_MetricsEndpoint(t *testing.T) {
	srv := newSrv(t)
	ts := httptest.NewServer(NewHTTPHandler(srv))
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("/metrics status = %d", resp.StatusCode)
	}
}

// M25-DF-01: an idle SSE stream must emit periodic keepalive comments so it (and its
// session) is not dropped. Shorten the interval so the test is fast; assert a ':' comment
// line arrives on an otherwise-silent stream (no POSTs).
func TestHTTPSSE_Keepalive(t *testing.T) {
	old := sseKeepAlive
	sseKeepAlive = 20 * time.Millisecond
	defer func() { sseKeepAlive = old }()

	srv := newSrv(t)
	ts := httptest.NewServer(NewHTTPHandler(srv))
	defer ts.Close()

	sse, err := http.Get(ts.URL + "/sse")
	if err != nil {
		t.Fatalf("GET /sse: %v", err)
	}
	defer sse.Body.Close()
	br := bufio.NewReader(sse.Body)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("sse read: %v", err)
		}
		if strings.HasPrefix(line, ":") { // an SSE comment = our keepalive
			return
		}
	}
	t.Fatal("idle SSE stream produced no keepalive comment within the window (M25-DF-01)")
}

// readSSE reads one complete SSE event (until the blank line), returning event + data.
func readSSE(t *testing.T, br *bufio.Reader) (event, data string) {
	t.Helper()
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("sse read: %v", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if event != "" || data != "" {
				return
			}
			continue
		}
		if strings.HasPrefix(line, "event:") {
			event = strings.TrimSpace(line[len("event:"):])
		}
		if strings.HasPrefix(line, "data:") {
			data = strings.TrimSpace(line[len("data:"):])
		}
	}
}
