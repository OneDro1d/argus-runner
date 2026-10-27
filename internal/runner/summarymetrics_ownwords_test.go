package runner

// AC-F1.1 (#694): when a summary reading cannot be read, the executor sends only Argus's own sentence, the HTTP status
// number Argus measured and words from fixed lists — never the source's error text, never Go's network error text. The
// source's full text stays in this executor's own log. Every case is read through SummaryTicker.Cycle, so the batch
// the poll would send and the log line are the ones the executor really produces.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/federation"
)

// The markers stand for the source's own words. None of them may reach the wire; each must reach the local log.
const (
	acf11MarkType = "acf11-mark-type"
	acf11MarkText = "acf11-mark-text customer ACME Ltd has no quota left"
	acf11MarkHost = "acf11-mark-host"
	acf11MarkCert = "acf11-mark-cert"
)

// acf11Config writes an argus-config.yaml with one summary reading from a source of the given type.
func acf11Config(t *testing.T, typ, srcURL, credential string) string {
	t.Helper()
	cred := ""
	if credential != "" {
		cred = ", credential: \"" + credential + "\""
	}
	query := "'sum(x)'"
	if typ == "metrics_endpoint" {
		query = "agents_online"
	}
	p := filepath.Join(t.TempDir(), "argus-config.yaml")
	body := "project:\n  name: x\ntargets:\n  http:\n    base_url: http://sut:8080\n" +
		"summary_metrics:\n  every: 1m\n  source: { type: " + typ + ", url: \"" + srcURL + "\"" + cred + " }\n" +
		"  readings:\n    - { name: agents_connected, unit: agents, query: " + query + " }\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// acf11Cycle runs one SummaryTicker cycle over cfg with hc (nil: the ticker's own client) and returns the one reading
// the poll would carry and every line the ticker logged.
func acf11Cycle(t *testing.T, cfg string, hc *http.Client) (federation.SummaryReading, []string) {
	t.Helper()
	var mu sync.Mutex
	var lines []string
	tk := NewSummaryTicker(cfg, func(f string, a ...any) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, fmt.Sprintf(f, a...))
	})
	if hc != nil {
		tk.hc = hc
	}
	tk.Cycle(context.Background(), time.Now())
	b := tk.Take()
	if len(b) != 1 {
		t.Fatalf("the cycle produced %d readings, want 1: %+v", len(b), b)
	}
	mu.Lock()
	defer mu.Unlock()
	return b[0], append([]string(nil), lines...)
}

// acf11Unreadable checks the rules every unreadable reading keeps: no number (never 0), a reason, at most
// federation.SummaryErrorMax characters, exactly the sentence want, none of banned on the wire, and one log line about
// the reading that holds every one of mustLog.
func acf11Unreadable(t *testing.T, r federation.SummaryReading, lines []string, want string, banned, mustLog []string) {
	t.Helper()
	if r.Value != nil {
		t.Errorf("an unreadable reading was sent as the number %v", *r.Value)
	}
	if r.Error != want {
		t.Errorf("sent error = %q\nwant         %q", r.Error, want)
	}
	if n := len([]rune(r.Error)); n == 0 || n > federation.SummaryErrorMax {
		t.Errorf("sent error has %d characters; it must be 1..%d", n, federation.SummaryErrorMax)
	}
	for _, b := range banned {
		if strings.Contains(r.Error, b) {
			t.Errorf("sent error %q carries the source's words %q", r.Error, b)
		}
	}
	var line string
	for _, l := range lines {
		if strings.Contains(l, "agents_connected unreadable") {
			line = l
		}
	}
	if line == "" {
		t.Fatalf("no local log line for the unreadable reading; lines: %q", lines)
	}
	for _, m := range mustLog {
		if !strings.Contains(line, m) {
			t.Errorf("the local log line lacks %q: %q", m, line)
		}
	}
	if strings.ContainsAny(line, "\n\r") {
		t.Errorf("the local log line holds a line break: %q", line)
	}
}

func acf11ErrorBodyServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// Way A: the source answers with an error; its answer's error type and error text are its own words.
func TestSummaryReadingError_SourceErrorAnswerSendsOnlyTheStatus(t *testing.T) {
	body := `{"status":"error","errorType":"` + acf11MarkType + `","error":"` + acf11MarkText + `"}`
	for _, typ := range []string{"prometheus", "metrics_endpoint"} {
		t.Run(typ, func(t *testing.T) {
			srv := acf11ErrorBodyServer(t, 500, body)
			url := srv.URL
			if typ == "metrics_endpoint" {
				url += "/metrics"
			}
			r, lines := acf11Cycle(t, acf11Config(t, typ, url, ""), nil)
			acf11Unreadable(t, r, lines, "the source answered HTTP 500; its own message is in the executor's log",
				[]string{acf11MarkType, "acf11-mark", "ACME", "quota", ":"}, []string{acf11MarkType, acf11MarkText, "HTTP 500"})
		})
	}
}

