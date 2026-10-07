package main

// doctor_pointer_test.go — the refusals `argus doctor` diagnoses must NAME it, and `argus` must say what it is.
//
// Measured on dev f2be4df (2026-09-30): outside doctor's own files, `git grep "argus doctor"` found code
// comments and one onboarding page — no message a refused command prints. The bare `argus` listing
// showed "doctor" among 60 names with no description. A diagnosis nobody is pointed at fixes nothing.

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/onboard"
)

func TestUsage_SaysWhatDoctorIs(t *testing.T) {
	doc := captureEmit(t, func() { usage() })
	d, _ := doc["doctor"].(string)
	for _, want := range []string{"doctor [--control-plane <url>]", "read-only", "the fix for each"} {
		if !strings.Contains(d, want) {
			t.Errorf("usage has no doctor line containing %q; got %q", want, d)
		}
	}
}

// The three refusals of the local (M2.5) hat gate. doctor's local-hats check diagnoses each one
// (internal/doctor/hats.go CheckLocalHats), so each must carry the pointer — and keep its old "error"
// text byte for byte, because scripts and tests grep it.
func TestLocalHatGate_RefusalsPointAtDoctor(t *testing.T) {
	for _, tc := range []struct {
		name                           string
		runner, author, presented, cmd string
		wantErr                        string
		noPointer                      bool
	}{
		{"no hat map at all", "", "", "", "list-scenarios", "auth not configured: ", false},
		{"map set, nothing presented", "hat-r", "hat-a", "", "list-scenarios", "denied: ", false},
		{"map set, a token matching neither hat", "hat-r", "hat-a", "hat-x", "list-scenarios", "denied: ", false},
		{"the runner hat on an author command", "hat-r", "hat-a", "hat-r", "list-scenarios", "not permitted for the product scope", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearAllTokenEnv(t)
			t.Setenv("ARGUS_RUNNER_TOKEN", tc.runner)
			t.Setenv("ARGUS_EXECUTOR_SECRET", tc.author)
			t.Setenv("ARGUS_TOKEN", tc.presented)
			var rc int
			doc := captureEmit(t, func() { rc = dispatch([]string{tc.cmd}) })
			if rc != exitDenied {
				t.Fatalf("rc = %d, want %d (exitDenied); output %v", rc, exitDenied, doc)
			}
			if e, _ := doc["error"].(string); !strings.HasPrefix(e, tc.wantErr) {
				t.Fatalf("error = %q, want it to start %q (the old text, unchanged)", e, tc.wantErr)
			}
			// Review finding B: the runner hat on an author command is a hat map that is FINE - doctor's
			// local-hats check would report ok, so a pointer there contradicts doctor_pointer.go's own rule.
			if _, has := doc["diagnose"]; tc.noPointer {
				if has {
					t.Fatalf("diagnose = %v on a refusal doctor reports ok for; want none", doc["diagnose"])
				}
				return
			}
			if d, _ := doc["diagnose"].(string); d != doctorPointerLocalHats {
				t.Fatalf("diagnose = %q, want the local-hats pointer %q", d, doctorPointerLocalHats)
			}
		})
	}
}

// emitErr is where every cloud-* command reports a failure (44 call sites), so the control-plane
// refusal is recognised THERE, from the error's type — never its text.
func TestEmitErr_ControlPlaneRefusalPointsAtDoctor(t *testing.T) {
	wrap := func(status int) error {
		return fmt.Errorf("list workspaces: %w", &onboard.HTTPStatusError{Status: status, Msg: fmt.Sprintf("status %d", status)})
	}
	for _, tc := range []struct {
		name string
		args []any
		want bool
	}{
		{"401: the control plane does not accept the credential", []any{wrap(http.StatusUnauthorized)}, true},
		{"403: it signs in, but not with the scope this needs", []any{wrap(http.StatusForbidden)}, true},
		// A static token (--token / ARGUS_CP_AUTHOR_TOKEN) answered 401 surfaces as ErrNoRefresh, with the
		// 401 NOT in the chain (onboard.Session.refreshOnce). That is the wrong-PAT case the 2026-09-29
		// live test (R4) found, so it is the one this pointer most needs to reach.
		{"a refused static token, reported as no-refresh", []any{fmt.Errorf("x: %w", onboard.ErrNoRefresh)}, true},
		{"the error after other args", []any{"inst-1", wrap(http.StatusForbidden)}, true},
		// Controls: none of these is a verdict on the credential, and doctor would not explain them.
		{"500: an outage", []any{wrap(http.StatusInternalServerError)}, false},
		{"400: a bad request", []any{wrap(http.StatusBadRequest)}, false},
		{"404", []any{wrap(http.StatusNotFound)}, false},
		{"a transport error", []any{errors.New("dial tcp: connection refused")}, false},
		{"the refusal's TEXT alone", []any{"HTTP 401: invalid credential"}, false},
		{"no args", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setCPForDiagnosis(t, "https://cp.example")
			var rc int
			format := "cloud-x:" + strings.Repeat(" %v", len(tc.args))
			doc := captureEmit(t, func() { rc = emitErr(exitErr, format, tc.args...) })
			if rc != exitErr {
				t.Fatalf("rc = %d, want %d: the pointer must not change the exit code", rc, exitErr)
			}
			d, has := doc["diagnose"]
			if has != tc.want {
				t.Fatalf("diagnose present = %v, want %v (doc %v)", has, tc.want, doc)
			}
			if want := "argus doctor --control-plane https://cp.example "; tc.want && !strings.HasPrefix(fmt.Sprint(d), want) {
				t.Fatalf("diagnose = %q, want it to start %q: a command that runs as printed", d, want)
			}
		})
	}
}

