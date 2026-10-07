package federation

// sealedexpect_test.go — AC-4: EXPECT sealed on the wire under a run-scoped key (X25519 + HKDF +
// AES-256-GCM, standard library only). TEST-FIRST (RED before sealedexpect.go/ScenarioPayload.Sealed
// exist): a sealed ScenarioPayload carries no clear EXPECT text; only the matching executor private key
// AND the matching run_id can open it; the opened text is exactly what the author wrote.

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/json"
	"strings"
	"testing"
)

const authorExpectText = "## EXPECT\n### Runnable\n- expect.status: 201\n- HOLDOUT-MARKER-DO-NOT-LEAK\n"

func genX25519(t *testing.T) *ecdh.PrivateKey {
	t.Helper()
	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate X25519 key: %v", err)
	}
	return priv
}

// TestScenarioPayload_SealedCarriesNoClearBody extends the forbidden-key pattern of
// TestResultsPush_NoEvidenceFields: a sealed payload's marshaled JSON must not contain the plaintext
// EXPECT text, and must carry no "body" key at all — Sealed and Body are mutually exclusive on the wire.
func TestScenarioPayload_SealedCarriesNoClearBody(t *testing.T) {
	executorPriv := genX25519(t)
	sealed, err := SealBody(executorPriv.PublicKey(), "run-1", authorExpectText)
	if err != nil {
		t.Fatalf("SealBody: %v", err)
	}
	p := ScenarioPayload{Path: "http-ingestion/ORDE-017.md", Sealed: &sealed}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), "HOLDOUT-MARKER-DO-NOT-LEAK") {
		t.Fatalf("sealed payload leaked plaintext EXPECT text: %s", b)
	}
	if strings.Contains(string(b), `"body"`) {
		t.Fatalf("sealed payload still carries a body key: %s", b)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["sealed"]; !ok {
		t.Fatalf("sealed payload marshaled with no \"sealed\" key: %s", b)
	}
}

// TestSealedBody_RoundTrip_ParserSeesAuthorTextUnchanged: the executor holding the matching private key
// and the matching run_id recovers EXACTLY the author's text.
func TestSealedBody_RoundTrip_ParserSeesAuthorTextUnchanged(t *testing.T) {
	priv := genX25519(t)
	sealed, err := SealBody(priv.PublicKey(), "run-42", authorExpectText)
	if err != nil {
		t.Fatalf("SealBody: %v", err)
	}
	got, err := OpenBody(priv, sealed, "run-42")
	if err != nil {
		t.Fatalf("OpenBody: %v", err)
	}
	if got != authorExpectText {
		t.Fatalf("opened body = %q, want exactly the author's text %q", got, authorExpectText)
	}
}

// TestSealedBody_WrongPrivateKeyFails: a runner without the matching private key cannot read it.
func TestSealedBody_WrongPrivateKeyFails(t *testing.T) {
	rightPriv := genX25519(t)
	wrongPriv := genX25519(t)
	sealed, err := SealBody(rightPriv.PublicKey(), "run-1", authorExpectText)
	if err != nil {
		t.Fatalf("SealBody: %v", err)
	}
	if _, err := OpenBody(wrongPriv, sealed, "run-1"); err == nil {
		t.Fatal("OpenBody succeeded with the WRONG private key; want a decrypt failure")
	}
}

// TestSealedBody_WrongRunIDFails: a wrong run_id fails to decrypt (the HKDF salt binds the key to ONE run).
func TestSealedBody_WrongRunIDFails(t *testing.T) {
	priv := genX25519(t)
	sealed, err := SealBody(priv.PublicKey(), "run-1", authorExpectText)
	if err != nil {
		t.Fatalf("SealBody: %v", err)
	}
	if _, err := OpenBody(priv, sealed, "run-2"); err == nil {
		t.Fatal("OpenBody succeeded under the WRONG run_id; want a decrypt failure")
	}
}

// TestSealAssignmentScenarios_NoPubkeyLeavesBodyClear: an executor with no registered X25519 key gets
// EXPECT in the clear exactly as before this ticket — never a fault, never refused.
func TestSealAssignmentScenarios_NoPubkeyLeavesBodyClear(t *testing.T) {
	in := []ScenarioPayload{{Path: "a.md", Body: "clear text"}}
	out, err := SealAssignmentScenarios(nil, "run-1", in)
	if err != nil {
		t.Fatalf("SealAssignmentScenarios: %v", err)
	}
	if out[0].Body != "clear text" || out[0].Sealed != nil {
		t.Fatalf("SealAssignmentScenarios with nil pubkey = %+v, want unchanged clear body", out[0])
	}
}

// TestSealAssignmentScenarios_WithPubkeySealsEveryScenario: with a registered pubkey, every scenario's
// body is sealed and none carries clear text.
func TestSealAssignmentScenarios_WithPubkeySealsEveryScenario(t *testing.T) {
	priv := genX25519(t)
	in := []ScenarioPayload{
		{Path: "a.md", Body: authorExpectText},
		{Path: "b.md", Body: "## EXPECT\n### Runnable\n- expect.status: 404\n"},
	}
	out, err := SealAssignmentScenarios(priv.PublicKey(), "run-9", in)
	if err != nil {
		t.Fatalf("SealAssignmentScenarios: %v", err)
	}
	for i, sc := range out {
		if sc.Body != "" || sc.Sealed == nil {
			t.Fatalf("scenario %d = %+v, want cleared Body + a Sealed payload", i, sc)
		}
		opened, err := OpenBody(priv, *sc.Sealed, "run-9")
		if err != nil {
			t.Fatalf("OpenBody(%d): %v", i, err)
		}
		if opened != in[i].Body {
			t.Fatalf("scenario %d opened = %q, want %q", i, opened, in[i].Body)
		}
	}
}
