package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/router"
)

// captureEmitRaw runs fn with os.Stdout redirected and returns the BYTES emit() wrote.
// emit uses fmt.Println, which resolves os.Stdout at call time, so swapping it is enough.
func captureEmitRaw(t *testing.T, fn func()) []byte {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	done := make(chan []byte, 1)
	go func() { b, _ := io.ReadAll(r); done <- b }()
	fn()
	_ = w.Close()
	os.Stdout = old
	return <-done
}

// captureEmit is captureEmitRaw parsed as JSON, for the assertions that are about VALUES rather than
// about the byte format the shell greps.
func captureEmit(t *testing.T, fn func()) map[string]any {
	t.Helper()
	blob := captureEmitRaw(t, fn)
	var doc map[string]any
	if err := json.Unmarshal(blob, &doc); err != nil {
		t.Fatalf("router status did not emit parseable JSON: %v\n%s", err, blob)
	}
	return doc
}

// VR7-T1 (V24-003) — `router status` MUST STATE THE ROUTER'S ID.
//
// ── THE DEFECT ────────────────────────────────────────────────────────────────────────────────────
//
// teardown.sh reads the id it needs to remove the control-plane registration — the `RT_ID=`
// assignment in its router-reclaim block:
//
//	RT_ID="$(docker run --rm -v <state>:/state "$IMAGE" router status --state /state \
//	          | grep -o '"router_id": *"[^"]*"' | head -1 | cut -d'"' -f4 || true)"
//
// cmdRouterStatus emitted state_dir, port and folders — and nothing else. The grep could never match,
// RT_ID was ALWAYS empty, and VR6-T3 has therefore never removed a registration on any machine, in any
// tier, under any conditions. Confirmed live 2026-08-19: after tearing down the last instance the
// router container and its state dir were gone and rtr_Uq6O5mLZ-yV3GJmq was still in `routers`.
//
// ⚠ THE ORDERING COMMENT ABOVE THAT ASSIGNMENT IS CORRECT AND MUST NOT BE "FIXED". The id genuinely is
// read before anything is destroyed, exactly as it says. The read simply returned nothing.
func TestRouterStatus_EmitsTheRouterID(t *testing.T) {
	dir := t.TempDir()
	id, err := router.LoadOrCreateIdentity(dir)
	if err != nil {
		t.Fatalf("seed identity: %v", err)
	}

	doc := captureEmit(t, func() {
		if rc := cmdRouterStatus([]string{"--state", dir}); rc != 0 {
			t.Fatalf("router status exit code = %d, want 0", rc)
		}
	})

	got, _ := doc["router_id"].(string)
	if got == "" {
		t.Fatalf("router status emitted no router_id. Keys present: %v\n"+
			"  teardown.sh greps this exact key to name the registration it is about to remove.\n"+
			"  Without it RT_ID is always empty and the registration can never be deleted — which is\n"+
			"  the permanent orphan the ordering comment three lines above it exists to prevent.",
			keysOf(doc))
	}
	if got != id.RouterID {
		t.Errorf("router status emitted router_id %q, want %q — it must be the id derived from the key\n"+
			"  in THIS state dir, or teardown would delete a registration belonging to something else.",
			got, id.RouterID)
	}
}

// teardownRouterIDPipeline reads the grep pattern and the cut field number that teardown.sh actually
// uses to pull router_id out of `router status`.
//
// ⚠ IT FAILS RATHER THAN SKIPS when the script cannot be found or the pipeline cannot be located.
// "I could not check the join" is not "the join is fine" — a skip here would restore exactly the
// blindness this test was rewritten to remove.
func teardownRouterIDPipeline(t *testing.T) (pattern string, cutField int) {
	t.Helper()
	path := filepath.Join("..", "..", "onboarding", "teardown.sh")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("cannot read %s: %v\n"+
			"  This test asserts that `router status` output matches the pipeline in that script. Without\n"+
			"  the script there is no contract to check, so it fails rather than reporting a pass it\n"+
			"  cannot justify.", path, err)
	}
	// Find the line that greps router_id and cuts a field out of it.
	line := regexp.MustCompile(`(?m)^.*grep -o '("router_id"[^']*)'.*cut -d'"' -f([0-9]+).*$`).
		FindSubmatch(b)
	if line == nil {
		t.Fatalf("could not find the router_id grep|cut pipeline in %s.\n"+
			"  It is the join this test exists to protect. If teardown now reads router_id a different\n"+
			"  way, point this lookup at the new mechanism — do not delete the check.", path)
	}
	cutField, err = strconv.Atoi(string(line[2]))
	if err != nil || cutField < 1 {
		t.Fatalf("unreadable cut field %q in %s", line[2], path)
	}
	return string(line[1]), cutField
}

// ⚠ THE GUARD. `status` is a READ. It must never mint a credential as a side effect.
//
// LoadOrCreateIdentity does exactly what its name says, so the obvious implementation — call it and
// print the id — would make `router status` CREATE an Ed25519 private key on any machine that ran it
// without one. Teardown runs this command against a state dir that may already be half-dismantled.
func TestRouterStatus_DoesNotMintAnIdentityJustToReportOne(t *testing.T) {
	dir := t.TempDir()

	doc := captureEmit(t, func() {
		if rc := cmdRouterStatus([]string{"--state", dir}); rc != 0 {
			t.Fatalf("router status exit code = %d, want 0 on a dir with no identity", rc)
		}
	})

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read state dir: %v", err)
	}
	for _, e := range entries {
		if e.Name() == "identity.key" {
			t.Fatalf("`router status` CREATED %s. A read-only status command must not mint a private\n"+
				"  key — teardown runs it against a state dir it is in the middle of dismantling.",
				filepath.Join(dir, e.Name()))
		}
	}
	// And it must say so rather than emit an empty id, which a shell would read as success.
	if v, ok := doc["router_id"]; ok && v != "" {
		t.Errorf("router_id = %v on a dir with no identity; want the key absent entirely.\n"+
			"  An empty string here is worse than no key: `cut -d'\"' -f4` yields \"\" either way, and\n"+
			"  the caller cannot tell \"no router here\" from \"could not read it\".", v)
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
