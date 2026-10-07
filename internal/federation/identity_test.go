package federation

import (
	"crypto/ed25519"
	"strings"
	"testing"
	"time"
)

func mustKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

func TestJWT_MintVerifyRoundTrip(t *testing.T) {
	pub, priv := mustKey(t)
	now := time.Unix(1_700_000_000, 0)
	tok, err := MintJWT(priv, "inst-abc", now)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	cl, err := VerifyJWT(tok, pub, now.Add(10*time.Second))
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if cl.Sub != "inst-abc" {
		t.Errorf("sub = %q, want inst-abc", cl.Sub)
	}
}

func TestJWT_Expired(t *testing.T) {
	pub, priv := mustKey(t)
	now := time.Unix(1_700_000_000, 0)
	tok, _ := MintJWT(priv, "i", now)
	// well beyond exp(+60s) + skew(+300s)
	_, err := VerifyJWT(tok, pub, now.Add(JWTLifetime+SkewWindow+time.Second))
	if AsRejectReason(err) != ReasonExpired {
		t.Fatalf("got reason %q, want expired", AsRejectReason(err))
	}
}

func TestJWT_SkewToleratedBothDirections(t *testing.T) {
	pub, priv := mustKey(t)
	now := time.Unix(1_700_000_000, 0)
	tok, _ := MintJWT(priv, "i", now)
	// CP clock 200s BEHIND the executor (now-200): within -skew of iat → accepted.
	if _, err := VerifyJWT(tok, pub, now.Add(-200*time.Second)); err != nil {
		t.Errorf("200s-behind should be tolerated, got %v", err)
	}
	// CP clock 200s AHEAD (still within exp+skew) → accepted.
	if _, err := VerifyJWT(tok, pub, now.Add(200*time.Second)); err != nil {
		t.Errorf("200s-ahead should be tolerated, got %v", err)
	}
}

func TestJWT_BadSignature_WrongKey(t *testing.T) {
	_, priv := mustKey(t)
	otherPub, _ := mustKey(t)
	now := time.Unix(1_700_000_000, 0)
	tok, _ := MintJWT(priv, "i", now)
	_, err := VerifyJWT(tok, otherPub, now)
	if AsRejectReason(err) != ReasonBadSignature {
		t.Fatalf("got reason %q, want bad_signature", AsRejectReason(err))
	}
}

func TestJWT_BadSignature_Tampered(t *testing.T) {
	pub, priv := mustKey(t)
	now := time.Unix(1_700_000_000, 0)
	tok, _ := MintJWT(priv, "inst-abc", now)
	// flip a char in the claims segment
	parts := strings.SplitN(tok, ".", 3)
	tampered := parts[0] + "." + flip(parts[1]) + "." + parts[2]
	_, err := VerifyJWT(tampered, pub, now)
	if r := AsRejectReason(err); r != ReasonBadSignature && r != ReasonMalformed {
		t.Fatalf("tampered token got %q, want bad_signature/malformed", r)
	}
}

func TestJWT_Malformed(t *testing.T) {
	pub, _ := mustKey(t)
	for _, tok := range []string{"", "a.b", "not-a-jwt", "a.b.c.d"} {
		if AsRejectReason(VerifyErr(pub, tok)) != ReasonMalformed {
			t.Errorf("token %q not classified malformed", tok)
		}
	}
}

func VerifyErr(pub ed25519.PublicKey, tok string) error {
	_, err := VerifyJWT(tok, pub, time.Unix(1_700_000_000, 0))
	return err
}

func flip(s string) string {
	if s == "" {
		return "x"
	}
	b := []byte(s)
	if b[0] == 'A' {
		b[0] = 'B'
	} else {
		b[0] = 'A'
	}
	return string(b)
}
