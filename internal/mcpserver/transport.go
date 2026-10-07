package mcpserver

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// sseKeepAlive is how often the SSE stream emits a keepalive comment to stop an idle
// stream (and therefore its session) from being dropped by the client/OS/intermediary
// (M25-DF-01). A package var so tests can shorten it.
var sseKeepAlive = 15 * time.Second

// HTTPHandler is the legacy HTTP+SSE 2-endpoint transport (Camp-B / mario shape):
//
//	GET  /sse                       opens the stream; emits an `endpoint` event with the POST URL + session id
//	POST /message?sessionId=<id>    JSON-RPC in; 202; the response is delivered back over the correlated SSE stream
//	GET  /metrics                   the server-side tool-call metrics
//
// Correlation is BY SESSION ID (minted on the SSE connect). The bearer token rides
// the Authorization header on the POST. If a POST arrives for a session with no live
// SSE stream (e.g. a direct request/response client), the response is returned inline.
type HTTPHandler struct {
	srv     *Server
	mu      sync.Mutex
	streams map[string]chan []byte
	// ready answers "can this component actually serve?", as opposed to "is the process up?".
	//
	// nil means unconditionally ready, which is what the control plane and the runner have always
	// been. Only the ROUTER supplies one (VR8-K2 / V26-003): its state directory is a host bind
	// mount that can be deleted underneath it, and when that happened the container reported
	// (healthy) for 13 hours while every agent on the machine was unroutable.
	ready func() error
	// WWWAuthenticate, when set, is the exact value of the WWW-Authenticate header sent on a 401 (no /
	// invalid / expired token) — per the MCP authorization spec's use of RFC 6750 §3. Empty means the
	// header is omitted (a caller that never configures it keeps today's shape minus the status code
	// change). Set by the control plane at wiring time (control.go) — this package does not know its
	// own issuer URL or whether a protected-resource metadata endpoint exists.
	WWWAuthenticate string
}

func NewHTTPHandler(srv *Server) *HTTPHandler {
	return &HTTPHandler{srv: srv, streams: map[string]chan []byte{}}
}

// SetReady installs the readiness probe. Call it before serving.
//
// ⛔ IT AFFECTS /ready AND /health/ready ONLY. /healthz and /health/live stay unconditional on
// purpose: Kubernetes KILLS a pod that fails liveness, and the condition this probe reports — a
// state directory that vanished from the host — is not something a restart can fix. Wiring it to
// liveness would turn a silent degradation into an unbounded crash-loop.
func (h *HTTPHandler) SetReady(probe func() error) { h.ready = probe }

func (h *HTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/sse":
		h.handleSSE(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/message":
		h.handleMessage(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/mcp":
		h.handleStreamable(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/metrics":
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		io.WriteString(w, h.srv.Metrics())
	case r.URL.Path == "/healthz" || r.URL.Path == "/health/live":
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "ok")
	case r.URL.Path == "/ready" || r.URL.Path == "/health/ready":
		if h.ready != nil {
			if err := h.ready(); err != nil {
				// NAME THE CAUSE. "not ready" on its own is the same dead end "healthy" was: it
				// tells an operator that something is wrong and nothing about what.
				w.WriteHeader(http.StatusServiceUnavailable)
				io.WriteString(w, "not ready: "+err.Error())
				return
			}
		}
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "ready")
	default:
		http.NotFound(w, r)
	}
}

func (h *HTTPHandler) handleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	sid := newSessionID()
	ch := make(chan []byte, 32)
	h.mu.Lock()
	h.streams[sid] = ch
	h.mu.Unlock()
	h.srv.NewSession(sid)
	defer func() {
		h.mu.Lock()
		delete(h.streams, sid)
		h.mu.Unlock()
		h.srv.EndSession(sid)
	}()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "event: endpoint\ndata: /message?sessionId=%s\n\n", sid)
	flusher.Flush()

	// keepalive: emit an SSE comment line every sseKeepAlive so an idle stream isn't
	// closed (which would EndSession this sid and make the next tool call -32003). Per
	// the SSE spec a line beginning with ':' is a comment the client ignores (M25-DF-01).
	ka := time.NewTicker(sseKeepAlive)
	defer ka.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ka.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		case msg := <-ch:
			fmt.Fprintf(w, "event: message\ndata: %s\n\n", msg)
			flusher.Flush()
		}
	}
}

