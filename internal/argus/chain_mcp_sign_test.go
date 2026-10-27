package argus

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// Per-call Ed25519 signing of an mcp chain step, driven through the REAL chain path
// (runChainScenario -> mcpPreflight -> chain.MCPStepWith -> mcp.Client) against a fake MCP server that VERIFIES the
// signatures. Every key is generated here; no key material is committed.

type signKeys struct {
	pubA, pubB   ed25519.PublicKey
	seedA        []byte
	seedB        []byte
	b64A         string // env value of key A: base64 of the 32-byte seed
	pemB         string // env value of key B: PKCS#8 PEM
	pemBBody     string
	keyIDToKey   map[string]ed25519.PublicKey
	privB64Wrong string
}

func newSignKeys(t *testing.T) signKeys {
	t.Helper()
	pubA, privA, _ := ed25519.GenerateKey(rand.Reader)
	pubB, privB, _ := ed25519.GenerateKey(rand.Reader)
	der, err := x509.MarshalPKCS8PrivateKey(privB)
	if err != nil {
		t.Fatal(err)
	}
	pemB := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	return signKeys{pubA: pubA, pubB: pubB, seedA: privA.Seed(), seedB: privB.Seed(),
		b64A: base64.StdEncoding.EncodeToString(privA.Seed()), pemB: pemB,
		pemBBody:   base64.StdEncoding.EncodeToString(der),
		keyIDToKey: map[string]ed25519.PublicKey{"key-a": pubA, "key-b": pubB}}
}

// secrets lists every form of the test private keys a leak could take: raw bytes, base64 (seed and DER), PEM.
func (k signKeys) secrets() []string {
	return []string{string(k.seedA), string(k.seedB), k.b64A, base64.StdEncoding.EncodeToString(k.seedB),
		base64.RawStdEncoding.EncodeToString(k.seedA), hex.EncodeToString(k.seedA), hex.EncodeToString(k.seedB),
		k.pemB, k.pemBBody, "BEGIN PRIVATE KEY"}
}

// signServer is a fake MCP server in the msgbus shape: call 1 (dryRun) answers the canonical string to sign; call 2
// must carry a valid Ed25519 signature for the key id in the x-key-id header, over that same canonical string.
type signServer struct {
	mu        sync.Mutex
	keys      map[string]ed25519.PublicKey
	toolCalls int
	valid     []string // key ids whose signature verified
	invalid   int
	argsSeen  []string
}

func canonicalOf(messageID, prompt string) string {
	h := sha256.Sum256([]byte(prompt))
	return "v4|" + messageID + "|" + hex.EncodeToString(h[:])
}

