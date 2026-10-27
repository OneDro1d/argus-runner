// Package dataaccess holds the ONE spelling of an author credential's data-access type (AC-F3): Full or Limited.
//
// A Full credential may use every Argus feature, including the few that send part of the app's answers to the Argus
// cloud (the full report, one recorded comparison result, Tolerance numbers). A Limited credential may not: the app's
// answers stay in the user's environment. The control plane decides it per caller, on every call
// (control/cloudauth.go sets mcpserver.Principal.DataAccess; control/dataaccess.go holds the Full-only list).
//
// The values are a wire and schema contract: author_tokens.data_access_type and the three OAuth tables (migration 083),
// the signed token's "dat" claim, POST /api/tokens and the website all carry these exact strings.
package dataaccess

import "fmt"

const (
	// Full is the default: every feature, as before AC-F3. Every credential created before AC-F3 reads as Full.
	Full = "full"
	// Limited keeps the app's answers in the user's environment: the Full-only features are refused.
	Limited = "limited"
)

// Valid reports whether s is exactly one of the two values.
func Valid(s string) bool { return s == Full || s == Limited }

// IsFull is the fail-closed reading every enforcement point uses: only the exact value Full is Full. A type that was
// never set ("") or is anything else reads as not Full, so code that forgets to set the type can never serve app data.
func IsFull(s string) bool { return s == Full }

// Parse reads a requested type (POST /api/tokens, the OAuth allow page). An absent value is the default, Full (the
// spec: "For an author token it defaults to full"); any value that is not one of the two is refused, never guessed.
func Parse(s string) (string, error) {
	if s == "" {
		return Full, nil
	}
	if !Valid(s) {
		return "", fmt.Errorf("unknown data_access_type %q (want %q or %q)", s, Full, Limited)
	}
	return s, nil
}
