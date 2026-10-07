package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/buildinfo"
	"github.com/OneDro1d/argus-runner/internal/doctor"
)

// Build-time default addresses. The wiring test: it BUILDS the real binary twice, once
// with no settings and once with every setting overridden, and reads what each prints. A unit test of the
// variables alone would stay green if one place still carried the old literal.

const buildinfoPkg = "github.com/OneDro1d/argus-runner/internal/buildinfo"

var (
	newCP   = "https://argus.example.org"
	newDocs = "https://docs.example.org"
	newGraf = "https://grafana.example.org"
)

func buildArgusWithHosts(t *testing.T, ldflags string) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go is not on PATH; cannot build the binary under test")
	}
	bin := filepath.Join(t.TempDir(), "argus")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	args := []string{"build"}
	if ldflags != "" {
		args = append(args, "-ldflags", ldflags)
	}
	args = append(args, "-o", bin, ".")
	build := exec.Command("go", args...)
	build.Env = os.Environ()
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

func runArgus(t *testing.T, bin string, args ...string) string {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), "ARGUS_CP_URL=", "ARGUS_CP_AUTHOR_TOKEN=", "ARGUS_CP_TOKEN=")
	out, _ := cmd.CombinedOutput() // several of these exit non-zero by design; the text is what is read
	return string(out)
}

// tierScalarConfig is the old single-value form, which the parser rejects with a message that shows an
// example Grafana host.
func writeTierScalarConfig(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "argus-config.yaml")
	body := "project:\n  name: svc\nobservability:\n  grafana:\n    public_url: http://localhost:3000\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// surfaces is every user-visible place a built-in address reaches, by what is run to see it.
func surfaces(t *testing.T, bin string) map[string]string {
	cfg := writeTierScalarConfig(t)
	return map[string]string{
		"version":         runArgus(t, bin, "version"),
		"tester --help":   runArgus(t, bin, "tester", "--help"),
		"builder --help":  runArgus(t, bin, "builder", "--help"),
		"preflight":       runArgus(t, bin, "preflight", "--tier", "compose"),
		"validate-config": runArgus(t, bin, "validate-config", "--config", cfg),
	}
}

func TestBuildHosts_DefaultBuildPrintsTodaysAddresses(t *testing.T) {
	s := surfaces(t, buildArgusWithHosts(t, ""))
	for surface, want := range map[string]string{
		"version":         "https://argus-dev.onedroid.ai",
		"tester --help":   "else https://argus-dev.onedroid.ai)",
		"builder --help":  "else https://argus-dev.onedroid.ai)",
		"preflight":       "the dev CP is https://argus-dev.onedroid.ai)",
		"validate-config": "managed: https://grafana.example.com",
	} {
		if !strings.Contains(s[surface], want) {
			t.Errorf("default build, %s: want %q in\n%s", surface, want, s[surface])
		}
	}
	if !strings.Contains(s["version"], `"docs": "https://docs.example.com"`) {
		t.Errorf("default build, version: want the docs address in\n%s", s["version"])
	}
}

func TestBuildHosts_OverriddenBuildChangesEverySurface(t *testing.T) {
	flags := "-X " + buildinfoPkg + ".ControlPlaneURL=" + newCP +
		" -X " + buildinfoPkg + ".DocsURL=" + newDocs +
		" -X " + buildinfoPkg + ".GrafanaURL=" + newGraf
	s := surfaces(t, buildArgusWithHosts(t, flags))
	for surface, want := range map[string]string{
		"version":         newCP,
		"tester --help":   "else " + newCP + ")",
		"builder --help":  "else " + newCP + ")",
		"preflight":       "the dev CP is " + newCP + ")",
		"validate-config": "managed: " + newGraf,
	} {
		if !strings.Contains(s[surface], want) {
			t.Errorf("overridden build, %s: want %q in\n%s", surface, want, s[surface])
		}
	}
	if !strings.Contains(s["version"], `"docs": "`+newDocs+`"`) || !strings.Contains(s["version"], `"grafana": "`+newGraf+`"`) {
		t.Errorf("overridden build, version: want the docs and grafana addresses in\n%s", s["version"])
	}
	// Nothing of the old estate may survive in anything the binary printed.
	for surface, out := range s {
		for _, old := range []string{"argus-dev.onedroid.ai", "docs.example.com", "grafana.example.com"} {
			if strings.Contains(out, old) {
				t.Errorf("overridden build, %s still prints %q:\n%s", surface, old, out)
			}
		}
	}
}

// The docs address reaches the tester doctor's fix hint. That path needs a kubectl, so it is read
// in-process with the fake kubectl the other doctor tests use.
func TestBuildHosts_DocsAddressReachesTheDoctorHint(t *testing.T) {
	for _, c := range []struct{ docs, want string }{
		{buildinfo.DocsURL, "docs.example.com/argus-tester-guide"},
		{"https://docs.example.org", "docs.example.org/argus-tester-guide"},
	} {
		old := buildinfo.DocsURL
		buildinfo.DocsURL = c.docs
		cp := newTesterCP(t, plantedToken, authorTools(3), "i", recentSeen())
		fakeKubectlDoctor(t)
		t.Setenv("FAKE_KUBECTL_NS_MISSING", "1")
		env := writeTesterEnv(t, "ARGUS_TESTER_TOKEN_MSGBUS="+plantedToken+"\n")
		rep, _, _, _ := runDoctorTester(t, "--env-file", env, "--control-plane", cp.srv.URL, "--instance", "i")
		buildinfo.DocsURL = old
		ns := statusOf(t, rep, "tester-namespace")
		if ns.Status != doctor.StatusFail || !strings.Contains(ns.Fix, "("+c.want+")") {
			t.Errorf("docs %q: tester-namespace fix = %q, want it to name %q", c.docs, ns.Fix, c.want)
		}
	}
}

// A bad address stops the binary at start, naming the setting. An empty one does too.
func TestBuildHosts_BadAddressStopsTheBinaryAtStart(t *testing.T) {
	for name, flags := range map[string]string{
		"http":  "-X " + buildinfoPkg + ".ControlPlaneURL=http://argus.example.org",
		"empty": "-X " + buildinfoPkg + ".DocsURL=",
	} {
		bin := buildArgusWithHosts(t, flags)
		cmd := exec.Command(bin, "version")
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Errorf("%s: the binary ran; it must refuse to start:\n%s", name, out)
			continue
		}
		want := "ControlPlaneURL"
		if name == "empty" {
			want = "DocsURL"
		}
		if !strings.Contains(string(out), "buildinfo."+want) || strings.Contains(string(out), `"version"`) {
			t.Errorf("%s: want a refusal naming %s and no command output, got\n%s", name, want, out)
		}
	}
}
