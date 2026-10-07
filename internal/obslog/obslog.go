// Package obslog is the M3 structured-logging floor (D-CP-MCP.OPS point 4 · UC184 · UC169): JSON
// slog to stdout with correlation fields, a Begin/Completed request pair per HTTP call, and
// X-Correlation-ID assignment + propagation across the federation HTTP hops (§5.4 item 3). One
// logger shape for both deploy profiles (control + runner).
//
// Over-inform (the suite's own standard, CLAUDE.md): DEBUG-on by default — the operator tunes log
// VOLUME via LOG_LEVEL at the ingestion layer, never with `if env=="prod"` gates in code. The health
// floor + /metrics are logged at DEBUG so probe traffic stays out of the INFO stream.
package obslog

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
)

// CorrelationHeader carries the request correlation id across HTTP hops (executor ↔ control plane).
const CorrelationHeader = "X-Correlation-ID"

type ctxKey int

const corrKey ctxKey = 0

// New returns a JSON slog.Logger to stdout tagged with service+version. Level from LOG_LEVEL
// (debug|info|warn|error); default debug — the over-inform standard.
func New(service, version string) *slog.Logger {
	lvl := slog.LevelDebug
	switch strings.ToLower(os.Getenv("LOG_LEVEL")) {
	case "info":
		lvl = slog.LevelInfo
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	}
	h := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl})
	return slog.New(h).With("service", service, "version", version)
}

// Discard drops everything (tests + a nil-safe default).
func Discard() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

// NewID returns a random 128-bit hex correlation id, "cid_"-prefixed.
func NewID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "cid_unknown"
	}
	return "cid_" + hex.EncodeToString(b[:])
}

// WithCorrelation stores id on ctx.
func WithCorrelation(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, corrKey, id)
}

// Correlation reads the ctx correlation id ("" when absent).
func Correlation(ctx context.Context) string {
	if v, ok := ctx.Value(corrKey).(string); ok {
		return v
	}
	return ""
}

// statusWriter captures the response status for the Completed log while preserving streaming
// (the SSE transport does a direct w.(http.Flusher) assertion — Flush must delegate).
type statusWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (s *statusWriter) WriteHeader(code int) {
	if !s.wrote {
		s.status = code
		s.wrote = true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	if !s.wrote {
		s.status = http.StatusOK
		s.wrote = true
	}
	return s.ResponseWriter.Write(b)
}

// Flush delegates so SSE streaming keeps working through the wrapper.
func (s *statusWriter) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the underlying writer (Hijacker/Flusher/etc.).
func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// Middleware assigns/propagates X-Correlation-ID, puts it on the request context, and logs a
// Begin/Completed pair (method, path, status, duration_ms, correlation_id). Probe traffic (the
// health floor + /metrics) logs at DEBUG; application endpoints at INFO.
func Middleware(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cid := r.Header.Get(CorrelationHeader)
		if cid == "" {
			cid = NewID()
		}
		w.Header().Set(CorrelationHeader, cid)
		ctx := WithCorrelation(r.Context(), cid)
		r = r.WithContext(ctx)

		lvl := slog.LevelInfo
		if isProbe(r.URL.Path) {
			lvl = slog.LevelDebug
		}
		// remote_addr + xff (CP-M3-III-5, S8 diagnostic): behind the App Gateway the effective client key for
		// rate-limiting depends on which XFF hop is trusted; logging both lets us set ARGUS_TRUSTED_PROXIES
		// from the actual header the CP receives instead of assuming the AGW's forwarding behaviour.
		lg := logger.With("correlation_id", cid, "method", r.Method, "path", redactPath(r.URL.Path),
			"remote_addr", r.RemoteAddr, "xff", r.Header.Get("X-Forwarded-For"))
		lg.Log(ctx, lvl, "http request Begin")
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		lg.Log(ctx, lvl, "http request Completed", "status", sw.status, "duration_ms", time.Since(start).Milliseconds())
	})
}

// redactPath removes the /public/{slug} SLUG from anything that reaches a log line.
//
// ⛔ THE SLUG IS BEARER-EQUIVALENT. The public view (internal/control/publicview.go) is an
// ALLOWLIST keyed by a long unguessable slug and protected by nothing else — no token, no cookie,
// no session. That is deliberate: the slug IS the capability. So writing it to the application log
// beside the viewer's IP hands that capability to everyone with log read access, which is a wider
// audience than the people the slug was shared with.
//
// ⚠ DEMOTING /public TO DEBUG WOULD NOT FIX THIS. This package is DEBUG-ON BY DEFAULT (see the
// package comment): the operator tunes volume with LOG_LEVEL at the ingestion layer, so a Debug
// line is still emitted and still shipped on a default deployment. The slug has to be removed from
// the record, not merely logged more quietly.
//
// Only the slug segment goes. The route is kept verbatim so the logs still show that /public was
// hit, by whom and how often — which is what an operator needs to spot someone enumerating it.
func redactPath(p string) string {
	if strings.HasPrefix(p, "/public/") && len(p) > len("/public/") {
		return "/public/<redacted>"
	}
	return p
}

func isProbe(p string) bool {
	switch p {
	case "/healthz", "/health/live", "/health", "/metrics":
		return true
	}
	return false
}
