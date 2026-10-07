package amqpengine

import (
	"errors"
	"strings"
	"testing"

	amqp "github.com/rabbitmq/amqp091-go"
)

func TestRedactURL(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "amqp URL with user and password",
			in:   "dial amqp://produser:s3cr3tPass@broker.example.com:5672/: connection refused",
			want: "dial amqp://REDACTED@broker.example.com:5672/: connection refused",
		},
		{
			name: "amqps scheme",
			in:   "amqps://argus-test:hunter2@msgbus-broker.msgbus.svc.cluster.local:5671/",
			want: "amqps://REDACTED@msgbus-broker.msgbus.svc.cluster.local:5671/",
		},
		{
			name: "userinfo with no password",
			in:   "amqp://justauser@host:5672/",
			want: "amqp://REDACTED@host:5672/",
		},
		{
			name: "two URLs in one string",
			in:   "tried amqp://a:pw1@host1/ then amqp://b:pw2@host2/",
			want: "tried amqp://REDACTED@host1/ then amqp://REDACTED@host2/",
		},
		{
			name: "no URL at all",
			in:   "PRECONDITION_FAILED - user_id property set to 'spoofed' but authenticated user was 'argus-test'",
			want: "PRECONDITION_FAILED - user_id property set to 'spoofed' but authenticated user was 'argus-test'",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RedactURL(tc.in)
			if got != tc.want {
				t.Fatalf("RedactURL(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if strings.Contains(got, "s3cr3t") || strings.Contains(got, "hunter2") || strings.Contains(got, "pw1") || strings.Contains(got, "pw2") {
				t.Fatalf("RedactURL(%q) leaked a credential: %q", tc.in, got)
			}
		})
	}
}

func TestConnect_RequiresEnvVarName(t *testing.T) {
	if _, err := Connect(""); err == nil {
		t.Fatal("Connect(\"\") should refuse a literal/empty env var name")
	}
}

func TestConnect_EnvVarNotSet(t *testing.T) {
	const name = "ARGUS_AMQPENGINE_TEST_UNSET_VAR"
	t.Setenv(name, "")
	if _, err := Connect(name); err == nil {
		t.Fatalf("Connect(%s) with no value set should error", name)
	}
}

// TestConnect_DialFailureIsRedacted proves the redaction promise end-to-end
// through Connect: a dial to a closed local port fails fast, and the
// resulting error — built from the URL amqp091-go was given — must not
// contain the password from that URL.
func TestConnect_DialFailureIsRedacted(t *testing.T) {
	const name = "ARGUS_AMQPENGINE_TEST_BAD_URL"
	const password = "th3-secret-nobody-should-see"
	t.Setenv(name, "amqp://produser:"+password+"@127.0.0.1:1/") // port 1: nothing listens

	_, err := Connect(name)
	if err == nil {
		t.Fatal("Connect against an unreachable port should fail")
	}
	if strings.Contains(err.Error(), password) {
		t.Fatalf("Connect error leaked the password: %v", err)
	}
	if strings.Contains(err.Error(), "produser:"+password) {
		t.Fatalf("Connect error leaked userinfo verbatim: %v", err)
	}
}

func TestErrOutcome_NilIsSuccess(t *testing.T) {
	o := errOutcome(nil)
	if !o.OK || o.Code != 0 || o.Text != "" {
		t.Fatalf("errOutcome(nil) = %+v, want a plain success", o)
	}
}

func TestErrOutcome_WrapsAMQPErrorCode(t *testing.T) {
	aerr := &amqp.Error{Code: 403, Reason: "ACCESS_REFUSED - amqp://argus-test:pw@host/ denied", Server: true}
	wrapped := errors.New("channel closed: " + aerr.Error())
	_ = wrapped // errors.As needs the *amqp.Error in the chain, not just its text
	o := errOutcome(&wrappedAMQPError{err: aerr})
	if o.OK {
		t.Fatal("a refusal must not report OK")
	}
	if o.Code != 403 {
		t.Fatalf("Code = %d, want 403", o.Code)
	}
	if strings.Contains(o.Text, "argus-test:pw") {
		t.Fatalf("Outcome.Text leaked userinfo: %q", o.Text)
	}
}

func TestErrOutcome_PlainErrorHasZeroCode(t *testing.T) {
	o := errOutcome(errors.New("some non-AMQP plumbing failure"))
	if o.OK {
		t.Fatal("an error must not report OK")
	}
	if o.Code != 0 {
		t.Fatalf("Code = %d, want 0 for a non-*amqp.Error", o.Code)
	}
}

// wrappedAMQPError lets TestErrOutcome_WrapsAMQPErrorCode exercise the
// errors.As path errOutcome uses, the same way a real amqp091-go call
// wraps its *amqp.Error inside a higher-level error.
type wrappedAMQPError struct{ err *amqp.Error }

func (w *wrappedAMQPError) Error() string { return "wrapped: " + w.err.Error() }
func (w *wrappedAMQPError) Unwrap() error { return w.err }
