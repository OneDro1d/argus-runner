package runner

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/textproto"
	"os"
	"syscall"

	"github.com/OneDro1d/argus-runner/internal/report"
)

// summarymetrics_neterr.go — AC-F1.1 way D: the ONE word a summary reading's network failure is sent with. The words are
// AC-F2.2's seven (report.LoadErrorKinds), never redefined here. The word is chosen from the error's TYPES (errors.Is /
// errors.As) and from where the failure happened, never from its text: Go's text names hosts, addresses and
// certificates of the environment, and it is never sent.
//
// Checked against Go 1.26.5's real errors on Linux and on Windows (34 cases through a real http.Client, 32 of them
// failures, measured 2026-10-17), and against its source:
//   - crypto/tls returns RecordHeaderError by VALUE (conn.go:580), so the pointer form never matches;
//   - net/http turns a RecordHeaderError that starts with "HTTP/" into http.ErrSchemeMismatch (client.go:270-276);
//   - over TCP a TLS alert is a *net.OpError whose Op is "remote error" or "local error" (conn.go:733, :740, :861);
//     tls.AlertError is used for QUIC only (conn.go:1578-1598);
//   - every certificate failure, the macOS verifier's plain-text one too, is a *tls.CertificateVerificationError
//     (handshake_client.go:1161);
//   - on Windows a refused or reset connection is a WSA number (summarymetrics_errno_windows.go);
//   - most answers that are not valid HTTP have no exported type (request.go:96 badStringError, chunked.go:280-294):
//     they are known by WHERE they happened — once the source had started to answer — and by holding no socket error;
//   - most TLS handshake failures are plain errors too (78 "tls: …" sites in the client handshake, e.g. an old TLS
//     version, a certificate that does not parse, an EOF in the handshake): httptrace's TLSHandshakeDone reports them.
// The signals come from the read's LAST request (readTrace): a redirect does not carry an earlier hop's into the next.

// networkErrorWord returns the word for a client error that is not a timeout or a cancellation (transportError keeps its
// own sentences for those). s is what the read's last request saw (readSignals).
func networkErrorWord(err error, s readSignals) string {
	if err == nil || nilInTree(err) {
		return report.LoadErrorKindOther
	}
	var dnsErr *net.DNSError
	switch {
	case errors.As(err, &dnsErr):
		return report.LoadErrorKindUnknownHost
	case errnoIn(err, connRefusedErrnos):
		return report.LoadErrorKindRefused
	case errnoIn(err, connResetErrnos):
		return report.LoadErrorKindReset
	case s.redirectRefused: // the client's own redirect rule, not the network and not the answer's HTTP
		return report.LoadErrorKindOther
	case tlsFailure(err), s.tlsFailed:
		return report.LoadErrorKindTLS
	case protocolFailure(err, s.answered):
		return report.LoadErrorKindProtocol
	}
	return report.LoadErrorKindOther
}

func errnoIn(err error, errnos []syscall.Errno) bool {
	for _, e := range errnos {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}

// tlsFailure: the TLS handshake or a TLS record failed — a certificate, a protocol version, an alert, or a source that
// does not speak TLS where https was declared.
func tlsFailure(err error) bool {
	var (
		header    tls.RecordHeaderError
		verify    *tls.CertificateVerificationError
		alert     tls.AlertError
		authority x509.UnknownAuthorityError
		host      x509.HostnameError
		invalid   x509.CertificateInvalidError
	)
	switch {
	case errors.Is(err, http.ErrSchemeMismatch), errors.As(err, &header), errors.As(err, &verify), errors.As(err, &alert),
		errors.As(err, &authority), errors.As(err, &host), errors.As(err, &invalid):
		return true
	}
	return tlsAlertOp(err)
}

// tlsAlertOp reports whether err's tree holds a *net.OpError whose Op is crypto/tls's "remote error" (an alert the
// source sent) or "local error" (one this client sent). The whole tree is walked: errors.As stops at the FIRST
// *net.OpError, and a proxy adds an outer one ("proxyconnect").
func tlsAlertOp(err error) bool {
	if err == nil || nilErr(err) {
		return false
	}
	if op, ok := err.(*net.OpError); ok && (op.Op == "remote error" || op.Op == "local error") {
		return true
	}
	switch u := err.(type) {
	case interface{ Unwrap() []error }:
		for _, c := range u.Unwrap() {
			if tlsAlertOp(c) {
				return true
			}
		}
	case interface{ Unwrap() error }:
		return tlsAlertOp(u.Unwrap())
	}
	return false
}

// protocolFailure: the source's answer is not valid HTTP — an answer cut short, closed with no answer, a malformed
// header — or, once the source had started to answer, any failure that is not a socket error.
func protocolFailure(err error, answerStarted bool) bool {
	var textErr textproto.ProtocolError
	switch {
	case errors.As(err, &textErr), errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
		return true
	}
	return answerStarted && !socketError(err)
}

// socketError: err's tree holds an error of the operating system's socket layer.
func socketError(err error) bool {
	var (
		op    *net.OpError
		sc    *os.SyscallError
		errno syscall.Errno
	)
	return errors.As(err, &op) || errors.As(err, &sc) || errors.As(err, &errno)
}