// End to end through a real cloud-* command against a stub control plane: the wiring, not only the helper.
func TestCloudCommand_RefusedCredentialPointsAtDoctor(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, `{"error":"invalid credential"}`, status)
			}))
			defer srv.Close()
			clearAllTokenEnv(t)
			setCPForDiagnosis(t, "")      // the command must record its own control plane
			t.Setenv("HOME", t.TempDir()) // no session file can answer instead
			t.Setenv("ARGUS_CP_AUTHOR_TOKEN", "odts_synthetic_not_a_real_token")
			var rc int
			doc := captureEmit(t, func() {
				rc = dispatch([]string{"cloud-executor-status", "--control-plane", srv.URL, "--instance-id", "inst-1"})
			})
			if rc != exitErr {
				t.Fatalf("rc = %d, want %d; doc %v", rc, exitErr, doc)
			}
			if d, _ := doc["diagnose"].(string); !strings.HasPrefix(d, "argus doctor --control-plane "+srv.URL+" ") {
				t.Fatalf("diagnose = %q, want it to name the control plane this command used (%s); doc %v", d, srv.URL, doc)
			}
			if strings.Contains(fmt.Sprint(doc), "odts_synthetic") {
				t.Fatalf("the credential value reached the output: %v", doc)
			}
		})
	}
}

// No control plane recorded (a refusal reached emitErr outside sessionForCommand): the pointer still
// names doctor, with the flag's placeholder rather than a URL it would have to guess.
func TestDoctorPointerCP_WithoutARecordedControlPlane(t *testing.T) {
	setCPForDiagnosis(t, "")
	if got, want := doctorPointerCP(), "argus doctor --control-plane <control-plane-url> "; !strings.HasPrefix(got, want) {
		t.Fatalf("doctorPointerCP() = %q, want it to start %q", got, want)
	}
}

// setCPForDiagnosis pins the package-level record for one test and restores it: other tests in this
// package run cloud-* commands, and each of them records its control plane.
func setCPForDiagnosis(t *testing.T, v string) {
	t.Helper()
	old := cpForDiagnosis
	cpForDiagnosis = v
	t.Cleanup(func() { cpForDiagnosis = old })
}

// The `diagnose` pointer and the `error` text of one refusal render the control plane through the SAME function
// (onboard.ControlPlaneForCommand), so a schemeless value is the placeholder in both, never echoed in one.
func TestDoctorPointerCP_SchemelessValueIsThePlaceholder(t *testing.T) {
	setCPForDiagnosis(t, "//host.example/base")
	got := doctorPointerCP()
	if want := "argus doctor --control-plane <control-plane-url> — "; !strings.HasPrefix(got, want) {
		t.Fatalf("pointer = %q, want prefix %q", got, want)
	}
	if strings.Contains(got, "host.example") {
		t.Fatalf("pointer echoes a value that is not scheme://host: %q", got)
	}
}

// Review finding A: the pointer is built from the raw --control-plane value, so userinfo and a query
// string (which can carry a token) were echoed into stdout JSON. FAKE credentials only.
func TestDoctorPointerCP_NeverEchoesCredentialsAndRunsAsPrinted(t *testing.T) {
	for _, tc := range []struct {
		name, cp, wantArg string
	}{
		{"userinfo", "https://fakeuser:fakepw-123@cp.example", "https://cp.example"},
		{"query", "https://cp.example?token=fake-tok-456", "https://cp.example"},
		{"fragment", "https://cp.example/#fake-frag-789", "https://cp.example/"},
		{"all three", "https://fakeuser:fakepw-123@cp.example:8443/base?token=fake-tok-456#fake-frag-789", "https://cp.example:8443/base"},
		{"shell metacharacters in the path", "https://cp.example/a b;$(touch x)", "'https://cp.example/a%20b;$%28touch%20x%29'"},
		{"single quote in the path", "https://cp.example/it's", "'https://cp.example/it'\\''s'"},
		{"unparseable", "http://fakeuser:fakepw-123@[::1", "<control-plane-url>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setCPForDiagnosis(t, tc.cp)
			got := doctorPointerCP()
			for _, secret := range []string{"fakeuser", "fakepw", "fake-tok", "fake-frag"} {
				if strings.Contains(got, secret) {
					t.Fatalf("pointer echoes %q: %q", secret, got)
				}
			}
			if want := "argus doctor --control-plane " + tc.wantArg + " — "; !strings.HasPrefix(got, want) {
				t.Fatalf("pointer = %q, want prefix %q", got, want)
			}
		})
	}
}
