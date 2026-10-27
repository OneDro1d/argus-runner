package chain

import (
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// PER-CALL Ed25519 SIGNING OF AN mcp STEP (the grammar is scenario.MCPSign).
//
// The key is read ONCE, at preflight, from the executor's environment by the NAME the check gives (a name declared
// under check_env), and lives only in a SignPlan. Nothing here formats a key, a seed or any part of the value into
// a string: every error is a fixed sentence plus the reference NAME. The signature is not secret and may be shown.

// SignPlan is a resolved `sign` block: the spec and the private key it names. The key is never printed.
type SignPlan struct {
	Ref  string // the check_env NAME that held the key
	Key  ed25519.PrivateKey
	Spec scenario.MCPSign
}

// ParseEd25519Key reads an Ed25519 private key from the text of a secret. Accepted: a PKCS#8 PEM ("PRIVATE KEY"),
// or base64 (standard or URL alphabet, padded or not) of a raw 32-byte seed, of the 64-byte Go form (seed then
// public key), or of a PKCS#8 DER. The error says WHAT is wrong in fixed words and never quotes the value.
func ParseEd25519Key(text string) (ed25519.PrivateKey, error) {
	t := strings.TrimSpace(text)
	if t == "" {
		return nil, errors.New("the secret is empty")
	}
	if strings.Contains(t, "-----BEGIN") {
		blk, _ := pem.Decode([]byte(t))
		if blk == nil {
			return nil, errors.New("the secret looks like a PEM block that cannot be read")
		}
		return fromDER(blk.Bytes)
	}
	raw, ok := decodeB64(t)
	if !ok {
		return nil, errors.New("the secret is neither a PKCS#8 PEM nor base64")
	}
	switch len(raw) {
	case ed25519.SeedSize:
		return ed25519.NewKeyFromSeed(raw), nil
	case ed25519.PrivateKeySize:
		k := ed25519.PrivateKey(raw)
		if !k.Public().(ed25519.PublicKey).Equal(ed25519.NewKeyFromSeed(raw[:ed25519.SeedSize]).Public()) {
			return nil, errors.New("the 64-byte key's public half does not match its seed")
		}
		return k, nil
	}
	return fromDER(raw)
}

func fromDER(der []byte) (ed25519.PrivateKey, error) {
	k, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, errors.New("the secret is not a 32-byte seed, a 64-byte Ed25519 key or a PKCS#8 private key")
	}
	ek, ok := k.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("the secret is a private key of another algorithm; only Ed25519 signs")
	}
	return ek, nil
}

func decodeB64(s string) ([]byte, bool) {
	s = strings.Join(strings.Fields(s), "")
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil {
			return b, true
		}
	}
	return nil, false
}

// signError is a harness problem: the step is `error` (grey), never `failed`. The text names the reference and the
// problem, and never a key.
type signError struct{ msg string }

func (e *signError) Error() string { return e.msg }

func signErr(format string, a ...any) error { return &signError{fmt.Sprintf(format, a...)} }

// IsSignError reports whether err is a harness problem of the sign block.
func IsSignError(err error) bool { var se *signError; return errors.As(err, &se) }

// SignOutcome is what one signing produced for one call.
type SignOutcome struct {
	Args    any               // the request arguments with the signature injected (a copy)
	Headers map[string]string // headers to send with this call
}

// bindSavedRaw fills ${saved.<var>} with the saved text EXACTLY as saved. bindSaved JSON-escapes, which is right for
// a value spliced into a JSON document and wrong for bytes that are signed or sent in a header. A variable saved with
// the `json` form is refused (its text is a JSON fragment, not a value).
func bindSavedRaw(s string, vars map[string]string) (string, error) {
	var missing []string
	out := savedRefRe.ReplaceAllStringFunc(s, func(m string) string {
		name := savedRefRe.FindStringSubmatch(m)[1]
		v, ok := vars[name]
		if !ok || isJSONSaved(vars, name) {
			missing = append(missing, name)
			return m
		}
		return v
	})
	if len(missing) > 0 {
		return "", unresolvedSavedError(missing)
	}
	return out, nil
}