// Way A with a Prometheus error type from the list: the word is sent, no part of the error text.
func TestSummaryReadingError_SourceErrorAnswerKeepsAListedPrometheusErrorType(t *testing.T) {
	for _, c := range []struct {
		status int
		typ    string
	}{{500, "bad_data"}, {400, "bad_data"}, {422, "execution"}, {503, "timeout"}, {500, "unavailable"}, {499, "canceled"},
		{500, "internal"}, {404, "not_found"}, {406, "not_acceptable"}} { // Prometheus's own codes (web/api/v1/api.go getDefaultErrorCode)
		srv := acf11ErrorBodyServer(t, c.status, `{"status":"error","errorType":"`+c.typ+`","error":"`+acf11MarkText+`"}`)
		r, lines := acf11Cycle(t, acf11Config(t, "prometheus", srv.URL, ""), nil)
		want := "the source answered HTTP " + strconv.Itoa(c.status) + " (" + c.typ + "); its own message is in the executor's log"
		acf11Unreadable(t, r, lines, want, []string{"acf11-mark", "ACME", "quota"}, []string{acf11MarkText})
	}
}

// A word that only looks like one on the list is dropped whole: the list is exact.
func TestSummaryReadingError_AnErrorTypeOffTheListIsDropped(t *testing.T) {
	for _, typ := range []string{"Bad_Data", "bad_data ", " bad_data", "bad_data\nx", "bad_data: " + acf11MarkText, "execution2", "warning"} {
		srv := acf11ErrorBodyServer(t, 400, `{"errorType":`+jsonString(typ)+`,"error":""}`)
		r, lines := acf11Cycle(t, acf11Config(t, "prometheus", srv.URL, ""), nil)
		acf11Unreadable(t, r, lines, "the source answered HTTP 400; its own message is in the executor's log",
			[]string{"Bad_Data", "bad_data", "execution2", "warning", "ACME"}, nil)
	}
}

// Way A with nothing of its own in the answer: the status alone, and no claim that the log holds a message.
func TestSummaryReadingError_AnErrorAnswerWithoutAMessageSendsTheStatusAlone(t *testing.T) {
	for _, body := range []string{`upstream down`, ``, `{}`, `{"error":""}`, `<html>` + acf11MarkText + `</html>`} {
		srv := acf11ErrorBodyServer(t, 503, body)
		r, lines := acf11Cycle(t, acf11Config(t, "metrics_endpoint", srv.URL+"/metrics", ""), nil)
		acf11Unreadable(t, r, lines, "the source answered HTTP 503", []string{"upstream", "ACME", "html"}, []string{"HTTP 503"})
	}
}

// Way B: Prometheus says "error" inside a normal (200) answer.
func TestSummaryReadingError_PrometheusErrorInsideA200AnswerSendsOnlyTheListedWord(t *testing.T) {
	srv := acf11ErrorBodyServer(t, 200, `{"status":"error","errorType":"execution","error":"`+acf11MarkText+`"}`)
	r, lines := acf11Cycle(t, acf11Config(t, "prometheus", srv.URL, ""), nil)
	acf11Unreadable(t, r, lines, "the source reported an error (execution); its own message is in the executor's log",
		[]string{"acf11-mark", "ACME", "quota"}, []string{acf11MarkText, "execution"})

	srv2 := acf11ErrorBodyServer(t, 200, `{"status":"error","errorType":"`+acf11MarkType+`","error":"`+acf11MarkText+`"}`)
	r, lines = acf11Cycle(t, acf11Config(t, "prometheus", srv2.URL, ""), nil)
	acf11Unreadable(t, r, lines, "the source reported an error; its own message is in the executor's log",
		[]string{"acf11-mark", "ACME"}, []string{acf11MarkType, acf11MarkText})

	srv3 := acf11ErrorBodyServer(t, 200, `{"status":"`+acf11MarkType+`"}`)
	r, lines = acf11Cycle(t, acf11Config(t, "prometheus", srv3.URL, ""), nil)
	acf11Unreadable(t, r, lines, "the source reported an error", []string{"acf11-mark"}, nil)
}

// Way C: a result type is quoted only when it is one of Prometheus's four.
func TestSummaryReadingError_AResultTypeIsQuotedOnlyFromTheList(t *testing.T) {
	for _, c := range []struct{ rt, want string }{
		{"matrix", `the query returned a "matrix" result; it must return one number`},
		{"string", `the query returned a "string" result; it must return one number`},
		{acf11MarkType, "the query returned a result type Argus does not know; it must return one number"},
		{"", "the query returned a result type Argus does not know; it must return one number"},
		{"Matrix", "the query returned a result type Argus does not know; it must return one number"},
	} {
		srv := acf11ErrorBodyServer(t, 200, `{"status":"success","data":{"resultType":`+jsonString(c.rt)+`,"result":[]}}`)
		r, lines := acf11Cycle(t, acf11Config(t, "prometheus", srv.URL, ""), nil)
		var mustLog []string
		if c.rt == acf11MarkType {
			mustLog = []string{acf11MarkType}
		}
		acf11Unreadable(t, r, lines, c.want, []string{"acf11-mark", "Matrix"}, mustLog)
	}
}

// Way D: a source whose host name does not resolve. The DNS server is a local socket that answers every question with
// NXDOMAIN, so Go's own resolver produces its real error and no lookup leaves the machine.
func TestSummaryReadingError_AnUnknownHostSendsOnlyTheWord(t *testing.T) {
	hc := &http.Client{Transport: &http.Transport{DialContext: (&net.Dialer{Resolver: nxdomainResolver(t)}).DialContext}}
	cfg := acf11Config(t, "prometheus", "http://"+acf11MarkHost+".invalid:9090", "")
	r, lines := acf11Cycle(t, cfg, hc)
	acf11Unreadable(t, r, lines, "could not reach the source (unknown_host); the details are in the executor's log",
		[]string{"acf11-mark", ".invalid", "lookup", "no such host"}, []string{acf11MarkHost + ".invalid", "no such host"})
}

