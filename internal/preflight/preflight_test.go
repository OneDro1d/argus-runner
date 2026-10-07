package preflight

import (
	"errors"
	"strings"
	"testing"
)

// fakeProbes lets every verdict be tested without a cluster. Each field is either a value or an
// error, so "the probe could not run" is as easy to simulate as "the thing is absent" — which is
// the distinction this package exists to keep.
type fakeProbes struct {
	ctx      string
	ctxErr   error
	classes  []string
	classErr error
	code     int
	reachErr error

	dockerErr error
	ports     []int
	portsErr  error
	holders   map[int]PortHolder
	holderErr error
}

func (f fakeProbes) DockerReady() error               { return f.dockerErr }
func (f fakeProbes) ComposeHostPorts() ([]int, error) { return f.ports, f.portsErr }
func (f fakeProbes) HostPortHolder(p int) (PortHolder, error) {
	return f.holders[p], f.holderErr
}

func (f fakeProbes) CurrentContext() (string, error)         { return f.ctx, f.ctxErr }
func (f fakeProbes) StorageClasses(string) ([]string, error) { return f.classes, f.classErr }
func (f fakeProbes) Reachable(string) (int, error)           { return f.code, f.reachErr }

// stubClassFor stands in for k8srender.StorageDefaultsFor. The REAL mapping is pinned against the
// renderer in internal/k8srender; here we only need a deterministic answer.
func stubClassFor(tier string) (string, string) {
	switch strings.ToLower(tier) {
	case "aks":
		return "azurefile-csi", "ReadWriteMany"
	case "k3d", "kind", "minikube":
		return "argus-rwx", "ReadWriteMany"
	default:
		return "local-path", "ReadWriteOnce"
	}
}

func find(t *testing.T, r Report, id string) Check {
	t.Helper()
	for _, c := range r.Checks {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("no check with id %q in report", id)
	return Check{}
}

func readyInput() Input {
	return Input{
		Tier:            "aks",
		KubeContext:     "example-cluster",
		ControlPlaneURL: "https://argus-dev.onedroid.ai",
		TokenPresent:    true,
		TokenSource:     "ARGUS_CP_TOKEN",
	}
}

func readyProbes() fakeProbes {
	return fakeProbes{
		ctx:     "example-cluster",
		classes: []string{"default", "azurefile-csi", "managed-csi"},
		code:    200,
	}
}

func TestRun_EverythingPresentIsReady(t *testing.T) {
	r := Run(readyInput(), readyProbes(), stubClassFor)
	if r.Verdict != "ready" {
		t.Fatalf("verdict = %q, want ready. Blocking: %v", r.Verdict, r.Blocking)
	}
	if len(r.Blocking) != 0 {
		t.Errorf("Blocking = %v, want empty on a ready report", r.Blocking)
	}
}

// TestRun_AProbeThatCouldNotRunBlocks is the central test of this package. A failed probe must
// never read as ok — an error in the reassuring direction is the one nobody audits.
func TestRun_AProbeThatCouldNotRunBlocks(t *testing.T) {
	for _, tc := range []struct {
		name   string
		probes fakeProbes
		id     string
	}{
		{"kubectl missing", fakeProbes{ctxErr: errors.New("exec: kubectl: not found"), code: 200}, "kube-context"},
		{"cannot list storage classes", func() fakeProbes {
			p := readyProbes()
			p.classErr = errors.New("connection refused")
			return p
		}(), "storage-class"},
		{"cp unreachable", func() fakeProbes {
			p := readyProbes()
			p.reachErr = errors.New("no such host")
			return p
		}(), "control-plane"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := Run(readyInput(), tc.probes, stubClassFor)
			c := find(t, r, tc.id)
			if c.Status != StatusUnknown {
				t.Errorf("status = %q, want %q — a probe that could not run must not report a "+
					"definite answer in either direction", c.Status, StatusUnknown)
			}
			if r.Verdict != "blocked" {
				t.Errorf("verdict = %q, want blocked: an unknown blocking check must block, "+
					"otherwise the caller proceeds on a question nobody answered", r.Verdict)
			}
			if c.Subject == "" {
				t.Errorf("check %q reports no Subject — a verdict without its subject is how a "+
					"check answers a question you did not ask", c.ID)
			}
		})
	}
}

