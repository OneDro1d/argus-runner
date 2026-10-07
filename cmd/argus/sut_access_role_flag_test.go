package main

import (
	"os"
	"strings"
	"testing"
)

// TestRenderK8s_EmitSUTAccessRole_WritesSeparateFile (P3 #23): --emit-sut-access-role must produce
// a THIRD file, never folded into executor.yaml/obs.yaml — those are applied by the operator into
// the executor's OWN namespace, while this one is meant for the SUT owner to review and apply in a
// DIFFERENT namespace they own.
func TestRenderK8s_EmitSUTAccessRole_WritesSeparateFile(t *testing.T) {
	rc, out, _ := runRenderK8s(t, "--emit-sut-access-role")
	if rc != exitOK {
		t.Fatalf("render-k8s exited %d: %+v", rc, out)
	}
	if out.SUTAccessRole == "" {
		t.Fatal("no sut_access_role path in output")
	}
	manifest, err := os.ReadFile(out.SUTAccessRole)
	if err != nil {
		t.Fatalf("read sut access role manifest: %v", err)
	}
	m := string(manifest)
	if !strings.Contains(m, "kind: Role") || !strings.Contains(m, "kind: RoleBinding") {
		t.Errorf("manifest does not carry a Role + RoleBinding:\n%s", m)
	}
	if !strings.Contains(m, "namespace: sut") { // runRenderK8s's default --sut-namespace
		t.Errorf("manifest does not target the SUT namespace:\n%s", m)
	}
	execManifest, err := os.ReadFile(out.Executor)
	if err != nil {
		t.Fatalf("read executor manifest: %v", err)
	}
	if strings.Contains(string(execManifest), "argus-load-environment-read") {
		t.Error("the SUT access role leaked into executor.yaml — it must stay a separate file")
	}
}

// TestRenderK8s_NoEmitFlag_NoSUTAccessRoleFile: the default (no flag) must not write this file at
// all — it is opt-in.
func TestRenderK8s_NoEmitFlag_NoSUTAccessRoleFile(t *testing.T) {
	rc, out, dir := runRenderK8s(t)
	if rc != exitOK {
		t.Fatalf("render-k8s exited %d: %+v", rc, out)
	}
	if out.SUTAccessRole != "" {
		t.Errorf("sut_access_role = %q with no --emit-sut-access-role flag", out.SUTAccessRole)
	}
	if _, err := os.Stat(dir + "/sut-access-role.yaml"); err == nil {
		t.Error("sut-access-role.yaml written despite no --emit-sut-access-role flag")
	}
}

// TestRenderK8s_EmitSUTAccessRole_NoNamespace_Refused: the flag with no --sut-namespace has nothing
// to target — refused BEFORE anything is written, same posture as the other render-k8s refusals.
func TestRenderK8s_EmitSUTAccessRole_NoNamespace_Refused(t *testing.T) {
	// runRenderK8s always passes --sut-namespace sut; override it to empty by re-passing the flag —
	// flag.FlagSet takes the LAST occurrence, so this simulates "no namespace given" for this check.
	rc, out, dir := runRenderK8s(t, "--emit-sut-access-role", "--sut-namespace", "")
	if rc == exitOK {
		t.Fatalf("render-k8s accepted --emit-sut-access-role with an empty --sut-namespace; want a refusal. output: %+v", out)
	}
	if out.Error == "" {
		t.Errorf("no error message in output: %+v", out)
	}
	if _, err := os.Stat(dir + "/executor.yaml"); err == nil {
		t.Error("executor.yaml was written despite the refused --emit-sut-access-role — nothing should be written before the refusal")
	}
}