// Way D: a refused connection.
func TestSummaryReadingError_ARefusedConnectionSendsOnlyTheWord(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	url := dead.URL
	dead.Close()
	r, lines := acf11Cycle(t, acf11Config(t, "metrics_endpoint", url+"/metrics", ""), nil)
	acf11Unreadable(t, r, lines, "could not reach the source (refused); the details are in the executor's log",
		[]string{"127.0.0.1", "dial", "connect", "refused it"}, []string{"dial tcp 127.0.0.1:"}) // Go's text differs by OS
}

// Way D: a TLS failure. The certificate names only a host inside the environment (the marker); the client trusts it but
// asks for another name, so Go's error names the certificate's host. Also: a certificate nobody trusts.
func TestSummaryReadingError_ATLSFailureSendsOnlyTheWord(t *testing.T) {
	cert, pool := acf11Cert(t, acf11MarkCert+".example")
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(vec("1"))) }))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "prometheus.example"}}}
	r, lines := acf11Cycle(t, acf11Config(t, "prometheus", srv.URL, ""), hc)
	acf11Unreadable(t, r, lines, "could not reach the source (tls); the details are in the executor's log",
		[]string{"acf11-mark", "x509", "certificate", "prometheus.example"}, []string{acf11MarkCert + ".example", "x509"})

	untrusted := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(untrusted.Close)
	r, lines = acf11Cycle(t, acf11Config(t, "prometheus", untrusted.URL, ""), nil)
	acf11Unreadable(t, r, lines, "could not reach the source (tls); the details are in the executor's log",
		[]string{"x509", "certificate", "authority"}, []string{"x509"})
}

// Way D, TLS, the failures with no certificate in them: https:// to a plain-HTTP source (Go reports a scheme mismatch),
// bytes that are neither TLS nor HTTP (a record header error), and a source that requires a client certificate (an
// alert: over TCP Go has no exported type for it).
func TestSummaryReadingError_TLSFailuresWithoutACertificateSendOnlyTheWord(t *testing.T) {
	const want = "could not reach the source (tls); the details are in the executor's log"
	plain := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(plain.Close)
	r, lines := acf11Cycle(t, acf11Config(t, "prometheus", "https://"+strings.TrimPrefix(plain.URL, "http://"), ""), nil)
	acf11Unreadable(t, r, lines, want, []string{"HTTPS", "http:", "server gave"}, []string{"server gave HTTP response to HTTPS client"})

	// the garbage is written, then the server waits for the client to close: a close with the client's hello still
	// unread would reset the connection (Windows does so at once) and the client would never read the garbage
	garbage := acf11RawServer(t, func(c net.Conn) {
		_, _ = c.Write([]byte("garbage-" + acf11MarkText + "\n"))
		_, _ = io.Copy(io.Discard, c)
	})
	r, lines = acf11Cycle(t, acf11Config(t, "prometheus", "https://"+garbage, ""), nil)
	acf11Unreadable(t, r, lines, want, []string{"garba", "record", "first record"}, []string{"does not look like a TLS handshake"})

	needsClientCert := httptest.NewUnstartedServer(http.NotFoundHandler())
	needsClientCert.TLS = &tls.Config{ClientAuth: tls.RequireAnyClientCert}
	needsClientCert.StartTLS()
	t.Cleanup(needsClientCert.Close)
	hc := needsClientCert.Client() // trusts the server's certificate, has none of its own
	r, lines = acf11Cycle(t, acf11Config(t, "prometheus", needsClientCert.URL, ""), hc)
	acf11Unreadable(t, r, lines, want, []string{"remote error", "alert", "certificate"}, []string{"remote error: tls:"})
}

// Way D, TLS on macOS: the system verifier there returns a plain fmt.Errorf("x509: %s") with no x509 type
// (crypto/x509/root_darwin.go), which the TLS client wraps in *tls.CertificateVerificationError (handshake_client.go).
// A Linux or Windows run cannot produce it, so the real wrapper type is built here around such a text.
func TestSummaryReadingError_ACertificateErrorWithNoX509TypeIsTLS(t *testing.T) {
	err := &url.Error{Op: "Get", URL: "https://" + acf11MarkHost, Err: &tls.CertificateVerificationError{
		Err: fmt.Errorf("x509: %s", "“"+acf11MarkCert+"” certificate is not trusted")}}
	words, local := summaryErrorTexts(transportError("could not reach the source", err, readSignals{}), nil)
	if words != "could not reach the source (tls); the details are in the executor's log" {
		t.Errorf("sent %q, want the tls sentence", words)
	}
	if !strings.Contains(local, acf11MarkCert) || strings.Contains(words, acf11MarkCert) {
		t.Errorf("the certificate's name must reach the log only: sent %q, logged %q", words, local)
	}
}

