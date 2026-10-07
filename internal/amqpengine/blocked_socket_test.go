package amqpengine

import (
	"encoding/binary"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// blockedBroker is a loopback stand-in for a RabbitMQ with a memory alarm: it completes the AMQP
// handshake, sends connection.blocked, and then reads and answers NOTHING. gone is closed when the
// client's side of the socket ends, which is how a test sees that the connection was torn down.
type blockedBroker struct {
	addr string
	gone chan struct{}
}

func amqpFrame(typ byte, channelID uint16, payload []byte) []byte {
	b := make([]byte, 7, 8+len(payload))
	b[0] = typ
	binary.BigEndian.PutUint16(b[1:], channelID)
	binary.BigEndian.PutUint32(b[3:], uint32(len(payload)))
	b = append(b, payload...)
	return append(b, 0xCE)
}

func amqpMethod(class, method uint16, args []byte) []byte {
	p := make([]byte, 4, 4+len(args))
	binary.BigEndian.PutUint16(p, class)
	binary.BigEndian.PutUint16(p[2:], method)
	return amqpFrame(1, 0, append(p, args...))
}

func longstr(s string) []byte {
	b := make([]byte, 4, 4+len(s))
	binary.BigEndian.PutUint32(b, uint32(len(s)))
	return append(b, s...)
}

func readFrame(c net.Conn) error {
	hdr := make([]byte, 7)
	if _, err := io.ReadFull(c, hdr); err != nil {
		return err
	}
	_, err := io.CopyN(io.Discard, c, int64(binary.BigEndian.Uint32(hdr[3:]))+1)
	return err
}

func startBlockedBroker(t *testing.T, reason string) *blockedBroker {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("loopback listen: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	b := &blockedBroker{addr: l.Addr().String(), gone: make(chan struct{})}
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		if _, err := io.CopyN(io.Discard, c, 8); err != nil { // protocol header
			return
		}
		start := append([]byte{0, 9}, longstr("")...) // version 0-9, empty server-properties table
		start = append(start, longstr("PLAIN")...)
		start = append(start, longstr("en_US")...)
		_, _ = c.Write(amqpMethod(10, 10, start))
		if readFrame(c) != nil { // start-ok
			return
		}
		tune := make([]byte, 8)
		binary.BigEndian.PutUint16(tune, 16)            // channel-max
		binary.BigEndian.PutUint32(tune[2:], 4096)      // frame-max
		_, _ = c.Write(amqpMethod(10, 30, tune))        // heartbeat 0
		if readFrame(c) != nil || readFrame(c) != nil { // tune-ok, open
			return
		}
		_, _ = c.Write(amqpMethod(10, 41, []byte{0}))
		// The alarm is raised once the client starts working (a block announced before the client
		// has subscribed is the dial-time gap amqp091-go gives no hook for; see).
		if readFrame(c) != nil {
			return
		}
		_, _ = c.Write(amqpMethod(10, 60, append([]byte{byte(len(reason))}, reason...))) // connection.blocked
		_, _ = io.Copy(io.Discard, c)                                                    // never answers
		close(b.gone)
	}()
	return b
}

func blockedEngine(t *testing.T, reason string) (*Engine, *blockedBroker) {
	t.Helper()
	b := startBlockedBroker(t, reason)
	conn, err := amqp.Dial("amqp://guest:guest@" + b.addr + "/")
	if err != nil {
		t.Fatalf("dial the loopback broker: %v", err)
	}
	e := newEngine(realConn{conn})
	e.closeTimeout = 150 * time.Millisecond
	e.rpcTimeout = 300 * time.Millisecond
	e.confirmTimeout = 100 * time.Millisecond
	return e, b
}

func waitGone(t *testing.T, b *blockedBroker) {
	t.Helper()
	select {
	case <-b.gone:
	case <-time.After(2 * time.Second):
		t.Fatal("the client never ended its connection: the abandoned call is still waiting on the socket")
	}
}

func TestRealSocket_OperationOnABlockedBrokerReturnsNamesTheBlockAndTearsDown(t *testing.T) {
	e, b := blockedEngine(t, "low on memory")
	out := within(t, returnBound, "PublishConfirmed", func() Outcome {
		return e.PublishConfirmed("x", "rk", []byte("b"), Props{})
	})
	if out.OK {
		t.Fatalf("outcome OK against a broker that answers nothing: %+v", out)
	}
	if !strings.Contains(out.Text, "the broker blocked this connection (reason: low on memory)") {
		t.Fatalf("Text = %q, want the block named with the broker's reason", out.Text)
	}
	waitGone(t, b)
}

func TestRealSocket_EngineCloseOnABlockedBrokerReturnsAndTearsDown(t *testing.T) {
	e, b := blockedEngine(t, "disk free space low")
	err := within(t, returnBound, "Engine.Close", e.Close)
	if err == nil || !strings.Contains(err.Error(), "torn down") {
		t.Fatalf("Close error = %v, want one saying the connection was torn down", err)
	}
	waitGone(t, b)
}
