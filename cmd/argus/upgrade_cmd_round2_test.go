package main

// upgrade_cmd_round2_test.go — the second review round on `argus upgrade` (PR #370): a Service port
// whose number changes, a change the credential mask would otherwise hide, and the wording of the
// Secret line. Same fake kubectl as upgrade_cmd_test.go, which now also refuses a Service that repeats
// a port name, as the API server does.

import (
	"os"
	"strings"
	"testing"
)

// ---- a Service port whose NUMBER changes while its NAME stays -----------------------------------------

// loki's Service is a NodePort on the k3d tier: its port carries a nodePort the server allocated.
func lokiServiceWithAnotherPortNumber(t *testing.T, c *upCluster) {
	t.Helper()
	c.setLive(t, "service", "loki", func(m map[string]any) {
		p := m["spec"].(map[string]any)["ports"].([]any)[0].(map[string]any)
		p["port"] = float64(9999)
		p["nodePort"] = float64(31000)
		p["protocol"] = "TCP"
	})
}

func TestUpgrade_AServicePortWhoseNumberChangesIsPatchedAsAWholeListKeepingItsNodePort(t *testing.T) {
	c := newCluster(t, nil)
	lokiServiceWithAnotherPortNumber(t, c)

	out, _, code := run(t, c.upgradeArgs()...)
	if code != exitOK || !strings.Contains(out, "Service/loki") || !strings.Contains(out, "spec.ports[3100]") {
		t.Fatalf("dry run: exit %d; the port is a difference keyed by its number:\n%s", code, out)
	}

	out, _, code = run(t, c.upgradeArgs("--apply")...)
	if code != exitOK {
		t.Fatalf("--apply exit %d (the server refuses a Service with a duplicate port name):\n%s", code, out)
	}
	var call string
	for _, l := range c.calls(t) {
		if strings.Contains(l, "patch service loki") {
			call = l
		}
	}
	if !strings.Contains(call, "--type=merge") {
		t.Errorf("a Service whose ports differ is patched with a JSON merge patch (it replaces the list): %q", call)
	}
	ports := c.live(t, "service", "loki")["spec"].(map[string]any)["ports"].([]any)
	if len(ports) != 1 {
		t.Fatalf("the Service must hold exactly one port, got %s", toJSON(ports))
	}
	p := ports[0].(map[string]any)
	if p["name"] != "http" || p["port"] != float64(3100) || p["nodePort"] != float64(31000) {
		t.Errorf("want http 3100 with the allocated nodePort 31000 kept, got %s", toJSON(p))
	}

	c.resetCalls()
	out, _, code = run(t, c.upgradeArgs("--apply")...)
	if code != exitOK || !strings.Contains(out, "no differences") {
		t.Errorf("the run after --apply must report no differences (exit %d):\n%s", code, out)
	}
}

// ---- a change that masking would otherwise hide ---------------------------------------------------------

const hiddenCredentialLine = "a credential inside a URL changed and is not shown"

func TestUpgrade_AChangeInsideAURLsCredentialIsSaidNotSilent(t *testing.T) {
	t.Run("config map", func(t *testing.T) {
		c := newCluster(t, nil)
		if err := os.WriteFile(c.cfg, []byte(strings.Replace(upConfig, "https://example.invalid", "https://newuser:newpw@example.invalid", 1)), 0o600); err != nil {
			t.Fatal(err)
		}
		c.setLive(t, "configmap", "argus-config", func(m map[string]any) {
			data := m["data"].(map[string]any)
			for k, v := range data {
				if s, ok := v.(string); ok {
					data[k] = strings.Replace(s, "https://example.invalid", "https://olduser:oldpw@example.invalid", 1)
				}
			}
		})
		out, errOut, code := run(t, c.upgradeArgs()...)
		if code != exitOK {
			t.Fatalf("exit %d:\n%s", code, out)
		}
		if !strings.Contains(out, "ConfigMap/argus-config  DIFFERS") || !strings.Contains(out, hiddenCredentialLine) {
			t.Errorf("a ConfigMap that differs only in a URL's userinfo must say so, not print DIFFERS over nothing:\n%s", out)
		}
		if !strings.Contains(out, "data.") {
			t.Errorf("the line must name the key that changed:\n%s", out)
		}
		for _, leak := range []string{"newuser", "newpw", "olduser", "oldpw"} {
			if strings.Contains(out+errOut, leak) {
				t.Errorf("credential %q reached the output:\n%s", leak, out)
			}
		}
	})
	t.Run("env value", func(t *testing.T) {
		c := newCluster(t, nil)
		t.Setenv("ARGUS_GRAFANA_PUBLIC_URL", "https://newuser:newpw@grafana.example.invalid/d")
		c.setLive(t, "deployment.apps", "executor", func(m map[string]any) {
			cs := m["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)
			c0 := cs[0].(map[string]any)
			c0["env"] = append(c0["env"].([]any), map[string]any{"name": "ARGUS_GRAFANA_PUBLIC_URL", "value": "https://olduser:oldpw@grafana.example.invalid/d"})
		})
		out, errOut, code := run(t, c.upgradeArgs()...)
		if code != exitOK {
			t.Fatalf("exit %d:\n%s", code, out)
		}
		if !strings.Contains(out, hiddenCredentialLine) || !strings.Contains(out, "ARGUS_GRAFANA_PUBLIC_URL") {
			t.Errorf("an env value that differs only in a URL's userinfo must say so and name the variable:\n%s", out)
		}
		for _, leak := range []string{"newuser", "newpw", "olduser", "oldpw"} {
			if strings.Contains(out+errOut, leak) {
				t.Errorf("credential %q reached the output:\n%s", leak, out)
			}
		}
	})
}

// ---- the Secret line says what is true -------------------------------------------------------------------

func TestUpgrade_TheSecretLineSaysContentIsNeverReadAndOnlyExistenceIsChecked(t *testing.T) {
	c := newCluster(t, nil)
	out, _, code := run(t, c.upgradeArgs()...)
	if code != exitOK {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	var line string
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "Secret/exec-tokens") {
			line = l
		}
	}
	for _, want := range []string{"SKIPPED", "content is never read", "existence", "kubectl get secret <name> -o name"} {
		if !strings.Contains(line, want) {
			t.Errorf("the Secret line lacks %q: %q", want, line)
		}
	}
	if strings.Contains(line, "a Secret is never read") {
		t.Errorf("the Secret line still contradicts itself (a Secret's existence IS checked): %q", line)
	}
	_, help := runHelp(t, "upgrade", "--help")
	if !strings.Contains(help, "content is never read") {
		t.Errorf("the help must say a Secret's content is never read:\n%s", help)
	}
}