// Way D, an answer that is not valid HTTP: Go quotes what it read, so the marker reaches Go's text — and only the log.
func TestSummaryReadingError_AnAnswerThatIsNotHTTPSendsOnlyTheWord(t *testing.T) {
	notHTTP := acf11RawServer(t, func(c net.Conn) {
		acf11ReadRequest(c)
		_, _ = c.Write([]byte("HELLO " + acf11MarkType + "\r\n\r\n"))
	})
	r, lines := acf11Cycle(t, acf11Config(t, "prometheus", "http://"+notHTTP, ""), nil)
	acf11Unreadable(t, r, lines, "could not reach the source (protocol); the details are in the executor's log",
		[]string{"acf11-mark", "malformed", "HELLO"}, []string{acf11MarkType})

	badHeader := acf11RawServer(t, func(c net.Conn) {
		acf11ReadRequest(c)
		_, _ = c.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: " + acf11MarkType + "\r\n\r\n"))
	})
	r, lines = acf11Cycle(t, acf11Config(t, "metrics_endpoint", "http://"+badHeader+"/metrics", ""), nil)
	acf11Unreadable(t, r, lines, "could not reach the source (protocol); the details are in the executor's log",
		[]string{"acf11-mark", "Content-Length"}, []string{acf11MarkType})

	noAnswer := acf11RawServer(t, func(c net.Conn) { acf11ReadRequest(c) })
	r, lines = acf11Cycle(t, acf11Config(t, "prometheus", "http://"+noAnswer, ""), nil)
	acf11Unreadable(t, r, lines, "could not reach the source (protocol); the details are in the executor's log",
		[]string{"EOF"}, []string{"EOF"})

	cutBody := acf11RawServer(t, func(c net.Conn) {
		acf11ReadRequest(c)
		_, _ = c.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\n{\"status\":"))
	})
	r, lines = acf11Cycle(t, acf11Config(t, "prometheus", "http://"+cutBody, ""), nil)
	acf11Unreadable(t, r, lines, "could not read the source's answer (protocol); the details are in the executor's log",
		[]string{"EOF", "unexpected"}, []string{"unexpected EOF"})

	// a malformed chunked body: Go's error has no exported type; it is known by happening while the body was read
	badChunk := acf11RawServer(t, func(c net.Conn) {
		acf11ReadRequest(c)
		_, _ = c.Write([]byte("HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\nzz\r\n"))
	})
	r, lines = acf11Cycle(t, acf11Config(t, "prometheus", "http://"+badChunk, ""), nil)
	acf11Unreadable(t, r, lines, "could not read the source's answer (protocol); the details are in the executor's log",
		[]string{"chunk", "invalid"}, []string{"chunk"})
}

// Both log-line guards, each on its own: the source's text is quoted, and the whole line has no control character.
func TestSummaryReadingError_EachLogGuardKeepsTheLineWhole(t *testing.T) {
	if q := quoteForLog("a\nb\rc\x00d"); strings.ContainsAny(q, "\n\r\x00") {
		t.Errorf("quoteForLog left a control character: %q", q)
	}
	if s := summaryLogText("a\nb\rc\x00d\x7fe", nil); strings.ContainsAny(s, "\n\r\x00\x7f") || s != "a b c d e" {
		t.Errorf("summaryLogText = %q, want %q", s, "a b c d e")
	}
	if s := summaryLogText("user svc:pw-long and pw-long", []string{"svc:pw-long"}); strings.Contains(s, "pw-long") || strings.Contains(s, "svc") {
		t.Errorf("summaryLogText kept the credential: %q", s)
	}
}

// Way D, a reset connection (the source drops the connection with a TCP RST).
func TestSummaryReadingError_AResetConnectionSendsOnlyTheWord(t *testing.T) {
	reset := acf11RawServer(t, func(c net.Conn) {
		acf11ReadRequest(c)
		if tc, ok := c.(*net.TCPConn); ok {
			_ = tc.SetLinger(0)
		}
	})
	r, lines := acf11Cycle(t, acf11Config(t, "prometheus", "http://"+reset, ""), nil)
	acf11Unreadable(t, r, lines, "could not reach the source (reset); the details are in the executor's log",
		[]string{"127.0.0.1", "connection reset", "forcibly", "read"}, []string{"read tcp 127.0.0.1:"}) // Go's text differs by OS
}

// A timeout keeps Argus's existing sentence; the detail goes to the log.
func TestSummaryReadingError_ATimeoutKeepsItsSentence(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	old := summaryReadTimeout
	summaryReadTimeout = 200 * time.Millisecond
	t.Cleanup(func() { summaryReadTimeout = old })
	r, lines := acf11Cycle(t, acf11Config(t, "prometheus", srv.URL, ""), nil)
	acf11Unreadable(t, r, lines, "timed out reading the source", []string{"127.0.0.1", "deadline", "context"},
		[]string{"deadline exceeded"})
}

// The local log keeps the source's text but never the configured credential (the one thing still removed there).
func TestSummaryReadingError_TheLogLineHoldsTheSourceTextButNotTheCredential(t *testing.T) {
	t.Setenv("ACF11_SOURCE_CRED", "svc:acf11-topsecret")
	srv := acf11ErrorBodyServer(t, 401, `{"error":"bad password acf11-topsecret for svc:acf11-topsecret `+acf11MarkText+`"}`)
	r, lines := acf11Cycle(t, acf11Config(t, "metrics_endpoint", srv.URL+"/metrics", "${ACF11_SOURCE_CRED}"), nil)
	acf11Unreadable(t, r, lines, "the source answered HTTP 401; its own message is in the executor's log",
		[]string{"topsecret", "svc", "ACME"}, []string{acf11MarkText, "bad password"})
	for _, l := range lines {
		if strings.Contains(l, "acf11-topsecret") {
			t.Errorf("the local log line carries the configured credential: %q", l)
		}
	}
}

