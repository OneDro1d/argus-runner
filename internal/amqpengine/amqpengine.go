// Package amqpengine is Argus's native AMQP client for the Message Flow
// layer: a core it exposes so a native, non-JMeter engine (mirroring
// internal/chain, internal/mcp and internal/ui) can probe a real broker
// directly, instead of only triggering over HTTP
// (templates/message-flow.jmx:185) and verifying through the RabbitMQ
// management API (internal/argus/argus.go:365-386).
//
// AC-D18 builds only the core: connect, probe (publish / publish-confirmed /
// consume / declare), and a msgbus v2 envelope encoder. Wiring this into the
// scenario grammar / DeriveProps is explicitly out of scope here — AC-D16, on
// its own branch, is changing that grammar and would conflict.
//
// Every probe returns a typed Outcome rather than a bare error: OK, the AMQP
// reply code observed (403 access-refused, 406 precondition-failed, 404
// not-found, 0 on success), and a redacted human Text. Each probe method
// opens and closes its OWN channel, so a channel RabbitMQ closes out from
// under one probe (a 403/406 permission refusal) never poisons the next.
package amqpengine

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// Engine is a live connection to a broker. It never stores the URL it was
// dialed with, and never logs it — Connect takes the URL from an environment
// variable NAME, never a literal, precisely so a credentialed connection
// string cannot end up embedded in a scenario or a call site by accident.
type Engine struct {
	conn connection

	// The waits, per Engine so a test can shorten them; newEngine sets the package constants.
	confirmTimeout, closeTimeout, rpcTimeout time.Duration

	// mu guards the broker's connection.blocked state (watchBlocked, guard.go).
	mu          sync.Mutex
	blocked     bool
	blockReason string
}

// Connect dials the broker whose AMQP URL lives in the environment variable
// named envVar (e.g. "ARGUS_TEST_AMQP_URL"). Passing a literal URL here
// defeats the point — envVar must be a variable NAME.
//
// The URL and any credentials it carries are read once, from the
// environment, handed straight to amqp.Dial, and then discarded: Engine
// keeps no copy. On failure the returned error has its userinfo redacted
// (RedactURL) — see redact.go and TestRedactURL / TestConnect_* for the
// proof that a password never survives into an error string.
func Connect(envVar string) (*Engine, error) {
	if envVar == "" {
		return nil, errors.New("amqpengine: Connect requires an environment variable NAME, not a literal URL")
	}
	raw, isSet := os.LookupEnv(envVar)
	if !isSet || raw == "" {
		return nil, fmt.Errorf("amqpengine: environment variable %s is not set", envVar)
	}
	conn, err := amqp.Dial(raw)
	if err != nil {
		return nil, fmt.Errorf("amqpengine: dial: %s", RedactURL(err.Error()))
	}
	return newEngine(realConn{conn}), nil
}

// Close closes the underlying connection, waiting at most closeTimeout for the broker's close-ok;
// past that the connection is torn down and an error says so. Safe to call on a nil *Engine.
func (e *Engine) Close() error {
	if e == nil || e.conn == nil {
		return nil
	}
	err, done := boundedCall(e.closeTimeout, func() error { return e.conn.Close() })
	if !done {
		e.teardown()
		return errors.New("amqpengine: the broker did not answer the connection close; the connection was torn down")
	}
	return err
}

// Outcome is the typed result of one probe operation.
type Outcome struct {
	OK   bool   // true only when the broker accepted the operation outright
	Code int    // the AMQP reply code (403, 406, 404, ...); 0 on success or when no channel-level code was observed
	Text string // redacted human-readable detail; "" on a plain success
}

// ok is the shared "the broker accepted this" Outcome.
func ok() Outcome { return Outcome{OK: true, Code: 0, Text: ""} }

// errOutcome turns any error from an amqp091-go call into an Outcome,
// pulling the AMQP reply code out of a channel/connection-level *amqp.Error
// when there is one, and always redacting the text.
func errOutcome(err error) Outcome {
	if err == nil {
		return ok()
	}
	var aerr *amqp.Error
	if errors.As(err, &aerr) {
		return Outcome{OK: false, Code: aerr.Code, Text: RedactURL(aerr.Reason)}
	}
	return Outcome{OK: false, Code: 0, Text: RedactURL(err.Error())}
}