// TestRun_UnknownIsNotMissing keeps the two apart. They have different fixes, and collapsing them
// sends an operator to install a storage class on a machine with no kubectl.
func TestRun_UnknownIsNotMissing(t *testing.T) {
	p := readyProbes()
	p.classErr = errors.New("connection refused")
	unknown := find(t, Run(readyInput(), p, stubClassFor), "storage-class")

	p2 := readyProbes()
	p2.classes = []string{"default"} // real answer, class genuinely absent
	missing := find(t, Run(readyInput(), p2, stubClassFor), "storage-class")

	if unknown.Status == missing.Status {
		t.Fatalf("a failed probe and a genuinely absent class both reported %q", unknown.Status)
	}
	if unknown.Fix == missing.Fix {
		t.Errorf("both statuses offer the same fix %q — they are different problems", unknown.Fix)
	}
}

func TestStorageClass_MissingClassSaysWhyItMatters(t *testing.T) {
	p := readyProbes()
	p.classes = []string{"default", "managed-csi"} // no azurefile-csi
	c := find(t, Run(readyInput(), p, stubClassFor), "storage-class")

	if c.Status != StatusMissing {
		t.Fatalf("status = %q, want missing", c.Status)
	}
	// The whole value of this check is that the eventual symptom does not name the tier.
	if !strings.Contains(c.Detail, "Pending") {
		t.Errorf("detail %q does not tell the operator what the failure will LOOK like "+
			"(a Pending claim and an executor that never goes Ready)", c.Detail)
	}
	if !strings.Contains(c.Subject, "azurefile-csi") || !strings.Contains(c.Subject, "managed-csi") {
		t.Errorf("subject %q must name both what was needed and what was actually found", c.Subject)
	}
}

// TestStorageClass_EmptyTierIsCalledOutEvenWhenItPasses is the quiet one. An empty tier resolves to
// local-path + ReadWriteOnce, which a cluster may well have — so the check goes GREEN while the
// operator gets a single-node pin they never chose.
func TestStorageClass_EmptyTierIsCalledOutEvenWhenItPasses(t *testing.T) {
	in := readyInput()
	in.Tier = ""
	p := readyProbes()
	p.classes = []string{"local-path"}

	c := find(t, Run(in, p, stubClassFor), "storage-class")
	if c.Status != StatusOK {
		t.Fatalf("status = %q, want ok — the class IS present", c.Status)
	}
	if !strings.Contains(c.Detail, "ReadWriteOnce") {
		t.Errorf("an empty tier passed silently. detail = %q, and it must warn that the fallback "+
			"pins every replica to one node", c.Detail)
	}
}

func TestKubeContext_WrongContextIsRefusedWithBothNames(t *testing.T) {
	p := readyProbes()
	p.ctx = "some-other-cluster"
	c := find(t, Run(readyInput(), p, stubClassFor), "kube-context")

	if c.Status != StatusMissing {
		t.Fatalf("status = %q, want missing", c.Status)
	}
	if !strings.Contains(c.Subject, "some-other-cluster") || !strings.Contains(c.Subject, "example-cluster") {
		t.Errorf("subject %q must name BOTH the current and the wanted context — otherwise the "+
			"operator cannot see which way round the mismatch is", c.Subject)
	}
}

// TestControlPlane_AnAuthChallengeIsReachable guards against the opposite error: treating a 401
// as unreachable would send an operator to debug a network that is fine.
func TestControlPlane_AnAuthChallengeIsReachable(t *testing.T) {
	for _, code := range []int{200, 401, 403, 404} {
		p := readyProbes()
		p.code = code
		c := find(t, Run(readyInput(), p, stubClassFor), "control-plane")
		if c.Status != StatusOK {
			t.Errorf("HTTP %d reported %q — any answer proves reachability, which is what this "+
				"check measures", code, c.Status)
		}
	}
	p := readyProbes()
	p.code = 503
	if c := find(t, Run(readyInput(), p, stubClassFor), "control-plane"); c.Status == StatusOK {
		t.Errorf("HTTP 503 reported ok; a server error is not a healthy control plane")
	}
}

