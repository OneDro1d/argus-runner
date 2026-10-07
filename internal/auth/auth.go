// Package auth is the M2.5 dark-factory enforcement core: it turns an opaque
// bearer token into a hat (role.Role) via a constant-time compare against the
// two configured tokens. The matched token IS the role — there is no signing or
// key service (JWT/OAuth is M4). This single package is called in front of BOTH
// entry points (the CLI in cmd/argus and the MCP server in internal/mcpserver)
// so the authorization boundary is enforced once, not duplicated.
//
//	ARGUS_RUNNER_TOKEN -> role.Product  (product-agent: runner__* scope only)
//	ARGUS_EXECUTOR_SECRET, formerly ARGUS_AUTHOR_TOKEN -> role.Test  (test-agent: runner__* + author__* scope)
//
// It is safety-critical (VR-C8): a wrong answer here hands the holdout (author
// scope) to the product agent. Fail-closed on no/unknown token; refuse to start
// on an ambiguous configuration (DF-DEC-M25-05).
package auth

import (
	"crypto/subtle"
	"errors"
	"fmt"

	"github.com/OneDro1d/argus-runner/internal/role"
)

var (
	// ErrNoToken is returned when no token is presented (fail-closed).
	ErrNoToken = errors.New("auth: a token is required")
	// ErrUnknownToken is returned when the presented token matches neither configured value.
	ErrUnknownToken = errors.New("auth: token not recognized")
	// ErrConfig is returned when the token configuration itself is invalid
	// (a token unset, or the two tokens equal) — the role mapping would be ambiguous.
	ErrConfig = errors.New("auth: token configuration invalid")
)

// Config holds the two opaque bearer tokens (origin: the operator's .env).
type Config struct {
	RunnerToken string // product-agent: runner-scope only
	AuthorToken string // test-agent: runner + author scope
}

// Validate enforces the startup invariant (DF-DEC-M25-05): both tokens must be
// set and distinct. An unset token or a collision makes the role mapping
// ambiguous, so the CLI / MCP server must refuse to start.
func (c Config) Validate() error {
	if c.RunnerToken == "" || c.AuthorToken == "" {
		return fmt.Errorf("%w: both ARGUS_RUNNER_TOKEN and ARGUS_EXECUTOR_SECRET (formerly ARGUS_AUTHOR_TOKEN) must be set", ErrConfig)
	}
	if subtle.ConstantTimeCompare([]byte(c.RunnerToken), []byte(c.AuthorToken)) == 1 {
		return fmt.Errorf("%w: the two tokens must be distinct (collision -> ambiguous role)", ErrConfig)
	}
	return nil
}

// Verify resolves a presented bearer token to a hat. The matched token is the
// role. Comparison is constant-time against both configured values; an empty or
// unknown token is rejected (fail-closed). The author token is checked first so
// the more-privileged scope is never silently granted to a runner-only token.
func (c Config) Verify(presented string) (role.Role, error) {
	if presented == "" {
		return "", ErrNoToken
	}
	isAuthor := subtle.ConstantTimeCompare([]byte(presented), []byte(c.AuthorToken)) == 1
	isRunner := subtle.ConstantTimeCompare([]byte(presented), []byte(c.RunnerToken)) == 1
	switch {
	case isAuthor:
		return role.Test, nil // author-scope = the test hat (runner + author)
	case isRunner:
		return role.Product, nil // runner-scope = the product hat (runner only)
	default:
		return "", ErrUnknownToken
	}
}