func (s *signServer) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Arguments json.RawMessage `json:"arguments"`
			} `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "s1")
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"protocolVersion": "2025-03-26"}})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/call":
			var a struct {
				MessageID  string            `json:"messageId"`
				Prompt     string            `json:"prompt"`
				DryRun     bool              `json:"dryRun"`
				Signatures map[string]string `json:"signatures"`
			}
			_ = json.Unmarshal(req.Params.Arguments, &a)
			canon := canonicalOf(a.MessageID, a.Prompt)
			s.mu.Lock()
			s.toolCalls++
			s.argsSeen = append(s.argsSeen, string(req.Params.Arguments))
			s.mu.Unlock()
			text, isErr := "", false
			if a.DryRun {
				b, _ := json.Marshal(map[string]string{"canonical": canon})
				text = string(b)
			} else {
				keyID := r.Header.Get("x-key-id")
				pub, known := s.keys[keyID]
				ok := false
				for _, sig := range a.Signatures {
					raw, err := base64.StdEncoding.DecodeString(sig)
					if known && err == nil && ed25519.Verify(pub, []byte(canon), raw) {
						ok = true
					}
				}
				s.mu.Lock()
				if ok {
					s.valid = append(s.valid, keyID)
				} else {
					s.invalid++
				}
				s.mu.Unlock()
				if ok {
					text = "accepted from " + keyID
				} else {
					text, isErr = "signature rejected", true
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{
				"content": []any{map[string]any{"type": "text", "text": text}}, "isError": isErr}})
		}
	}
}

func signCfg(t *testing.T, base string, names ...string) *config.Config {
	t.Helper()
	c := chainCfg(t, base)
	c.CheckEnv = names
	return c
}

const dryStep = `{"type":"mcp","name":"dry","tool":"send","args":{"messageId":"m1","prompt":"hello","dryRun":true},"save":{"canonical":"canonical"}}`

// callOne: the saved-value form, key A (a base64 seed), the signature in the arguments at a JSON pointer whose key holds a dot.
const sendA = `{"type":"mcp","name":"send-a","tool":"send","args":{"messageId":"m1","prompt":"hello","signatures":{}},
 "sign":{"key":"MEMBER_A_KEY","key_id":"key-a","key_id_header":"x-key-id","value":"${saved.canonical}","arg":"/signatures/agent.inbox"}}`

// callTwo: the template form, key B (a PKCS#8 PEM), the same canonical string built from the request's own fields.
const sendB = `{"type":"mcp","name":"send-b","tool":"send","args":{"messageId":"m1","prompt":"hello"},
 "sign":{"key":"MEMBER_B_KEY","key_id":"key-b","key_id_header":"x-key-id","separator":"|",
  "fields":[{"literal":"v4"},{"path":"messageId"},{"path":"prompt","sha256":"hex"}],"arg":"signatures.inbox"}}`

const signClaims = "### Runnable\n- step dry: body contains canonical\n- step send-a: body contains accepted from key-a\n- step send-b: body contains accepted from key-b\n"

func runSign(t *testing.T, c *config.Config, steps, claims string) report.ScenarioResult {
	t.Helper()
	s := scenario.Parse(chainRunMD("", `{"steps":[`+steps+`]}`, claims))
	return runChainScenario(c, s, "tr-sign", "testkit/ui")
}

func TestChainMCP_Sign_TwoKeysTwoForms_ServerSeesTwoValidSignatures(t *testing.T) {
	k := newSignKeys(t)
	t.Setenv("MEMBER_A_KEY", k.b64A)
	t.Setenv("MEMBER_B_KEY", k.pemB)
	srv := &signServer{keys: k.keyIDToKey}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()
	c := signCfg(t, ts.URL, "MEMBER_A_KEY", "MEMBER_B_KEY")
	res := runSign(t, c, dryStep+","+sendA+","+sendB, signClaims)
	if res.Status != "passed" {
		t.Fatalf("want passed, got %q %+v %+v", res.Status, res.Failure, res.Steps)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if len(srv.valid) != 2 || srv.valid[0] != "key-a" || srv.valid[1] != "key-b" || srv.invalid != 0 {
		t.Fatalf("the server must see two valid signatures from key-a and key-b: valid=%v invalid=%d args=%v", srv.valid, srv.invalid, srv.argsSeen)
	}
	// the evidence the run produced carries none of the private keys, in any form
	out, _ := json.MarshalIndent(res, "", " ")
	// Optional, for an out-of-process grep: SIGN_EVIDENCE_DIR=<dir> writes the evidence (result.json) and the test keys in
	// every form (needles.txt, one per line, newline-free forms only) into <dir>. Never set in CI; <dir> is not in the repo.
	if dir := os.Getenv("SIGN_EVIDENCE_DIR"); dir != "" {
		_ = os.WriteFile(filepath.Join(dir, "result.json"), out, 0o600)
		var needles []string
		for _, s := range k.secrets() {
			if !strings.Contains(s, "\n") && len(s) >= 8 {
				needles = append(needles, s)
			}
		}
		_ = os.WriteFile(filepath.Join(dir, "needles.txt"), []byte(strings.Join(needles, "\n")+"\n"), 0o600)
	}
	for i, leak := range k.secrets() {
		if strings.Contains(string(out), leak) {
			t.Errorf("the run's evidence carries private key material (form #%d of signKeys.secrets)", i)
		}
	}
}

func TestChainMCP_Sign_AWrongSignatureIsTheSUTsFailure_NotTheHarnesss(t *testing.T) {
	k := newSignKeys(t)
	t.Setenv("MEMBER_A_KEY", k.b64A)
	// the server knows key-a as a DIFFERENT public key: the signature is valid Ed25519 but is not key-a's
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	srv := &signServer{keys: map[string]ed25519.PublicKey{"key-a": other}}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()
	c := signCfg(t, ts.URL, "MEMBER_A_KEY")
	res := runSign(t, c, dryStep+","+sendA, "### Runnable\n- step dry: body contains canonical\n- step send-a: body contains accepted\n")
	if res.Status != "failed" {
		t.Fatalf("a signature the SUT rejects is the SUT's answer: want failed, got %q %+v", res.Status, res.Steps)
	}
}

func TestChainMCP_Sign_HarnessErrorsAreGreyAndNameTheReference(t *testing.T) {
	k := newSignKeys(t)
	cases := map[string]struct {
		env      map[string]string
		declared []string
		steps    string
		wantRef  string
		wantSent int // tools/call requests that reached the server
	}{
		"not a key": {env: map[string]string{"MEMBER_A_KEY": "this-is-not-an-ed25519-key"}, declared: []string{"MEMBER_A_KEY"},
			steps: dryStep + "," + sendA, wantRef: "MEMBER_A_KEY"},
		"not base64 at all": {env: map[string]string{"MEMBER_A_KEY": "words that are not a key !!"}, declared: []string{"MEMBER_A_KEY"},
			steps: dryStep + "," + sendA, wantRef: "MEMBER_A_KEY"},
		"a key of another algorithm": {env: map[string]string{"MEMBER_A_KEY": otherAlgoPEM(t)}, declared: []string{"MEMBER_A_KEY"},
			steps: dryStep + "," + sendA, wantRef: "MEMBER_A_KEY"},
		"empty": {env: map[string]string{"MEMBER_A_KEY": ""}, declared: []string{"MEMBER_A_KEY"},
			steps: dryStep + "," + sendA, wantRef: "MEMBER_A_KEY"},
		"not declared": {env: map[string]string{"MEMBER_A_KEY": k.b64A}, declared: nil,
			steps: dryStep + "," + sendA, wantRef: "MEMBER_A_KEY"},
		"saved value missing": {env: map[string]string{"MEMBER_A_KEY": k.b64A}, declared: []string{"MEMBER_A_KEY"},
			steps:   strings.Replace(sendA, "${saved.canonical}", "${saved.never_saved}", 1),
			wantRef: "never_saved", wantSent: 0},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			for e, v := range tc.env {
				t.Setenv(e, v)
			}
			srv := &signServer{keys: k.keyIDToKey}
			ts := httptest.NewServer(srv.handler())
			defer ts.Close()
			c := signCfg(t, ts.URL, tc.declared...)
			res := runSign(t, c, tc.steps, "### Runnable\n- step send-a: body contains accepted\n")
			if res.Status != report.StatusError {
				t.Fatalf("a harness problem is grey (error), never red: got %q %+v %+v", res.Status, res.Failure, res.Steps)
			}
			text := ""
			if res.Failure != nil {
				text = res.Failure.Observed
			}
			for _, st := range res.Steps {
				text += " " + st.Observed
			}
			if !strings.Contains(text, tc.wantRef) {
				t.Errorf("the sentence must name %q: %s", tc.wantRef, text)
			}
			for _, v := range tc.env {
				if v != "" && strings.Contains(text, v) {
					t.Errorf("the sentence carries the key value")
				}
			}
			srv.mu.Lock()
			defer srv.mu.Unlock()
			if len(srv.valid) != 0 || srv.invalid != 0 {
				t.Errorf("the signed call must not be sent without a signature: valid=%v invalid=%d", srv.valid, srv.invalid)
			}
		})
	}
}

func otherAlgoPEM(t *testing.T) string {
	t.Helper()
	// an RSA-free way to get a non-Ed25519 PKCS#8: an ECDSA key
	return ecdsaPEM(t)
}

// The signature is injected where the step says AND/OR in a header; the key id is in the header the step names.
func TestChainMCP_Sign_HeaderDestination(t *testing.T) {
	k := newSignKeys(t)
	t.Setenv("MEMBER_A_KEY", k.b64A)
	var mu sync.Mutex
	var gotSig, gotKeyID string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "s1")
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"protocolVersion": "2025-03-26"}})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/call":
			mu.Lock()
			gotSig, gotKeyID = r.Header.Get("X-Signature"), r.Header.Get("x-key-id")
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{
				"content": []any{map[string]any{"type": "text", "text": "ok"}}, "isError": false}})
		}
	}))
	defer ts.Close()
	step := `{"type":"mcp","name":"s","tool":"t","args":{"a":"x"},"sign":{"key":"MEMBER_A_KEY","key_id":"key-a","key_id_header":"x-key-id","fields":[{"path":"a"}],"header":"X-Signature"}}`
	res := runSign(t, signCfg(t, ts.URL, "MEMBER_A_KEY"), step, "### Runnable\n- step s: body contains ok\n")
	if res.Status != "passed" {
		t.Fatalf("%q %+v", res.Status, res.Failure)
	}
	raw, err := base64.StdEncoding.DecodeString(gotSig)
	if err != nil || !ed25519.Verify(k.pubA, []byte("x"), raw) || gotKeyID != "key-a" {
		t.Fatalf("header signature/key id wrong: sig=%q keyid=%q err=%v", gotSig, gotKeyID, err)
	}
}

func ecdsaPEM(t *testing.T) string {
	t.Helper()
	pk, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(pk)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}
