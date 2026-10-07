package main

import (
	"strings"
	"testing"
)

// secrets_kubeconfig_test.go — onboarding review item 12 (msgbus tester, 2026-09-29): `argus secrets`
// had --kube-context but no --kubeconfig, so an in-cluster tester (whose kubeconfig is NOT the default)
// had to export KUBECONFIG, which does not persist between an agent's separate shell calls and falls
// back silently to the default context. The flag must reach EVERY kubectl call each subcommand makes,
// including the follow-up `rollout restart` and the restart command the result prints for the operator.

const kubeconfigPath = "/home/tester/.config/argus/msgbus.kubeconfig"

func TestSecretsKubeconfig_SetPassesFlagToEveryKubectlCall(t *testing.T) {
	dir := t.TempDir()
	writeFakeKubectl(t, dir, "")

	out, code := runSecretsWithStdin(t, "v4lue", cmdSecretsSet,
		[]string{"--key", "DB_PASSWORD", "--namespace", "argus-inst-i9", "--kubeconfig", kubeconfigPath, "--kube-context", "in-cluster", "--restart"})
	if code != exitOK {
		t.Fatalf("exit=%d output=%s", code, out)
	}
	calls := callsFile(t, dir)
	if len(calls) != 2 {
		t.Fatalf("want 2 kubectl calls (patch + restart), got %d: %v", len(calls), calls)
	}
	want := "--kubeconfig " + kubeconfigPath + " --context in-cluster "
	for i, c := range calls {
		if !strings.HasPrefix(c, want) {
			t.Errorf("kubectl call %d = %q, want it to start with %q", i, c, want)
		}
	}
	if !strings.Contains(out, "kubectl --kubeconfig "+kubeconfigPath+" --context in-cluster rollout restart deployment/executor -n argus-inst-i9") {
		t.Errorf("the printed restart_cmd must carry the kubeconfig and context too:\n%s", out)
	}
}

func TestSecretsKubeconfig_ListPassesFlag(t *testing.T) {
	dir := t.TempDir()
	writeFakeKubectl(t, dir, `{"data":{"A":"YQ=="}}`)

	out, code := runSecretsWithStdin(t, "", cmdSecretsList,
		[]string{"--namespace", "argus-inst-i9", "--kubeconfig", kubeconfigPath})
	if code != exitOK {
		t.Fatalf("exit=%d output=%s", code, out)
	}
	calls := callsFile(t, dir)
	if len(calls) != 1 || !strings.HasPrefix(calls[0], "--kubeconfig "+kubeconfigPath+" get secret exec-tokens") {
		t.Fatalf("kubectl calls = %v, want one starting with --kubeconfig <path> get secret", calls)
	}
}

func TestSecretsKubeconfig_MigratePassesFlagToGetReplaceAndRestart(t *testing.T) {
	dir := t.TempDir()
	writeFakeKubectl(t, dir, `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"exec-tokens"},"data":{"ARGUS_AUTHOR_TOKEN":"dG9r"}}`)

	out, code := runSecretsWithStdin(t, "", cmdSecretsMigrate,
		[]string{"--namespace", "argus-inst-i9", "--kubeconfig", kubeconfigPath, "--restart"})
	if code != exitOK {
		t.Fatalf("exit=%d output=%s", code, out)
	}
	calls := callsFile(t, dir)
	if len(calls) != 3 {
		t.Fatalf("want 3 kubectl calls (get, replace, restart), got %d: %v", len(calls), calls)
	}
	for i, c := range calls {
		if !strings.HasPrefix(c, "--kubeconfig "+kubeconfigPath+" ") {
			t.Errorf("kubectl call %d = %q, want it to start with --kubeconfig <path>", i, c)
		}
	}
}

// Without the flag nothing changes: no stray --kubeconfig, so the default resolution still applies.
func TestSecretsKubeconfig_AbsentFlagAddsNothing(t *testing.T) {
	dir := t.TempDir()
	writeFakeKubectl(t, dir, "")
	_, code := runSecretsWithStdin(t, "v", cmdSecretsSet, []string{"--key", "K", "--namespace", "argus-inst-i9"})
	if code != exitOK {
		t.Fatalf("exit=%d", code)
	}
	for _, c := range callsFile(t, dir) {
		if strings.Contains(c, "--kubeconfig") {
			t.Errorf("kubectl call %q carries --kubeconfig although none was given", c)
		}
	}
}

func TestSecretsUsage_DocumentsKubeconfig(t *testing.T) {
	if !strings.Contains(secretsUsage, "--kubeconfig <path>") || !strings.Contains(secretsUsage, "in-cluster") {
		t.Errorf("secrets help must document --kubeconfig (and why: in-cluster testers):\n%s", secretsUsage)
	}
}
