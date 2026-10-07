package chain

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/report"
)

// ── review item 1: once a TCP connection was established, EVERY error fails the claim ────────────

func listenAndHandle(t *testing.T, handle func(c net.Conn)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			handle(c)
		}
	}()
	return "http://" + ln.Addr().String() + "/x"
}

// readFirstByte holds the server's close until the client has sent its request, so the client's dial has
// surely completed. Closed at once, a loopback reset can beat the client's own connect check and arrive as
// a reset AT the dial, which is indistinguishable from a rejecting network and rightly passes (2 of 50 runs).
func readFirstByte(c net.Conn) {
	buf := make([]byte, 1)
	_, _ = c.Read(buf)
}

func TestUnreachable_AcceptThenCloseWithoutAnswerFails(t *testing.T) {
	u := listenAndHandle(t, func(c net.Conn) {
		readFirstByte(c)
		_ = c.Close()
	})
	st := unreachableStep(u).Run("cid", map[string]string{})
	if st.Status != "failed" {
		t.Fatalf("the connection was established, so the claim must FAIL; got %+v", st)
	}
}

func TestUnreachable_AcceptThenResetFails(t *testing.T) {
	u := listenAndHandle(t, func(c net.Conn) {
		readFirstByte(c)
		if tc, ok := c.(*net.TCPConn); ok {
			_ = tc.SetLinger(0)
		}
		_ = c.Close()
	})
	st := unreachableStep(u).Run("cid", map[string]string{})
	if st.Status != "failed" {
		t.Fatalf("the connection was established, so a reset after it must FAIL the claim; got %+v", st)
	}
}

// An https target whose server accepts the TCP connection and resets it during the TLS handshake: the
// connection WAS established, but http.Transport's GotConn fires only after the handshake, so a flag set
// there never saw it. Connection establishment is marked at the dial.
func TestUnreachable_ResetDuringTLSHandshakeFails(t *testing.T) {
	u := listenAndHandle(t, func(c net.Conn) {
		readFirstByte(c) // the ClientHello: the reset lands inside the handshake
		if tc, ok := c.(*net.TCPConn); ok {
			_ = tc.SetLinger(0)
		}
		_ = c.Close()
	})
	st := unreachableStep("https"+u[len("http"):]).Run("cid", map[string]string{})
	if st.Status != "failed" {
		t.Fatalf("the TCP connection was established before the TLS handshake was reset, so the claim must FAIL; got %+v", st)
	}
}

// A reset AT the dial (no connection established) is what a rejecting network policy looks like: pass.
func TestUnreachable_ResetAtTheDialPasses(t *testing.T) {
	withUnreachableKnobs(t, 2*time.Second, func(context.Context, string, string) (net.Conn, error) {
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNRESET)}
	})
	st := unreachableStep("http://10.255.255.1:8080/x").Run("cid", map[string]string{})
	if st.Status != "passed" {
		t.Fatalf("a reset before any connection is the expected answer; got %+v", st)
	}
}

func TestClassifyTransportError_ConnectedAlwaysFails(t *testing.T) {
	for name, e := range map[string]error{
		"reset": syscall.ECONNRESET, "eof": io.EOF, "unexpected-eof": io.ErrUnexpectedEOF,
		"refused": syscall.ECONNREFUSED, "other": errors.New("boom"),
	} {
		if pass, _ := classifyTransportError(e, true); pass {
			t.Errorf("%s with connected=true must fail", name)
		}
	}
	for name, e := range map[string]error{"reset": syscall.ECONNRESET, "refused": syscall.ECONNREFUSED} {
		if pass, _ := classifyTransportError(e, false); !pass {
			t.Errorf("%s with connected=false must pass", name)
		}
	}
}

// ── review item 2: only a POSITIVE step opens the guard ──────────────────────────────────────────

func namedUnreachable(t *testing.T, name string) Step {
	s := unreachableStep(closedURL(t))
	s.Name = name
	return s
}

func TestUnreachable_OnlyAPositiveStepOpensTheGuard(t *testing.T) {
	res := Run("cid", []Step{namedUnreachable(t, "A"), namedUnreachable(t, "B")})
	if statusOf(res, "A") != report.StepNotMeasured || statusOf(res, "B") != report.StepNotMeasured {
		t.Fatalf("two unreachable steps and no positive one: both not-measured; got %+v", res.Steps)
	}
	res = Run("cid", []Step{okStep("alive"), namedUnreachable(t, "A"), namedUnreachable(t, "B")})
	if statusOf(res, "A") != "passed" || statusOf(res, "B") != "passed" {
		t.Fatalf("positive passed: both unreachable steps are judged; got %+v", res.Steps)
	}
	res = Run("cid", []Step{failStep("alive"), namedUnreachable(t, "A"), namedUnreachable(t, "B")})
	if statusOf(res, "A") != report.StepNotMeasured || statusOf(res, "B") != report.StepNotMeasured {
		t.Fatalf("positive failed: both not-measured; got %+v", res.Steps)
	}
}
