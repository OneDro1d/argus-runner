package main

// upgrade_cmd_fixes_test.go — the review round on `argus upgrade` (PR #370): create what the newer kit
// adds, refuse an identity change, say which cluster, precise NotFound, credential masking, the API
// server's list merge keys, no image in the executor patch, a run that fails half way, the two
// instance guards, and a truthful help. Same fake kubectl as upgrade_cmd_test.go.

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func (c *upCluster) creates() string {
	b, _ := os.ReadFile(filepath.Join(c.dir, "creates.txt"))
	return string(b)
}

// callIndex is the position of the first recorded kubectl call that contains all of the fragments.
func (c *upCluster) callIndex(t *testing.T, frags ...string) int {
	t.Helper()
outer:
	for i, call := range c.calls(t) {
		for _, f := range frags {
			if !strings.Contains(call, f) {
				continue outer
			}
		}
		return i
	}
	return -1
}

// lokiGainsAConfigMap models the real case: the newer kit's Deployment/loki mounts ConfigMap/loki-config,
// which the older instance never had (no ConfigMap, no volume for it).
func lokiGainsAConfigMap(t *testing.T, c *upCluster) {
	t.Helper()
	c.remove("configmap", "loki-config")
	c.setLive(t, "deployment.apps", "loki", func(m map[string]any) {
		pod := m["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
		var kept []any
		for _, v := range pod["volumes"].([]any) {
			if v.(map[string]any)["name"] != "config" {
				kept = append(kept, v)
			}
		}
		pod["volumes"] = kept
	})
}

// ---- fix 1: create what the newer kit adds -------------------------------------------------------------

func TestUpgrade_DryRunSaysAMissingObjectWouldBeCreated(t *testing.T) {
	c := newCluster(t, nil)
	lokiGainsAConfigMap(t, c)
	out, _, code := run(t, c.upgradeArgs()...)
	if code != exitOK {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if !strings.Contains(out, "ConfigMap/loki-config") || !strings.Contains(out, "would be created") {
		t.Errorf("a missing non-Secret object must print as \"would be created\":\n%s", out)
	}
	onlyGets(t, c.calls(t))
	if c.creates() != "" {
		t.Errorf("a dry run created something:\n%s", c.creates())
	}
}

func TestUpgrade_ApplyCreatesWhatTheNewerKitAddsBeforeAnyWorkloadIsPatched(t *testing.T) {
	c := newCluster(t, nil)
	lokiGainsAConfigMap(t, c)
	out, _, code := run(t, c.upgradeArgs("--apply")...)
	if code != exitOK {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	create := c.callIndex(t, "create -f -")
	patch := c.callIndex(t, "patch deployment.apps loki")
	if create < 0 || patch < 0 || create > patch {
		t.Fatalf("the ConfigMap must be created (call %d) before Deployment/loki is patched (call %d):\n%v", create, patch, c.calls(t))
	}
	if !strings.Contains(c.creates(), "configmap/loki-config") || !strings.Contains(c.creates(), `"kind":"ConfigMap"`) {
		t.Errorf("the ConfigMap was not created from the render:\n%s", c.creates())
	}
	if strings.Contains(c.creates(), "$retainKeys") {
		t.Errorf("a create body is the plain object, not a patch:\n%s", c.creates())
	}
	for _, call := range c.calls(t) {
		if verbOf(call) == "apply" || verbOf(call) == "replace" || verbOf(call) == "delete" {
			t.Errorf("unexpected write verb: %q", call)
		}
	}
	if !strings.Contains(out, "created") {
		t.Errorf("the report must say what was created:\n%s", out)
	}
	// second run: nothing left to do
	c.resetCalls()
	out, _, code = run(t, c.upgradeArgs("--apply")...)
	if code != exitOK || !strings.Contains(out, "no differences") {
		t.Errorf("second run exit %d, want no differences:\n%s", code, out)
	}
}

func TestUpgrade_ACreateThatFailsStopsTheRunLikeAFailedPatch(t *testing.T) {
	c := newCluster(t, nil)
	lokiGainsAConfigMap(t, c)
	t.Setenv("STUB_FAIL", "create:configmap/loki-config")
	out, _, code := run(t, c.upgradeArgs("--apply")...)
	if code != exitErr {
		t.Fatalf("exit %d, want %d:\n%s", code, exitErr, out)
	}
	if c.patches() != "" {
		t.Errorf("nothing may be patched after a failed create:\n%s", c.patches())
	}
	if !strings.Contains(out, "FAILED") || !strings.Contains(out, "ConfigMap/loki-config") || !strings.Contains(out, "NOT ATTEMPTED") {
		t.Errorf("the report must name the failed create and mark the rest not attempted:\n%s", out)
	}
}

func TestUpgrade_AMissingSecretIsNamedNeverCreatedAndItsWorkloadIsNotPatched(t *testing.T) {
	for _, mode := range [][]string{nil, {"--apply"}} {
		c := newCluster(t, nil)
		newerKit(t, c)
		c.remove("secret", "exec-tokens")
		out, _, code := run(t, c.upgradeArgs(mode...)...)
		if code != exitFailed {
			t.Fatalf("mode %v: exit %d, want %d:\n%s", mode, code, exitFailed, out)
		}
		for _, want := range []string{"Secret/exec-tokens", "argus secrets", "Deployment/executor", "NOT patched"} {
			if !strings.Contains(out, want) {
				t.Errorf("mode %v: output lacks %q:\n%s", mode, want, out)
			}
		}
		if strings.Contains(c.patches(), "deployment.apps/executor") {
			t.Errorf("mode %v: the workload that needs the missing Secret was patched:\n%s", mode, c.patches())
		}
		if c.creates() != "" {
			t.Errorf("mode %v: something was created (a Secret must never be):\n%s", mode, c.creates())
		}
		for _, call := range c.calls(t) {
			if strings.Contains(strings.ToLower(call), "secret") && !secretExistsCall.MatchString(call) {
				t.Errorf("mode %v: a Secret was asked about other than by existence: %q", mode, call)
			}
		}
		if len(mode) == 1 && !strings.Contains(c.patches(), "configmap/argus-config") {
			t.Errorf("the objects that do not depend on the missing Secret must still be patched:\n%s", c.patches())
		}
	}
}

func TestUpgrade_AKindTheServerDoesNotHaveIsSkippedAndDoesNotAbort(t *testing.T) {
	c := newCluster(t, nil, "--tier", "managed", "--podmonitor", "on")
	t.Setenv("STUB_NOKIND", "podmonitor.monitoring.coreos.com")
	for _, mode := range [][]string{nil, {"--apply"}} {
		c.resetCalls()
		out, _, code := run(t, c.upgradeArgs(append(mode, "--tier", "managed", "--podmonitor", "on")...)...)
		if code != exitOK {
			t.Fatalf("mode %v: exit %d:\n%s", mode, code, out)
		}
		if !strings.Contains(out, "PodMonitor/") || !strings.Contains(out, "kind not installed in this cluster — skipped") {
			t.Errorf("mode %v: the missing kind must be reported as skipped:\n%s", mode, out)
		}
		if c.callIndex(t, "create -f -") >= 0 {
			t.Errorf("mode %v: a create was attempted for a kind the cluster lacks", mode)
		}
	}
}

// ---- fix 2: refuse an identity change ------------------------------------------------------------------

func TestUpgrade_RefusesAnIdentityChange(t *testing.T) {
	cases := []struct {
		name    string
		oldEnv  map[string]string
		oldArgs []string
		pre     func(*testing.T)
		want    []string
	}{
		{"cp url", map[string]string{"ARGUS_CP_URL": "https://old-cp.example.invalid"}, nil, nil,
			[]string{"ARGUS_CP_URL", "https://old-cp.example.invalid", "https://cp.example.invalid"}},
		{"workspace", map[string]string{"ARGUS_WORKSPACE_ID": "ws_old"}, nil, nil,
			[]string{"ARGUS_WORKSPACE_ID", "ws_old", "ws_test"}},
		{"tier", nil, []string{"--tier", "managed"}, nil,
			[]string{"ARGUS_TIER", "argus.onedroid.ai/tier", "managed", "k3d"}},
		{"sut namespace", nil, []string{"--sut-namespace", "old-sut"}, nil,
			[]string{"ARGUS_SUT_NAMESPACE", "argus.onedroid.ai/sut-namespace", "old-sut", "sut"}},
		{"kube context", map[string]string{"ARGUS_KUBE_CONTEXT_HOST": "ctx-old"}, nil,
			func(t *testing.T) { t.Setenv("ARGUS_KUBE_CONTEXT_HOST", "ctx-new") },
			[]string{"ARGUS_KUBE_CONTEXT_HOST", "ctx-old", "ctx-new"}},
		{"two fields at once", map[string]string{"ARGUS_CP_URL": "https://old-cp.example.invalid", "ARGUS_WORKSPACE_ID": "ws_old"}, nil, nil,
			[]string{"ARGUS_CP_URL", "ARGUS_WORKSPACE_ID", "ws_old", "https://old-cp.example.invalid"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newCluster(t, tc.oldEnv, tc.oldArgs...)
			if tc.pre != nil {
				tc.pre(t)
			}
			for _, mode := range [][]string{nil, {"--apply"}} {
				c.resetCalls()
				out, _, code := run(t, c.upgradeArgs(mode...)...)
				if code != exitDenied {
					t.Fatalf("mode %v: exit %d, want %d (refused):\n%s", mode, code, exitDenied, out)
				}
				for _, w := range append(tc.want, "--accept-identity-change", "onboarded with") {
					if !strings.Contains(out, w) {
						t.Errorf("mode %v: the refusal lacks %q:\n%s", mode, w, out)
					}
				}
				if c.patches() != "" || c.creates() != "" {
					t.Errorf("mode %v: a refusal must write nothing", mode)
				}
				onlyGets(t, c.calls(t))
			}
		})
	}
}

func TestUpgrade_AddingAnIdentityValueTheLiveObjectLacksIsAllowed(t *testing.T) {
	c := newCluster(t, nil) // onboarded with no kube context
	t.Setenv("ARGUS_KUBE_CONTEXT_HOST", "ctx-new")
	out, _, code := run(t, c.upgradeArgs()...)
	if code != exitOK {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if !strings.Contains(out, "ARGUS_KUBE_CONTEXT_HOST") || !strings.Contains(out, "DIFFERS") {
		t.Errorf("an added identity value is a normal difference:\n%s", out)
	}
}

func TestUpgrade_AcceptIdentityChangeTurnsTheRefusalOff(t *testing.T) {
	c := newCluster(t, map[string]string{"ARGUS_CP_URL": "https://old-cp.example.invalid"})
	out, _, code := run(t, c.upgradeArgs("--accept-identity-change", "--apply")...)
	if code != exitOK {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if !strings.Contains(out, "ARGUS_CP_URL") || !strings.Contains(c.patches(), "https://cp.example.invalid") {
		t.Errorf("with the flag the change is shown and applied:\n%s\n%s", out, c.patches())
	}
}

// ---- fix 3: say which cluster --------------------------------------------------------------------------

func TestUpgrade_HeaderNamesTheKubeContextAndAPIServer(t *testing.T) {
	c := newCluster(t, nil)
	out, _, _ := run(t, c.upgradeArgs()...)
	if !strings.Contains(out, "stub-ctx") || !strings.Contains(out, "https://stub.example.invalid:6443") {
		t.Errorf("the output must name the kube context and API server:\n%s", out)
	}
	if c.callIndex(t, "config view --minify") < 0 {
		t.Errorf("the cluster is read with `kubectl config view --minify`: %v", c.calls(t))
	}
	out, _, _ = run(t, c.upgradeArgs("--kube-context", "chosen-ctx")...)
	if !strings.Contains(out, "chosen-ctx") {
		t.Errorf("--kube-context must show as the context:\n%s", out)
	}
	// and a refusal says it too: "not found" means nothing without knowing which cluster was asked
	c.remove("namespace", upNS)
	out, _, code := run(t, c.upgradeArgs()...)
	if code != exitDenied || !strings.Contains(out, "stub-ctx") {
		t.Errorf("a refusal must name the cluster (exit %d):\n%s", code, out)
	}
}

// ---- fix 4: only the API server's NotFound means absent ------------------------------------------------

func TestUpgrade_OnlyTheServersNotFoundMeansAbsent(t *testing.T) {
	c := newCluster(t, nil)
	t.Setenv("STUB_GET_ERROR", `error: context "nope" not found in kubeconfig`)
	out, _, code := run(t, c.upgradeArgs()...)
	if code != exitErr {
		t.Fatalf("exit %d, want %d (an error, not \"absent\"):\n%s", code, exitErr, out)
	}
	if !strings.Contains(out, `context \"nope\"`) && !strings.Contains(out, `context "nope"`) {
		t.Errorf("the error must carry kubectl's own text:\n%s", out)
	}
	if strings.Contains(out, "onboard") {
		t.Errorf("a kubectl error must not read as \"namespace not found — onboard first\":\n%s", out)
	}
}

func TestUpgrade_AMissingKubectlIsAnErrorNotAMissingNamespace(t *testing.T) {
	c := newCluster(t, nil)
	t.Setenv("PATH", t.TempDir()) // no kubectl anywhere
	out, _, code := run(t, c.upgradeArgs()...)
	if code != exitErr || !strings.Contains(out, "could not execute kubectl") || strings.Contains(out, "onboard") {
		t.Errorf("exit %d, want %d with kubectl's own error and no \"onboard first\":\n%s", code, exitErr, out)
	}
}

// ---- fix 5: the leak test must be able to fail ---------------------------------------------------------

func TestUpgrade_TheLeakNeedlesAreRealValues(t *testing.T) {
	for _, tok := range []string{upRunnerTok, upAuthorTok, upEnrollTok, upIdentOldB64, upIdentNewB64} {
		if len(tok) < 12 || strings.Contains(strings.ToUpper(tok), "REDACTED") || strings.HasPrefix(tok, "[") {
			t.Errorf("needle %q is not a distinctive value", tok)
		}
	}
	// The two identity keys are what the leak test searches for after an upgrade that changes the
	// key, so they must differ from each other and be well-formed base64 (the environment variable
	// that carries them is decoded by the render).
	if upIdentOldB64 == upIdentNewB64 {
		t.Errorf("the old and the new identity key needles are the same value")
	}
	for _, k := range []string{upIdentOldB64, upIdentNewB64} {
		if b, err := base64.StdEncoding.DecodeString(k); err != nil || len(b) == 0 {
			t.Errorf("identity key needle is not base64: %v", err)
		}
	}
	upEnv(t)
	if os.Getenv("ARGUS_RUNNER_TOKEN") != upRunnerTok || os.Getenv("ARGUS_AUTHOR_TOKEN") != upAuthorTok || os.Getenv("ARGUS_ENROLLMENT_TOKEN") != upEnrollTok {
		t.Errorf("the environment the run sees must hold exactly the needle values")
	}
}

// ---- fix 6: mask credentials in printed values ---------------------------------------------------------

func TestUpgrade_MasksUserinfoInPrintedURLsUnderAnInnocuousName(t *testing.T) {
	c := newCluster(t, nil)
	newerKit(t, c)
	t.Setenv("ARGUS_GRAFANA_PUBLIC_URL", "https://newuser:newpw@grafana.example.invalid/d")
	if err := os.WriteFile(c.cfg, []byte(strings.Replace(upConfig, "https://example.invalid", "https://cfguser:cfgpw@example.invalid", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	c.setLive(t, "deployment.apps", "executor", func(m map[string]any) {
		cs := m["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)
		c0 := cs[0].(map[string]any)
		c0["env"] = append(c0["env"].([]any), map[string]any{"name": "ARGUS_GRAFANA_PUBLIC_URL", "value": "https://olduser:oldpw@old-grafana.example.invalid/d"})
	})
	out, errOut, code := run(t, c.upgradeArgs()...)
	if code != exitOK {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	for _, leak := range []string{"newuser", "newpw", "olduser", "oldpw", "cfguser", "cfgpw"} {
		if strings.Contains(out+errOut, leak) {
			t.Errorf("credential %q reached the output:\n%s", leak, out)
		}
	}
	if !strings.Contains(out, "grafana.example.invalid") {
		t.Errorf("the host must still be shown:\n%s", out)
	}
}

// ---- fix 7: the server's list merge keys ---------------------------------------------------------------

func TestUpgrade_ListsMergeOnTheServersKeysSoASecondRunIsClean(t *testing.T) {
	cases := []struct {
		name   string
		res    string
		obj    string
		mutate func(m map[string]any)
		want   string
	}{
		{"service port number", "service", "executor", func(m map[string]any) {
			m["spec"].(map[string]any)["ports"].([]any)[0].(map[string]any)["port"] = float64(9999)
		}, "spec.ports[8080]"},
		{"container port number", "deployment.apps", "executor", func(m map[string]any) {
			c0 := m["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
			c0["ports"].([]any)[0].(map[string]any)["containerPort"] = float64(9090)
		}, "ports[8080]"},
		{"volume mount path", "deployment.apps", "executor", func(m map[string]any) {
			c0 := m["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
			c0["volumeMounts"].([]any)[0].(map[string]any)["mountPath"] = "/old-results"
		}, "volumeMounts[/results]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newCluster(t, nil)
			c.setLive(t, tc.res, tc.obj, tc.mutate)
			out, _, code := run(t, c.upgradeArgs()...)
			if code != exitOK || !strings.Contains(out, tc.want) {
				t.Fatalf("exit %d; the difference must be keyed as %q:\n%s", code, tc.want, out)
			}
			if _, _, code := run(t, c.upgradeArgs("--apply")...); code != exitOK {
				t.Fatalf("apply exit %d", code)
			}
			out, _, code = run(t, c.upgradeArgs("--apply")...)
			if code != exitOK || !strings.Contains(out, "no differences") {
				t.Errorf("the second run after --apply must report no differences (exit %d):\n%s", code, out)
			}
		})
	}
}

// ---- fix 8: no image in the executor patch -------------------------------------------------------------

func TestUpgrade_TheExecutorPatchBodyCarriesNoImage(t *testing.T) {
	c := newCluster(t, nil)
	newerKit(t, c)
	if _, _, code := run(t, c.upgradeArgs("--apply")...); code != exitOK {
		t.Fatalf("exit %d", code)
	}
	var body string
	for _, l := range strings.Split(c.patches(), "\n") {
		if strings.HasPrefix(l, "deployment.apps/executor ") {
			body = strings.TrimPrefix(l, "deployment.apps/executor ")
		}
	}
	if body == "" {
		t.Fatalf("no patch for the executor Deployment:\n%s", c.patches())
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatal(err)
	}
	cs := doc["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)
	for _, x := range cs {
		if _, has := x.(map[string]any)["image"]; has {
			t.Errorf("the executor container in the patch carries `image` (an update landing between the read and the patch would be reverted):\n%s", body)
		}
	}
	if strings.Contains(body, upOldDigest) {
		t.Errorf("the running image value is in the patch body:\n%s", body)
	}
}

// ---- fix 9: a run that fails half way ------------------------------------------------------------------

func TestUpgrade_APatchThatFailsMidwayIsReportedAndTheRestIsNotAttempted(t *testing.T) {
	c := newCluster(t, nil)
	newerKit(t, c)
	c.setLive(t, "deployment.apps", "loki", func(m map[string]any) { m["spec"].(map[string]any)["replicas"] = float64(5) })
	t.Setenv("STUB_FAIL", "patch:deployment.apps/executor")
	out, _, code := run(t, c.upgradeArgs("--apply")...)
	if code != exitErr {
		t.Fatalf("exit %d, want %d:\n%s", code, exitErr, out)
	}
	if !strings.Contains(c.patches(), "configmap/argus-config") || strings.Contains(c.patches(), "deployment.apps/loki") {
		t.Errorf("want the ConfigMap patched, the executor failed and Deployment/loki untouched:\n%s", c.patches())
	}
	var patchedLine, failedLine, notAttempted bool
	for _, l := range strings.Split(out, "\n") {
		patchedLine = patchedLine || (strings.Contains(l, "patched") && strings.Contains(l, "ConfigMap/argus-config"))
		failedLine = failedLine || (strings.Contains(l, "FAILED") && strings.Contains(l, "Deployment/executor"))
		notAttempted = notAttempted || (strings.Contains(l, "NOT ATTEMPTED") && strings.Contains(l, "Deployment/loki"))
	}
	if !patchedLine || !failedLine || !notAttempted {
		t.Errorf("the output must name what changed (%v), what failed (%v), and mark what was not attempted (%v):\n%s", patchedLine, failedLine, notAttempted, out)
	}
	if !strings.Contains(out, "not attempted") {
		t.Errorf("the summary must say the rest was not attempted:\n%s", out)
	}
}

// ---- fix 10: the two instance guards ------------------------------------------------------------------

func TestUpgrade_AnInstanceOnboardedByAnOlderKitHasNoNamespaceLabelAndIsAccepted(t *testing.T) {
	c := newCluster(t, nil)
	c.setLive(t, "namespace", upNS, func(m map[string]any) {
		delete(m["metadata"].(map[string]any)["labels"].(map[string]any), "argus.onedroid.ai/instance")
	})
	out, _, code := run(t, c.upgradeArgs()...)
	if code != exitOK {
		t.Fatalf("a Deployment that carries the label is enough (exit %d):\n%s", code, out)
	}
}

func TestUpgrade_RefusesWhenNeitherALabelNorTheEnvSaysWhichInstance(t *testing.T) {
	c := newCluster(t, nil)
	c.setLive(t, "namespace", upNS, func(m map[string]any) {
		delete(m["metadata"].(map[string]any)["labels"].(map[string]any), "argus.onedroid.ai/instance")
	})
	c.setLive(t, "deployment.apps", "executor", func(m map[string]any) {
		delete(m["metadata"].(map[string]any)["labels"].(map[string]any), "argus.onedroid.ai/instance")
		cs := m["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)
		c0 := cs[0].(map[string]any)
		var kept []any
		for _, e := range c0["env"].([]any) {
			if e.(map[string]any)["name"] != "ARGUS_INSTANCE_ID" {
				kept = append(kept, e)
			}
		}
		c0["env"] = kept
	})
	out, _, code := run(t, c.upgradeArgs("--apply")...)
	if code != exitDenied || !strings.Contains(out, "cannot confirm") {
		t.Fatalf("exit %d, want %d:\n%s", code, exitDenied, out)
	}
	if c.patches() != "" {
		t.Error("a refusal must change nothing")
	}
}

// ---- fix 1 and 11: the help says what is true ----------------------------------------------------------

func TestUpgradeUsage_IsTruthfulAboutCreatingAndAboutTheConfigsVariables(t *testing.T) {
	_, out := runHelp(t, "upgrade", "--help")
	for _, want := range []string{"${VAR}", "never written or printed", "kubectl create", "never removes", "--accept-identity-change"} {
		if !strings.Contains(out, want) {
			t.Errorf("upgrade --help lacks %q:\n%s", want, out)
		}
	}
	for _, gone := range []string{"never creates an object", "is even held", "Patches only"} {
		if strings.Contains(out, gone) {
			t.Errorf("upgrade --help still says %q:\n%s", gone, out)
		}
	}
}
