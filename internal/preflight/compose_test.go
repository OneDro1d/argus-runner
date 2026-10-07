package preflight

import (
	"errors"
	"strings"
	"testing"
)

// the compose tier (no --kube-context) asks whether Docker works and whether the host
// ports its stack publishes are free. Until now it reported `ready` without asking either.

func composeInput() Input {
	in := readyInput()
	in.KubeContext = ""
	return in
}

func composeProbes() fakeProbes {
	p := readyProbes()
	p.ctx = "whatever-is-current"
	p.ports = []int{3000, 9095}
	return p
}

func TestCompose_ReadyWhenDockerWorksAndPortsAreFree(t *testing.T) {
	r := Run(composeInput(), composeProbes(), stubClassFor)
	for _, id := range []string{"docker", "compose-host-ports"} {
		c := find(t, r, id)
		if c.Status != StatusOK {
			t.Errorf("%s = %q (%s), want ok", id, c.Status, c.Detail)
		}
		if c.Subject == "" {
			t.Errorf("%s has no Subject", id)
		}
	}
	if r.Verdict != "ready" {
		t.Errorf("verdict = %q, want ready. Blocking: %v", r.Verdict, r.Blocking)
	}
	if s := find(t, r, "compose-host-ports").Subject; !strings.Contains(s, "3000") || !strings.Contains(s, "9095") {
		t.Errorf("subject %q must name the ports that were examined", s)
	}
}

func TestCompose_DockerThatCannotBeAskedBlocksAsUnknown(t *testing.T) {
	p := composeProbes()
	p.dockerErr = errors.New("Cannot connect to the Docker daemon")
	r := Run(composeInput(), p, stubClassFor)
	c := find(t, r, "docker")
	if c.Status != StatusUnknown {
		t.Fatalf("docker = %q, want %q", c.Status, StatusUnknown)
	}
	if r.Verdict != "blocked" {
		t.Errorf("verdict = %q, want blocked — a compose deploy with no working Docker was reported ready", r.Verdict)
	}
	if !strings.Contains(c.Fix, "docker info") {
		t.Errorf("Fix = %q, want a literal command that confirms the cure (docker info)", c.Fix)
	}
	if c.Subject == "" {
		t.Error("no Subject")
	}
}

func TestCompose_PortsThatCouldNotBeReadBlockAsUnknown(t *testing.T) {
	for name, mut := range map[string]func(*fakeProbes){
		"compose config unreadable": func(p *fakeProbes) { p.portsErr = errors.New("no such file") },
		"no ports read at all":      func(p *fakeProbes) { p.ports = nil },
		"holder lookup failed":      func(p *fakeProbes) { p.holderErr = errors.New("docker ps failed") },
	} {
		t.Run(name, func(t *testing.T) {
			p := composeProbes()
			mut(&p)
			r := Run(composeInput(), p, stubClassFor)
			c := find(t, r, "compose-host-ports")
			if c.Status != StatusUnknown {
				t.Fatalf("status = %q, want %q — an absent signal must not read as free ports", c.Status, StatusUnknown)
			}
			if r.Verdict != "blocked" || c.Subject == "" || c.Fix == "" {
				t.Errorf("verdict=%q subject=%q fix=%q — must block, and say what and how", r.Verdict, c.Subject, c.Fix)
			}
		})
	}
}

func TestCompose_EveryTakenPortIsReportedAtOnceWithItsHolder(t *testing.T) {
	p := composeProbes()
	p.holders = map[int]PortHolder{
		3000: {Held: true, Container: "leftover-grafana", Project: "otherstack"},
		9095: {Held: true}, // a process outside Docker
	}
	r := Run(composeInput(), p, stubClassFor)
	c := find(t, r, "compose-host-ports")
	if c.Status != StatusMissing {
		t.Fatalf("status = %q, want missing", c.Status)
	}
	if r.Verdict != "blocked" {
		t.Errorf("verdict = %q, want blocked", r.Verdict)
	}
	for _, want := range []string{"3000", "leftover-grafana", "9095"} {
		if !strings.Contains(c.Subject, want) {
			t.Errorf("subject %q does not name %q", c.Subject, want)
		}
	}
	for _, want := range []string{"docker stop leftover-grafana", "lsof -nP -iTCP:9095"} {
		if !strings.Contains(c.Fix, want) {
			t.Errorf("Fix %q lacks the literal command %q", c.Fix, want)
		}
	}
}

func TestCompose_AContainerOfTheStacksOwnProjectIsAReonboardNotAConflict(t *testing.T) {
	p := composeProbes()
	p.holders = map[int]PortHolder{3000: {Held: true, Container: "argus-obs-grafana-1", Project: "argus-obs"}}
	r := Run(composeInput(), p, stubClassFor)
	if c := find(t, r, "compose-host-ports"); c.Status != StatusOK {
		t.Errorf("status = %q (%s) — a re-onboard was refused by its own stack", c.Status, c.Subject)
	}
}

func TestCompose_ChecksDoNotBlockOnAKubernetesTier(t *testing.T) {
	p := readyProbes() // kube context given
	p.dockerErr = errors.New("no docker here")
	p.portsErr = errors.New("no kit here")
	p.holders = map[int]PortHolder{3000: {Held: true, Container: "x"}}
	r := Run(readyInput(), p, stubClassFor)
	if r.Verdict != "ready" {
		t.Fatalf("verdict = %q, blocking %v — a k8s tier does not run the compose stack", r.Verdict, r.Blocking)
	}
}