// A credential whose parts are words of Argus's own sentences ("metrics", "refused", "data") does not change the sent
// text: replacing them there would tell the control plane the part, and break a word of the fixed lists. The log line
// still removes them.
func TestSummaryReadingError_ACredentialPartIsNeverReplacedInTheSentWords(t *testing.T) {
	t.Setenv("ACF11_ORACLE_CRED", "metrics:refused")
	notMetrics := metricsServer(t, "hello world\n")
	r, lines := acf11Cycle(t, acf11Config(t, "metrics_endpoint", notMetrics.URL+"/metrics", "${ACF11_ORACLE_CRED}"), nil)
	acf11Unreadable(t, r, lines, "the source did not answer with metrics text", []string{"<redacted>"}, nil)

	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	r, lines = acf11Cycle(t, acf11Config(t, "metrics_endpoint", deadURL+"/metrics", "${ACF11_ORACLE_CRED}"), nil)
	acf11Unreadable(t, r, lines, "could not reach the source (refused); the details are in the executor's log",
		[]string{"<redacted>"}, []string{"<redacted>"})

	t.Setenv("ACF11_ORACLE_CRED", "data:pw-acf11")
	badData := acf11ErrorBodyServer(t, 400, `{"status":"error","errorType":"bad_data","error":"parse error"}`)
	r, lines = acf11Cycle(t, acf11Config(t, "prometheus", badData.URL, "${ACF11_ORACLE_CRED}"), nil)
	acf11Unreadable(t, r, lines, "the source answered HTTP 400 (bad_data); its own message is in the executor's log",
		[]string{"<redacted>"}, []string{"parse error"})
}

// What really leaves: the poll request the control plane receives carries only the sentence, and no byte of the
// source's text (the hub transport sends the same PollRequest, client.go).
func TestSummaryReadingError_ThePollRequestCarriesOnlyTheSentence(t *testing.T) {
	var mu sync.Mutex
	var raw []byte
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		raw = append([]byte(nil), b...)
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(federation.PollResponse{})
	}))
	t.Cleanup(cp.Close)
	src := acf11ErrorBodyServer(t, 500, `{"status":"error","errorType":"`+acf11MarkType+`","error":"`+acf11MarkText+`"}`)
	priv, err := LoadOrCreateKey(filepath.Join(t.TempDir(), "id.key"))
	if err != nil {
		t.Fatal(err)
	}
	c := NewClient(cp.URL, "inst-acf11", priv)
	c.Summary = NewSummaryTicker(acf11Config(t, "prometheus", src.URL, ""), func(string, ...any) {})
	c.Summary.Cycle(context.Background(), time.Now())
	if _, err := c.Poll(context.Background(), "1.0.0", nil, nil, nil, nil, nil, federation.SUTObservation{}, nil, nil); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	var req federation.PollRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatalf("the poll body is not a PollRequest: %v", err)
	}
	if len(req.SummaryReadings) != 1 || req.SummaryReadings[0].Value != nil ||
		req.SummaryReadings[0].Error != "the source answered HTTP 500; its own message is in the executor's log" {
		t.Errorf("summary_readings on the wire = %+v", req.SummaryReadings)
	}
	for _, m := range []string{"acf11-mark", "ACME", "quota"} {
		if strings.Contains(string(raw), m) {
			t.Errorf("the poll request carries the source's words %q", m)
		}
	}
}

// The log keeps the source's text whole — a long stack trace too, with its root cause at the end. Only a text over
// summaryLogMax (the bound against a flood: Go quotes a malformed header line, up to 10 MiB) is cut, in its middle,
// so its head and its tail stay.
func TestSummaryReadingError_TheLogKeepsTheWholeTextAndBoundsAFlood(t *testing.T) {
	trace := "java.lang.IllegalStateException: pool " + strings.Repeat("at com.example.Exporter.scrape(Exporter.java:123) ", 100) +
		"Caused by: " + acf11MarkType + " database pool exhausted"
	srv := acf11ErrorBodyServer(t, 500, `{"error":"`+trace+`"}`)
	r, lines := acf11Cycle(t, acf11Config(t, "metrics_endpoint", srv.URL+"/metrics", ""), nil)
	acf11Unreadable(t, r, lines, "the source answered HTTP 500; its own message is in the executor's log",
		[]string{"acf11-mark", "Exporter"}, []string{trace})

	flood := acf11MarkText + strings.Repeat("x", 100000) + acf11MarkType
	srv2 := acf11ErrorBodyServer(t, 500, `{"error":"`+flood+`"}`)
	r, lines = acf11Cycle(t, acf11Config(t, "prometheus", srv2.URL, ""), nil)
	acf11Unreadable(t, r, lines, "the source answered HTTP 500; its own message is in the executor's log",
		[]string{"acf11-mark", "xxx"}, []string{acf11MarkText, acf11MarkType, " characters cut) … "})
	for _, l := range lines {
		if n := len([]rune(l)); n > summaryLogMax+200 {
			t.Errorf("the log line has %d characters; the bound is %d", n, summaryLogMax)
		}
	}
}

