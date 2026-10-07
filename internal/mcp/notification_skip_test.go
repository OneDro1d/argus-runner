package mcp

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// — the live server sends notifications/message events BEFORE the result on the same
// text/event-stream response. The client must read events until one with an `id`.

const notif1 = `{"jsonrpc":"2.0","method":"notifications/message","params":{"level":"info","data":"one"}}`
const notif2 = `{"jsonrpc":"2.0","method":"notifications/message","params":{"level":"info","data":"two"}}`

func sseMsg(w http.ResponseWriter, event, data string) {
	if event != "" {
		fmt.Fprintf(w, "event: %s\n", event)
	}
	fmt.Fprintf(w, "data: %s\n\n", data)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func rpcResult(id int, text string) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"content":[{"type":"text","text":%q}],"isError":false}}`, id, text)
}

// streamableServer answers initialize with a plain JSON result and tools/call with two notifications then
// the result on one event stream. initNotes makes the initialize answer an event stream with notifications too.
func streamableServer(t *testing.T, initNotes bool, callEvents func(w http.ResponseWriter)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b strings.Builder
		buf := make([]byte, 4096)
		n, _ := r.Body.Read(buf)
		b.Write(buf[:n])
		body := b.String()
		switch {
		case strings.Contains(body, `"initialize"`):
			if initNotes {
				w.Header().Set("Content-Type", "text/event-stream")
				sseMsg(w, "message", notif1)
				sseMsg(w, "message", notif2)
				sseMsg(w, "message", `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-03-26","capabilities":{}}}`)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-03-26","capabilities":{}}}`)
		case strings.Contains(body, `"tools/call"`):
			w.Header().Set("Content-Type", "text/event-stream")
			callEvents(w)
		default:
			w.WriteHeader(http.StatusAccepted)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestStreamable_SkipsNotificationsBeforeTheResult(t *testing.T) {
	for _, initNotes := range []bool{false, true} {
		srv := streamableServer(t, initNotes, func(w http.ResponseWriter) {
			sseMsg(w, "message", notif1)
			sseMsg(w, "message", notif2)
			sseMsg(w, "message", rpcResult(2, "the answer"))
		})
		cr := (&Client{ServerURL: srv.URL, Transport: Streamable}).Call(CallInput{Tool: "t"})
		if cr.TransportErr != "" || len(cr.Content) != 1 || cr.Content[0].Text != "the answer" {
			t.Fatalf("initNotes=%v: the notification was read as the answer: %+v", initNotes, cr)
		}
	}
}

func TestStreamable_OnlyNotificationsIsANamedError(t *testing.T) {
	srv := streamableServer(t, false, func(w http.ResponseWriter) {
		sseMsg(w, "message", notif1)
		sseMsg(w, "message", notif2)
	})
	cr := (&Client{ServerURL: srv.URL, Transport: Streamable}).Call(CallInput{Tool: "t"})
	if !strings.Contains(cr.TransportErr, "without a JSON-RPC response") {
		t.Fatalf("want a named error for a stream of notifications only; got %+v", cr)
	}
}

// legacySSEServer: GET /sse streams `endpoint`, then (after each POST) the scripted events.
func legacySSEServer(t *testing.T, onlyNotesForCall bool) *httptest.Server {
	t.Helper()
	posts := make(chan string, 8)
	mux := http.NewServeMux()
	mux.HandleFunc("/sse", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		sseMsg(w, "endpoint", "/message?sessionId=s1")
		for {
			select {
			case body := <-posts:
				switch {
				case strings.Contains(body, `"initialize"`):
					sseMsg(w, "message", notif1)
					sseMsg(w, "message", notif2)
					sseMsg(w, "message", `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2024-11-05","capabilities":{}}}`)
				case strings.Contains(body, `"tools/call"`):
					sseMsg(w, "message", notif1)
					sseMsg(w, "message", notif2)
					if onlyNotesForCall {
						return // the stream ends with only notifications
					}
					sseMsg(w, "message", rpcResult(2, "legacy answer"))
				}
			case <-r.Context().Done():
				return
			}
		}
	})
	mux.HandleFunc("/message", func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 4096)
		n, _ := r.Body.Read(buf)
		posts <- string(buf[:n])
		w.WriteHeader(http.StatusAccepted)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestLegacySSE_SkipsNotificationsOnInitializeAndToolsCall(t *testing.T) {
	srv := legacySSEServer(t, false)
	cr := (&Client{ServerURL: srv.URL, Transport: LegacySSE}).Call(CallInput{Tool: "t"})
	if cr.TransportErr != "" || len(cr.Content) != 1 || cr.Content[0].Text != "legacy answer" {
		t.Fatalf("the notification was read as the answer: %+v", cr)
	}
}

func TestLegacySSE_OnlyNotificationsIsANamedError(t *testing.T) {
	srv := legacySSEServer(t, true)
	cr := (&Client{ServerURL: srv.URL, Transport: LegacySSE}).Call(CallInput{Tool: "t"})
	if !strings.Contains(cr.TransportErr, "without a JSON-RPC response") {
		t.Fatalf("want a named error for a stream of notifications only; got %+v", cr)
	}
}
