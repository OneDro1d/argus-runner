package chain

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// The signer: key formats, the canonical string, the injection.

func TestParseEd25519Key_AcceptsSeedFullKeyAndPKCS8(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	der, _ := x509.MarshalPKCS8PrivateKey(priv)
	pemText := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	for name, in := range map[string]string{
		"seed std":      base64.StdEncoding.EncodeToString(priv.Seed()),
		"seed raw":      base64.RawStdEncoding.EncodeToString(priv.Seed()),
		"seed url":      base64.URLEncoding.EncodeToString(priv.Seed()),
		"64 bytes":      base64.StdEncoding.EncodeToString(priv),
		"pkcs8 der b64": base64.StdEncoding.EncodeToString(der),
		"pkcs8 pem":     pemText,
		"padded":        "  " + base64.StdEncoding.EncodeToString(priv.Seed()) + "\n",
	} {
		k, err := ParseEd25519Key(in)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if !k.Public().(ed25519.PublicKey).Equal(pub) {
			t.Errorf("%s: wrong key", name)
		}
	}
}

func TestParseEd25519Key_ErrorsNeverQuoteTheValue(t *testing.T) {
	for _, in := range []string{"", "plain words that are not a key!", "QUJDREVGRw==", "-----BEGIN " + "PRIVATE KEY-----\nSECRETBODY\n-----END PRIVATE KEY-----"} {
		_, err := ParseEd25519Key(in)
		if err == nil {
			t.Errorf("accepted %q", in)
			continue
		}
		if in != "" && strings.Contains(err.Error(), strings.TrimSpace(in)) || strings.Contains(err.Error(), "SECRETBODY") {
			t.Errorf("the error quotes the value: %v", err)
		}
	}
}

func TestSignCall_TemplateHashesJoinsAndInjects(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	sep := "|"
	lit := "v4"
	p := &SignPlan{Ref: "K", Key: priv, Spec: scenario.MCPSign{
		Fields:    []scenario.SignField{{Literal: &lit}, {Path: "m.id"}, {Path: "/p.x", Sha256: "hex"}, {Path: "n", Sha256: "base64"}},
		Separator: &sep, Arg: "/sigs/a.b", Header: "X-S", KeyID: "kid", KeyIDHeader: "X-K"}}
	args := map[string]any{"m": map[string]any{"id": "7"}, "p.x": "hello", "n": float64(12)}
	o, err := signCall(p, args, map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	h1 := sha256.Sum256([]byte("hello"))
	h2 := sha256.Sum256([]byte("12"))
	want := "v4|7|" + hex.EncodeToString(h1[:]) + "|" + base64.StdEncoding.EncodeToString(h2[:])
	sig := o.Args.(map[string]any)["sigs"].(map[string]any)["a.b"].(string)
	raw, _ := base64.StdEncoding.DecodeString(sig)
	if !ed25519.Verify(pub, []byte(want), raw) {
		t.Fatalf("the signature is not over %q", want)
	}
	if o.Headers["X-S"] != sig || o.Headers["X-K"] != "kid" {
		t.Fatalf("headers %v", o.Headers)
	}
	if _, touched := args["sigs"]; touched {
		t.Fatal("the caller's args were mutated")
	}
}

func TestSignCall_SavedValueIsSignedAsIs(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	p := &SignPlan{Ref: "K", Key: priv, Spec: scenario.MCPSign{Value: "${saved.c}", Header: "X-S"}}
	o, err := signCall(p, map[string]any{}, map[string]string{"c": "line1\nline2 \"q\""})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := base64.StdEncoding.DecodeString(o.Headers["X-S"])
	if !ed25519.Verify(pub, []byte("line1\nline2 \"q\""), raw) {
		t.Fatal("the saved value must be signed byte for byte, with no JSON escaping")
	}
}

func TestSignCall_HarnessProblemsNameTheReference(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	p := &SignPlan{Ref: "MY_REF", Key: priv, Spec: scenario.MCPSign{Fields: []scenario.SignField{{Path: "absent"}}, Header: "X-S"}}
	_, err := signCall(p, map[string]any{}, nil)
	if !IsSignError(err) || !strings.Contains(err.Error(), "MY_REF") {
		t.Fatalf("want a sign error naming the reference: %v", err)
	}
	p.Spec = scenario.MCPSign{Value: "${saved.gone}", Header: "X-S"}
	_, err = signCall(p, nil, map[string]string{})
	if !IsSignError(err) || !strings.Contains(err.Error(), "gone") || !strings.Contains(err.Error(), "MY_REF") {
		t.Fatalf("want the missing saved variable and the reference named: %v", err)
	}
}