// A line break in the source's text cannot forge a second log line.
func TestSummaryReadingError_TheSourceTextCannotBreakTheLogLine(t *testing.T) {
	srv := acf11ErrorBodyServer(t, 500, `{"error":"first\nsummary_metrics: forged line\r\n`+acf11MarkText+`"}`)
	r, lines := acf11Cycle(t, acf11Config(t, "prometheus", srv.URL, ""), nil)
	acf11Unreadable(t, r, lines, "the source answered HTTP 500; its own message is in the executor's log",
		[]string{"forged", "ACME"}, []string{"forged line", acf11MarkText})
	if len(lines) != 1 {
		t.Errorf("the cycle logged %d lines, want 1: %q", len(lines), lines)
	}
}

// An error that no code in summarymetrics.go vouched for is sent as Argus's fixed sentence (fail-closed, AC-F1's rule);
// its text is only logged.
func TestSummaryReadingError_AnUnvouchedErrorSendsTheFixedSentence(t *testing.T) {
	words, local := summaryErrorTexts(errors.New(acf11MarkText), nil)
	if words != "the reading failed; its details are in the executor's log" {
		t.Errorf("an unvouched error was sent as %q, want the fixed sentence", words)
	}
	if !strings.Contains(local, acf11MarkText) {
		t.Errorf("the local text lost the error: %q", local)
	}
	words, _ = summaryErrorTexts(fmt.Errorf("wrapped: %w", ownWordsf("the query returned no data")), nil)
	if words != "the query returned no data" {
		t.Errorf("a vouched error under a plain wrapper was sent as %q, want its own words only", words)
	}
}

// In summarymetrics.go, the file that builds the readings, a reading's Error is set in ONE place: from
// summaryErrorTexts. Read from the source, so a second writer added to this file fails here. (It reads this file only;
// what really leaves is pinned by TestSummaryReadingError_ThePollRequestCarriesOnlyTheSentence.)
func TestSummaryReadingError_TheReadingsErrorIsSetOnlyThroughTheRule(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "summarymetrics.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	writes := 0
	ast.Inspect(f, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for _, l := range as.Lhs {
			sel, ok := l.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Error" {
				continue
			}
			writes++
			viaRule := false
			if call, isCall := as.Rhs[0].(*ast.CallExpr); isCall && len(as.Rhs) == 1 {
				id, isID := call.Fun.(*ast.Ident)
				viaRule = isID && id.Name == "summaryErrorTexts"
			}
			if !viaRule {
				t.Errorf("%s: a reading's Error is set without summaryErrorTexts", fset.Position(as.Pos()))
			}
		}
		return true
	})
	if writes != 1 {
		t.Errorf("summarymetrics.go sets .Error %d times, want exactly 1 (through summaryErrorTexts)", writes)
	}
}

// Each typed TLS check gives tls on its own, with no httptrace signal: a client whose transport does its own TLS dial
// (DialTLSContext) never reports TLSHandshakeDone, and then the error's type is all there is. The values are Go's real
// types, in the forms Go returns them (RecordHeaderError and the x509 errors by value).
func TestSummaryReadingError_EachTLSErrorTypeIsTLSWithoutTheTraceSignal(t *testing.T) {
	cert, _ := acf11Cert(t, acf11MarkCert+".example") // HostnameError.Error reads its certificate
	for name, err := range map[string]error{
		"scheme mismatch":     http.ErrSchemeMismatch,
		"record header":       tls.RecordHeaderError{Msg: "tls: first record does not look like a TLS handshake"},
		"verification":        &tls.CertificateVerificationError{Err: errors.New("x509: " + acf11MarkCert)},
		"alert (QUIC)":        tls.AlertError(42),
		"unknown authority":   x509.UnknownAuthorityError{},
		"host name":           x509.HostnameError{Certificate: cert.Leaf, Host: acf11MarkHost},
		"invalid certificate": x509.CertificateInvalidError{Reason: x509.Expired},
		"remote alert":        &net.OpError{Op: "remote error", Err: errors.New("tls: bad certificate")},
		"local alert":         &net.OpError{Op: "local error", Err: errors.New("tls: protocol version not supported")},
		"alert behind proxy":  &net.OpError{Op: "proxyconnect", Err: &net.OpError{Op: "remote error", Err: errors.New("tls: x")}},
	} {
		if got := networkErrorWord(fmt.Errorf("Get: %w", err), readSignals{}); got != "tls" {
			t.Errorf("%s: word %q, want tls", name, got)
		}
	}
}

// Way D, TLS failures Go reports as plain errors ("tls: …" with no type): a certificate that does not parse, and a TLS
// front end that closes during the handshake (an EOF). httptrace's TLSHandshakeDone names them.
func TestSummaryReadingError_ATLSHandshakeFailureWithNoTypeIsTLS(t *testing.T) {
	const want = "could not reach the source (tls); the details are in the executor's log"
	_, pool := acf11Cert(t, "prometheus.example")
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	// a plain TLS listener: httptest's StartTLS would refuse the certificate before any client saw it
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	badCert := &http.Server{Handler: http.NotFoundHandler()}
	go func() {
		_ = badCert.Serve(tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{
			{Certificate: [][]byte{[]byte("not a certificate " + acf11MarkCert)}, PrivateKey: key}}}))
	}()
	t.Cleanup(func() { _ = badCert.Close() })
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
	r, lines := acf11Cycle(t, acf11Config(t, "prometheus", "https://"+ln.Addr().String(), ""), hc)
	acf11Unreadable(t, r, lines, want, []string{"parse", "x509", "certificate"}, []string{"failed to parse certificate"})

	// reads the client's whole first TLS record (5-byte header + its length), then closes cleanly: an EOF in the handshake
	closesInHandshake := acf11RawServer(t, func(c net.Conn) {
		head := make([]byte, 5)
		if _, err := io.ReadFull(c, head); err != nil {
			return
		}
		_, _ = io.CopyN(io.Discard, c, int64(head[3])<<8|int64(head[4]))
	})
	r, lines = acf11Cycle(t, acf11Config(t, "prometheus", "https://"+closesInHandshake, ""), nil)
	acf11Unreadable(t, r, lines, want, []string{"EOF"}, []string{"EOF"})
}

