package doctor

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"
)

// TokenKind is what a control-plane credential IS, read off its shape. The control plane accepts two
// kinds (internal/control/cloudauth.go): author PATs, and its own OAuth access tokens. Telling them
// apart locally is the whole point — each fails differently, and each failure used to be reported as a
// bare 401 or `denied` at the moment the credential was needed.
type TokenKind string

const (
	// KindNone: no credential.
	KindNone TokenKind = "none"
	// KindAuthorPAT: an `odts_…` personal access token — long-lived, author hat, minted by
	// `argus cloud-mint-token`. It is what a human or an agent presents to the author__* tools.
	KindAuthorPAT TokenKind = "author-pat"
	// KindJWT: a JSON Web Token — the control plane's own RS256 access token (15-minute life, scope
	// `author` or `runner`), or another issuer's. Its claims can be READ locally; only the control
	// plane can VERIFY them.
	KindJWT TokenKind = "jwt"
	// KindUnrecognised: neither of the above. Not known to be bad — not known at all.
	KindUnrecognised TokenKind = "unrecognised"
)

// authorPATPrefix mirrors store.AuthorTokenPrefix (internal/control/store/tokens.go), kept local so
// this package does not import the store; TestAuthorPATPrefixMirrorsTheStore pins the mirror.
const authorPATPrefix = "odts_"

// TokenFacts is everything doctor is allowed to say about a credential. The value is not a field.
type TokenFacts struct {
	Kind TokenKind
	// Scope is the JWT `scope` claim ("author" | "runner" on a control-plane token); empty otherwise.
	Scope string
	// Issuer is the JWT `iss` claim; empty otherwise.
	Issuer string
	// ExpiresAt is the JWT `exp` claim; HasExpiry says whether one was present.
	ExpiresAt time.Time
	HasExpiry bool
	// Malformed explains why a token that looks like a JWT could not be read. When set, Scope and
	// ExpiresAt are meaningless — and so is any conclusion about them.
	Malformed string
}

// Classify reads a credential's kind and, for a JWT, its claims. It does NOT verify a signature — it
// has no key to verify against — and every consumer must say so when it reports the claims.
//
// ⛔ The token goes in; only facts come out. Do not add a field that carries any part of the value
// back, including a prefix, a hash or a length: a transcript is stored.
func Classify(token string) TokenFacts {
	if token == "" {
		return TokenFacts{Kind: KindNone}
	}
	if strings.HasPrefix(token, authorPATPrefix) {
		return TokenFacts{Kind: KindAuthorPAT}
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return TokenFacts{Kind: KindUnrecognised}
	}
	hdr, err := b64url(parts[0])
	if err != nil {
		return TokenFacts{Kind: KindUnrecognised}
	}
	var h struct {
		Alg string `json:"alg"`
	}
	if json.Unmarshal(hdr, &h) != nil || h.Alg == "" {
		// Three dot-separated segments whose first is not a JOSE header: not a JWT.
		return TokenFacts{Kind: KindUnrecognised}
	}
	f := TokenFacts{Kind: KindJWT}
	body, err := b64url(parts[1])
	if err != nil {
		f.Malformed = "the claims segment is not base64url"
		return f
	}
	var c struct {
		Iss   string `json:"iss"`
		Scope string `json:"scope"`
		Exp   int64  `json:"exp"`
	}
	if json.Unmarshal(body, &c) != nil {
		f.Malformed = "the claims segment is not a JSON object"
		return f
	}
	f.Issuer, f.Scope = c.Iss, c.Scope
	if c.Exp > 0 {
		f.HasExpiry = true
		f.ExpiresAt = time.Unix(c.Exp, 0).UTC()
	}
	return f
}

// b64url decodes a JWT segment. RFC 7515 says unpadded base64url; some libraries pad anyway, and a
// token that decodes only after trimming the padding is still that token.
func b64url(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
}