func (h *HTTPHandler) handleMessage(w http.ResponseWriter, r *http.Request) {
	sid := r.URL.Query().Get("sessionId")
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	token := reqToken(r)

	// checked BEFORE Dispatch so the 401 decision does not depend on parsing the JSON-RPC body Dispatch
	// returns (D-CP-MCP.AUTH / the MCP authorization spec): no / invalid / expired token answers 401 +
	// WWW-Authenticate regardless of what was asked. Dispatch still runs the SAME Authenticate call right
	// after and still produces the -32001 JSON-RPC body — kept for a client that reads only that.
	if h.srv.CheckAuth(token) != nil {
		h.writeUnauthorized(w)
		resp, hasResp := h.srv.Dispatch(sid, token, body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		if hasResp {
			w.Write(resp)
		}
		return
	}

	resp, hasResp := h.srv.Dispatch(sid, token, body)

	h.mu.Lock()
	ch, hasStream := h.streams[sid]
	h.mu.Unlock()

	switch {
	case hasResp && hasStream:
		ch <- resp
		w.WriteHeader(http.StatusAccepted)
	case hasResp:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(resp)
	default:
		w.WriteHeader(http.StatusAccepted) // a notification: nothing to return
	}
}

// writeUnauthorized sets the WWW-Authenticate header (when configured) — must be called before
// w.WriteHeader, which commits the response headers.
func (h *HTTPHandler) writeUnauthorized(w http.ResponseWriter) {
	if h.WWWAuthenticate != "" {
		w.Header().Set("WWW-Authenticate", h.WWWAuthenticate)
	}
}

// handleStreamable is the modern MCP transport (Streamable HTTP, D-CP-MCP.TRANSPORT): a single POST
// /mcp carries one JSON-RPC message and the response is returned INLINE as JSON — no SSE stream needed.
// The session rides the `Mcp-Session-Id` header: absent → the server mints one (the `initialize` case)
// and returns it; a well-behaved client echoes it on every subsequent request. A response-less message
// (a notification) yields 202.
func (h *HTTPHandler) handleStreamable(w http.ResponseWriter, r *http.Request) {
	sid := r.Header.Get("Mcp-Session-Id")
	if sid == "" {
		sid = newSessionID()
		h.srv.NewSession(sid)
	} else {
		// R3/R2 (CP-M3-118): a client-supplied id unknown to THIS replica = the multi-replica
		// failover case — adopt it as ready so the streamable face is replica-agnostic.
		h.srv.AdoptSession(sid)
	}
	w.Header().Set("Mcp-Session-Id", sid)
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	token := reqToken(r)

	// See handleMessage's comment: checked before Dispatch, same Authenticate call either way, HTTP
	// status is the ONLY thing this changes. Per the MCP authorization spec §2.1 (and RFC 6750 §3), a
	// request with no bearer token, or an invalid/expired one, gets 401 + WWW-Authenticate — the
	// JSON-RPC -32001 body still rides along for a client that reads only that.
	if h.srv.CheckAuth(token) != nil {
		h.writeUnauthorized(w)
		resp, hasResp := h.srv.Dispatch(sid, token, body)
		if !hasResp {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write(resp)
		return
	}

	resp, hasResp := h.srv.Dispatch(sid, token, body)
	if !hasResp {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(resp)
}

func bearer(h string) string {
	const p = "Bearer "
	if strings.HasPrefix(h, p) {
		return strings.TrimPrefix(h, p)
	}
	return h
}

// reqToken reads the bearer from the Authorization header, falling back to a ?token= query param.
// S7 (CP-M3-III-5, §D-3.1.4/ADR-9): an AUTHOR PAT (odts_...) in a URL leaks into gateway/proxy access logs
// on an internet-facing CP, so the query form is REJECTED for odts_ tokens — authors present the
// Authorization header (Hub's MCP-router sends `Authorization: Bearer`, and onboarding is header-only).
// The ?token= form is RETAINED only for the static-gateway credential class (UC026 — a log-stripped, non-PAT
// token, per the owner's Q1 ruling). The "odts_" prefix mirrors store.AuthorTokenPrefix (kept local to avoid
// a control-layer import from this transport package).
func reqToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		return bearer(h)
	}
	if q := r.URL.Query().Get("token"); !strings.HasPrefix(q, "odts_") {
		return q
	}
	return ""
}

func newSessionID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