// Way D, redirects: the word is that of the request that failed, not of an earlier hop that answered; a redirect the
// client refuses to follow is "other". Both reach the "other" word.
func TestSummaryReadingError_ARedirectDoesNotCarryAnEarlierHopsSignals(t *testing.T) {
	const other = "could not reach the source (other); the details are in the executor's log"
	toFTP := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "ftp://"+acf11MarkHost+".example/metrics", http.StatusFound)
	}))
	t.Cleanup(toFTP.Close)
	r, lines := acf11Cycle(t, acf11Config(t, "metrics_endpoint", toFTP.URL+"/metrics", ""), nil)
	acf11Unreadable(t, r, lines, other, []string{"acf11-mark", "ftp", "scheme"}, []string{"unsupported protocol scheme"})

	var loop *httptest.Server
	loop = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, loop.URL+"/again", http.StatusFound)
	}))
	t.Cleanup(loop.Close)
	r, lines = acf11Cycle(t, acf11Config(t, "metrics_endpoint", loop.URL+"/metrics", ""), nil)
	acf11Unreadable(t, r, lines, other, []string{"redirects", "127.0.0.1"}, []string{"stopped after 10 redirects"})

	// a redirect to a source that refuses: the refusal's own word
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	toDead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, deadURL+"/metrics", http.StatusFound)
	}))
	t.Cleanup(toDead.Close)
	r, lines = acf11Cycle(t, acf11Config(t, "metrics_endpoint", toDead.URL+"/metrics", ""), nil)
	acf11Unreadable(t, r, lines, "could not reach the source (refused); the details are in the executor's log",
		[]string{"127.0.0.1"}, []string{"dial tcp 127.0.0.1:"})
}

// A cancelled read keeps Argus's sentence; the detail goes to the log.
func TestSummaryReadingError_ACancelledReadKeepsItsSentence(t *testing.T) {
	srv := acf11ErrorBodyServer(t, 200, vec("1"))
	tk := NewSummaryTicker(acf11Config(t, "prometheus", srv.URL, ""), nil)
	var lines []string
	tk.logf = func(f string, a ...any) { lines = append(lines, fmt.Sprintf(f, a...)) }
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tk.Cycle(ctx, time.Now())
	b := tk.Take()
	if len(b) != 1 {
		t.Fatalf("batch = %+v", b)
	}
	acf11Unreadable(t, b[0], lines, "read cancelled", []string{"context", "127.0.0.1"}, []string{"context canceled"})
}

// Every socket error of a refused or a reset connection has its word (real error types, built as net/http builds them
// for a write that fails: *net.OpError around *os.SyscallError around the errno).
func TestSummaryReadingError_EverySocketErrnoHasItsWord(t *testing.T) {
	sock := func(e syscall.Errno) error {
		return &net.OpError{Op: "write", Net: "tcp", Err: os.NewSyscallError("write", e)}
	}
	for _, c := range []struct {
		errno syscall.Errno
		want  string
	}{{syscall.ECONNREFUSED, "refused"}, {syscall.ECONNRESET, "reset"}, {syscall.EPIPE, "reset"}, {syscall.ECONNABORTED, "reset"}} {
		if got := networkErrorWord(sock(c.errno), readSignals{answered: true}); got != c.want {
			t.Errorf("%v: word %q, want %q", c.errno, got, c.want)
		}
	}
	if got := networkErrorWord(sock(syscall.ENOENT), readSignals{}); got != "other" {
		t.Errorf("an errno of no list: word %q, want other", got)
	}
}