// TestToken_NeverEchoesAValue is a standing guard, not a formality: a transcript is stored, so a
// printed credential counts as leaked. Input carries no value to leak BY CONSTRUCTION, and this
// test fails if someone adds one and starts echoing it.
func TestToken_NeverEchoesAValue(t *testing.T) {
	in := readyInput()
	in.TokenSource = "ARGUS_CP_TOKEN"
	r := Run(in, readyProbes(), stubClassFor)
	c := find(t, r, "control-plane-token")

	if !strings.Contains(c.Subject, "ARGUS_CP_TOKEN") {
		t.Errorf("subject %q should name WHERE the token was looked for, so a missing one is actionable", c.Subject)
	}
	// A token check that claims validity would be lying: presence is all it establishes.
	if !strings.Contains(c.Detail, "NOT prove it is valid") {
		t.Errorf("detail %q must not imply the token was validated — only that one was found", c.Detail)
	}
}

func TestToken_MissingBlocksAndNamesTheFix(t *testing.T) {
	in := readyInput()
	in.TokenPresent = false
	r := Run(in, readyProbes(), stubClassFor)
	c := find(t, r, "control-plane-token")

	if c.Status != StatusMissing {
		t.Fatalf("status = %q, want missing", c.Status)
	}
	if !strings.Contains(c.Fix, "cloud-login") {
		t.Errorf("fix = %q, want the literal command that obtains one", c.Fix)
	}
	if r.Verdict != "blocked" {
		t.Errorf("verdict = %q, want blocked", r.Verdict)
	}
}

// TestRun_ReportsEveryProblemAtOnce: an operator fixing one thing per round-trip is exactly the
// attention cost this command removes.
func TestRun_ReportsEveryProblemAtOnce(t *testing.T) {
	in := readyInput()
	in.TokenPresent = false
	in.ControlPlaneURL = ""
	p := readyProbes()
	p.ctx = "wrong-cluster"
	p.classes = []string{"default"}

	r := Run(in, p, stubClassFor)
	if got := len(r.Blocking); got < 4 {
		t.Fatalf("Blocking = %v (%d); all four problems must be reported in one pass, not "+
			"discovered one round-trip at a time", r.Blocking, got)
	}
	for _, c := range r.Checks {
		if c.Status == StatusOK {
			continue
		}
		if c.Fix == "" {
			t.Errorf("check %q is %q but offers no Fix — T2.3's acceptance is that an agent can "+
				"either fix or report PRECISELY, and a verdict with no next command is neither",
				c.ID, c.Status)
		}
	}
}

// TestRun_ATierTheControlPlaneRefusesBlocks: preflight said `ready` to `--tier k3s` on 2026-09-23, and
// the agent learned otherwise from `register: 500 invalid tier "k3s"`, with its executor already
// running. The cluster here is otherwise ready and HAS the class the tier resolves to, so the tier
// check is the only thing that can block.
func TestRun_ATierTheControlPlaneRefusesBlocks(t *testing.T) {
	for _, tier := range []string{"k3s", "kind", "minikube"} {
		t.Run(tier, func(t *testing.T) {
			in := readyInput()
			in.Tier = tier
			p := readyProbes()
			p.classes = []string{"argus-rwx", "local-path"}

			r := Run(in, p, stubClassFor)
			c := find(t, r, "tier")
			if c.Status != StatusMissing {
				t.Fatalf("tier check = %q, want %q for %q", c.Status, StatusMissing, tier)
			}
			if r.Verdict != "blocked" {
				t.Errorf("verdict = %q, want blocked: this tier deploys and is then refused at registration", r.Verdict)
			}
			if !strings.Contains(c.Fix, "--tier managed") {
				t.Errorf("Fix = %q — it must name the tier to use instead", c.Fix)
			}
		})
	}
}

// TestRun_ARegistrableTierPassesTheTierCheck keeps the guard from refusing the tiers that work; empty
// stays the storage-class check's business, as it was.
func TestRun_ARegistrableTierPassesTheTierCheck(t *testing.T) {
	for _, tier := range []string{"aks", "k3d", "managed", "eks", ""} {
		in := readyInput()
		in.Tier = tier
		if c := find(t, Run(in, readyProbes(), stubClassFor), "tier"); c.Status != StatusOK {
			t.Errorf("tier %q: tier check = %q (%s), want ok", tier, c.Status, c.Detail)
		}
	}
}
