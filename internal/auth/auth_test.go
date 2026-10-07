package auth

import (
	"errors"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/role"
)

const (
	runnerTok = "runner-tok-AAAA1111-zzzz"
	authorTok = "author-tok-BBBB2222-yyyy"
)

func cfg() Config { return Config{RunnerToken: runnerTok, AuthorToken: authorTok} }

// VR-I2: the runner (product-agent) token resolves to the product hat — runner scope ONLY.
func TestVerify_RunnerTokenIsProductHat(t *testing.T) {
	r, err := cfg().Verify(runnerTok)
	if err != nil {
		t.Fatalf("runner token rejected: %v", err)
	}
	if r != role.Product {
		t.Fatalf("runner token = %q, want product (runner-scope only)", r)
	}
	if r.CanAccessScenarios() {
		t.Errorf("product hat must NOT have scenario (author) scope")
	}
}

// VR-I2/I5: the author (test-agent) token resolves to the test hat — runner + author scope.
func TestVerify_AuthorTokenIsTestHat(t *testing.T) {
	r, err := cfg().Verify(authorTok)
	if err != nil {
		t.Fatalf("author token rejected: %v", err)
	}
	if r != role.Test {
		t.Fatalf("author token = %q, want test (runner+author scope)", r)
	}
	if !r.CanAccessScenarios() {
		t.Errorf("test hat must have scenario (author) scope")
	}
}

// VR-I1 / DF-DEC-M25-05: no/empty token is rejected fail-closed.
func TestVerify_EmptyTokenRejected(t *testing.T) {
	if _, err := cfg().Verify(""); !errors.Is(err, ErrNoToken) {
		t.Fatalf("empty token: err = %v, want ErrNoToken (fail-closed)", err)
	}
}

// VR-I1: an unknown/garbage token is rejected fail-closed.
func TestVerify_UnknownTokenRejected(t *testing.T) {
	if _, err := cfg().Verify("not-a-real-token"); !errors.Is(err, ErrUnknownToken) {
		t.Fatalf("garbage token: err = %v, want ErrUnknownToken (fail-closed)", err)
	}
}

// SECURITY (VR-C8 / the holdout): the product (runner) token must NEVER resolve to the
// test hat — that would hand author scope (the dark-factory holdout) to the product agent.
func TestVerify_RunnerTokenNeverGetsAuthorScope(t *testing.T) {
	r, _ := cfg().Verify(runnerTok)
	if r == role.Test || r.CanAccessScenarios() {
		t.Fatalf("SECURITY: runner token resolved to author scope (%q) — holdout leak", r)
	}
}

// DF-DEC-M25-05: both tokens must be set, else the role mapping is ambiguous — refuse.
func TestConfig_Validate_RejectsUnset(t *testing.T) {
	for _, c := range []Config{{}, {RunnerToken: runnerTok}, {AuthorToken: authorTok}} {
		if err := c.Validate(); !errors.Is(err, ErrConfig) {
			t.Errorf("Validate(%+v) = %v, want ErrConfig (both tokens required)", c, err)
		}
	}
}

// DF-DEC-M25-05: identical tokens collide (ambiguous role) — refuse to start.
func TestConfig_Validate_RejectsCollision(t *testing.T) {
	if err := (Config{RunnerToken: "same", AuthorToken: "same"}).Validate(); !errors.Is(err, ErrConfig) {
		t.Fatalf("identical tokens accepted; want ErrConfig (ambiguous role)")
	}
}

func TestConfig_Validate_AcceptsDistinct(t *testing.T) {
	if err := cfg().Validate(); err != nil {
		t.Fatalf("distinct tokens rejected: %v", err)
	}
}