// The log line never holds the configured credential: not inside the quoted text of the source (a password with " or
// \ is escaped there), and not in the Basic form this executor sent, when the source echoes its Authorization header.
func TestSummaryReadingError_TheLogLineHoldsNoFormOfTheCredential(t *testing.T) {
	const pw = `top"sec\ret-acf11`
	t.Setenv("ACF11_QUOTED_CRED", "svc:"+pw)
	basic := base64.StdEncoding.EncodeToString([]byte("svc:" + pw))
	body, _ := json.Marshal(map[string]string{"error": "bad password " + pw + " in header Basic " + basic + " " + acf11MarkText})
	srv := acf11ErrorBodyServer(t, 401, string(body))
	r, lines := acf11Cycle(t, acf11Config(t, "metrics_endpoint", srv.URL+"/metrics", "${ACF11_QUOTED_CRED}"), nil)
	acf11Unreadable(t, r, lines, "the source answered HTTP 401; its own message is in the executor's log",
		[]string{"sec", "ACME"}, []string{acf11MarkText, "bad password"})
	for _, l := range lines {
		for _, form := range []string{pw, strconv.Quote(pw)[1 : len(strconv.Quote(pw))-1], basic, "ret-acf11"} {
			if strings.Contains(l, form) {
				t.Errorf("the log line holds the credential as %q: %q", form, l)
			}
		}
	}

	// a credential with no ':' is sent as the user name with an empty password: Basic base64("<value>:")
	t.Setenv("ACF11_TOKEN_CRED", "acf11-token-only")
	tokenBasic := base64.StdEncoding.EncodeToString([]byte("acf11-token-only:"))
	echo := acf11ErrorBodyServer(t, 401, `{"error":"rejected Basic `+tokenBasic+` `+acf11MarkText+`"}`)
	r, lines = acf11Cycle(t, acf11Config(t, "metrics_endpoint", echo.URL+"/metrics", "${ACF11_TOKEN_CRED}"), nil)
	acf11Unreadable(t, r, lines, "the source answered HTTP 401; its own message is in the executor's log",
		[]string{"rejected", "ACME"}, []string{acf11MarkText})
	for _, l := range lines {
		if strings.Contains(l, tokenBasic) {
			t.Errorf("the log line holds the credential in its Basic form %q: %q", tokenBasic, l)
		}
	}
}

// The sentences Argus sends when a 200 answer cannot be read as one number: each exactly, never the answer's own text.
func TestSummaryReadingError_EachSentenceForAnUnreadable200AnswerIsExact(t *testing.T) {
	for _, c := range []struct {
		typ, body, want string
	}{
		{"prometheus", vec(), "the query returned no data"},
		{"prometheus", vec("1", "2"), "the query returned 2 series; it must reduce to ONE number (wrap it in sum())"},
		{"prometheus", vec("NaN"), "the sample value is not finite"},
		{"prometheus", vec(acf11MarkType), "the sample value is not a number"},
		{"prometheus", `<html>` + acf11MarkText + `</html>`, "the source did not answer with a Prometheus query result"},
		{"prometheus", ``, "the source did not answer with a Prometheus query result"},
		{"prometheus", `{"status":"success","data":{"resultType":"vector","result":"` + acf11MarkText + `"}}`, "malformed vector result"},
		{"prometheus", `{"status":"success","data":{"resultType":"scalar","result":"` + acf11MarkText + `"}}`, "malformed scalar result"},
		{"prometheus", `{"status":"success","data":{"resultType":"scalar","result":[1,7]}}`, "malformed sample value"},
		{"metrics_endpoint", "other_series 1\n", "the series was not found in the source"},
		{"metrics_endpoint", "<html>" + acf11MarkText + "</html>\n", "the source did not answer with metrics text"},
		{"metrics_endpoint", "agents_online NaN\n", "a matching sample is not finite"},
	} {
		srv := acf11ErrorBodyServer(t, 200, c.body)
		url := srv.URL
		if c.typ == "metrics_endpoint" {
			url += "/metrics"
		}
		r, lines := acf11Cycle(t, acf11Config(t, c.typ, url, ""), nil)
		acf11Unreadable(t, r, lines, c.want, []string{"acf11-mark", "ACME", "html"}, nil)
	}
}

// ---- helpers ------------------------------------------------------------------------------------------------------

// acf11RawServer serves every TCP connection with handle (the connection is closed after it) and returns host:port.
func acf11RawServer(t *testing.T, handle func(net.Conn)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_ = c.SetDeadline(time.Now().Add(5 * time.Second))
				handle(c)
			}()
		}
	}()
	return ln.Addr().String()
}

// acf11ReadRequest reads one request head from c (up to the empty line).
func acf11ReadRequest(c net.Conn) {
	buf := make([]byte, 0, 4096)
	one := make([]byte, 1)
	for !strings.HasSuffix(string(buf), "\r\n\r\n") && len(buf) < 64<<10 {
		if _, err := c.Read(one); err != nil {
			return
		}
		buf = append(buf, one[0])
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// nxdomainResolver is a Go resolver whose only DNS server is a local UDP socket that answers every question with
// NXDOMAIN (RCODE 3): Go's resolver then returns its real *net.DNSError, and nothing leaves the machine.
func nxdomainResolver(t *testing.T) *net.Resolver {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	go func() {
		buf := make([]byte, 1500)
		for {
			n, addr, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			q := buf[:n]
			if n < 12 {
				continue
			}
			i := 12 // the question: QNAME's labels up to the zero byte, then QTYPE and QCLASS
			for i < n && q[i] != 0 {
				i += int(q[i]) + 1
			}
			end := i + 5
			if end > n {
				continue
			}
			resp := make([]byte, end)
			copy(resp, q[:end])
			resp[2] = 0x80 | (q[2] & 0x79) // QR, the query's opcode and RD
			resp[3] = 0x80 | 3             // RA, RCODE 3: no such name
			for k := 6; k < 12; k++ {      // no answer, authority or additional records
				resp[k] = 0
			}
			_, _ = pc.WriteTo(resp, addr)
		}
	}()
	addr := pc.LocalAddr().String()
	return &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "udp", addr)
	}}
}

// acf11Cert makes a self-signed certificate for host only, and a pool that trusts it.
func acf11Cert(t *testing.T, host string) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: host}, DNSNames: []string{host},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true, IsCA: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, pool
}
