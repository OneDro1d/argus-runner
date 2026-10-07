package k8srender

import (
	"strings"
	"testing"
)

// keyB64 is an 88-char base64 (64 raw bytes) — the shape `argus keygen` emits.
const keyB64 = "QUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUE="

// G4/S4: with IdentityKeyB64 set, the machine key is rendered as the exec-identity Secret and mounted
// READ-ONLY off the results volume; the whole manifest stays valid (a misaligned Sprintf arg after the
// injection would garble the YAML or shift a later object's ns/id — both caught here).
func TestRenderExecutor_identitySecret(t *testing.T) {
	in := sampleInstance()
	in.IdentityKeyB64 = keyB64

	got, err := RenderExecutor(in)
	if err != nil {
		t.Fatalf("RenderExecutor with key: %v", err)
	}
	ds := docs(t, got) // must still parse as valid multi-doc YAML

	// the exec-identity Secret exists, in the instance namespace, carrying the key verbatim in data
	found := false
	for _, d := range ds {
		if nameOf(d) == "exec-identity" {
			found = true
			if nsOf(d) != in.Namespace() {
				t.Errorf("exec-identity ns = %q, want %q", nsOf(d), in.Namespace())
			}
			data, _ := d["data"].(map[string]any)
			if data["identity.key"] != keyB64 {
				t.Errorf("exec-identity data.identity.key = %v, want the provided key", data["identity.key"])
			}
		}
	}
	if !found {
		t.Fatalf("exec-identity Secret not rendered when IdentityKeyB64 is set")
	}
	if !strings.Contains(got, "value: /etc/argus/identity/identity.key") {
		t.Error("ARGUS_IDENTITY_PATH not pointed at the Secret mount")
	}
	if strings.Contains(got, "value: /results/identity.key") {
		t.Error("ARGUS_IDENTITY_PATH still points at the results volume")
	}
	if !strings.Contains(got, "mountPath: /etc/argus/identity") {
		t.Error("identity Secret not mounted")
	}
	if !strings.Contains(got, "secretName: exec-identity") {
		t.Error("identity volume not sourced from the exec-identity Secret")
	}
	// arg-alignment sanity: every core object still renders (a shifted arg would drop/garble one)
	kinds := kindsOf(ds)
	for _, k := range []string{"Deployment", "Service", "Secret", "ConfigMap", "ServiceAccount", "Role", "RoleBinding"} {
		if kinds[k] == 0 {
			t.Errorf("kind %s missing after identity injection (arg misalignment?)", k)
		}
	}
	if kinds["Secret"] < 2 {
		t.Errorf("expected exec-tokens + exec-identity = 2 Secrets, got %d", kinds["Secret"])
	}
}

// Backward-compat: no key provided → the legacy self-mint path on the results volume is unchanged,
// and NO identity Secret/mount appears.
func TestRenderExecutor_identityLegacyWhenNoKey(t *testing.T) {
	got, err := RenderExecutor(sampleInstance())
	if err != nil {
		t.Fatalf("RenderExecutor: %v", err)
	}
	if !strings.Contains(got, "value: /results/identity.key") {
		t.Error("legacy ARGUS_IDENTITY_PATH (/results/identity.key) not preserved when no key provided")
	}
	if strings.Contains(got, "exec-identity") {
		t.Error("exec-identity Secret rendered even without a key")
	}
	if strings.Contains(got, "/etc/argus/identity") {
		t.Error("identity Secret mount rendered even without a key")
	}
	if k := kindsOf(docs(t, got)); k["Deployment"] == 0 || k["Service"] == 0 {
		t.Error("legacy render missing core objects")
	}
}