// signCall computes the canonical string, signs it, and injects the signature and the key id.
func signCall(p *SignPlan, args any, vars map[string]string) (SignOutcome, error) {
	sp := p.Spec
	var canon string
	if sp.Value != "" {
		v, err := bindSavedRaw(sp.Value, vars)
		if err != nil {
			m := savedRefRe.FindStringSubmatch(sp.Value)
			name := ""
			if len(m) > 1 {
				name = m[1]
			}
			return SignOutcome{}, signErr("signing with key reference %q: the value to sign, ${saved.%s}, was not saved by an earlier step - nothing was sent", p.Ref, name)
		}
		canon = v
	} else {
		parts := make([]string, 0, len(sp.Fields))
		for i, f := range sp.Fields {
			var s string
			if f.Literal != nil {
				b, err := bindSavedRaw(*f.Literal, vars)
				if err != nil {
					return SignOutcome{}, signErr("signing with key reference %q: field %d names a ${saved.…} variable no earlier step saved - nothing was sent", p.Ref, i+1)
				}
				s = b
			} else {
				segs, _ := scenario.SignPathSegments(f.Path)
				got, ok := lookupSegs(args, segs)
				if !ok {
					return SignOutcome{}, signErr("signing with key reference %q: field %d reads %q in the request arguments, and nothing scalar is there - nothing was sent", p.Ref, i+1, f.Path)
				}
				s = got
			}
			switch f.Sha256 {
			case "hex":
				h := sha256.Sum256([]byte(s))
				s = hex.EncodeToString(h[:])
			case "base64":
				h := sha256.Sum256([]byte(s))
				s = base64.StdEncoding.EncodeToString(h[:])
			}
			parts = append(parts, s)
		}
		sep := ""
		if sp.Separator != nil {
			sep = *sp.Separator
		}
		canon = strings.Join(parts, sep)
	}
	sig := base64.StdEncoding.EncodeToString(ed25519.Sign(p.Key, []byte(canon)))

	out := SignOutcome{Args: args, Headers: map[string]string{}}
	if sp.Arg != "" {
		segs, _ := scenario.SignPathSegments(sp.Arg)
		na, err := setSegs(args, segs, sig)
		if err != nil {
			return SignOutcome{}, signErr("signing with key reference %q: the signature cannot be put at %q in the request arguments (%s) - nothing was sent", p.Ref, sp.Arg, err.Error())
		}
		out.Args = na
	}
	if sp.Header != "" {
		out.Headers[sp.Header] = sig
	}
	if sp.KeyIDHeader != "" {
		id, err := bindSavedRaw(sp.KeyID, vars)
		if err != nil {
			return SignOutcome{}, signErr("signing with key reference %q: the key id names a ${saved.…} variable no earlier step saved - nothing was sent", p.Ref)
		}
		out.Headers[sp.KeyIDHeader] = id
	}
	return out, nil
}

// lookupSegs walks decoded JSON by segments and returns a scalar as text (strings as they are, numbers without a
// trailing ".0", booleans as true/false). A container, null or a missing element is not found.
func lookupSegs(v any, segs []string) (string, bool) {
	cur := v
	for _, seg := range segs {
		switch n := cur.(type) {
		case map[string]any:
			next, ok := n[seg]
			if !ok {
				return "", false
			}
			cur = next
		case []any:
			i, err := strconv.Atoi(seg)
			if err != nil || i < 0 || i >= len(n) {
				return "", false
			}
			cur = n[i]
		default:
			return "", false
		}
	}
	switch x := cur.(type) {
	case string:
		return x, true
	case bool:
		return strconv.FormatBool(x), true
	case json.Number:
		return x.String(), true
	case float64:
		if x == float64(int64(x)) {
			return strconv.FormatInt(int64(x), 10), true
		}
		return strconv.FormatFloat(x, 'f', -1, 64), true
	}
	return "", false
}

// setSegs returns a deep copy of v with val set at segs, creating the objects on the way. The arguments must be a
// JSON object (or the path must lead through objects); an existing non-object on the way is refused.
func setSegs(v any, segs []string, val string) (any, error) {
	root, ok := deepCopy(v).(map[string]any)
	if !ok {
		return nil, errors.New("the step's args are not a JSON object")
	}
	cur := root
	for i, seg := range segs {
		if i == len(segs)-1 {
			cur[seg] = val
			return root, nil
		}
		next, exists := cur[seg]
		if !exists || next == nil {
			m := map[string]any{}
			cur[seg] = m
			cur = m
			continue
		}
		m, isMap := next.(map[string]any)
		if !isMap {
			return nil, fmt.Errorf("%q is not an object", seg)
		}
		cur = m
	}
	return root, nil
}

func deepCopy(v any) any {
	switch x := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(x))
		for k, e := range x {
			m[k] = deepCopy(e)
		}
		return m
	case []any:
		s := make([]any, len(x))
		for i, e := range x {
			s[i] = deepCopy(e)
		}
		return s
	}
	return v
}
