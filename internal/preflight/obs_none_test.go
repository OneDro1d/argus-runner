package preflight

import (
	"errors"
	"strings"
	"testing"
)

// `--obs none` starts no shared argus-obs stack (UI-2), so the host ports
// that stack publishes are not this operator's business — their own Grafana may legitimately hold :3000.

// countingProbes records whether the port probes were consulted at all.
type countingProbes struct {
	fakeProbes
	portsRead    *int
	holdersAsked *int
}

func (c countingProbes) ComposeHostPorts() ([]int, error) {
	*c.portsRead++
	return c.fakeProbes.ComposeHostPorts()
}

func (c countingProbes) HostPortHolder(p int) (PortHolder, error) {
	*c.holdersAsked++
	return c.fakeProbes.HostPortHolder(p)
}

func TestCompose_ObsNoneChecksNoSharedObsPorts(t *testing.T) {
	var read, asked int
	p := composeProbes()
	// The operator's OWN Grafana holds 3000, and the kit files could not even be read: neither may matter.
	p.holders = map[int]PortHolder{3000: {Held: true, Container: "my-grafana"}}
	p.portsErr = errors.New("no kit here")
	in := composeInput()
	in.Obs = "none"
	r := Run(in, countingProbes{fakeProbes: p, portsRead: &read, holdersAsked: &asked}, stubClassFor)
	c := find(t, r, "compose-host-ports")
	if c.Status != StatusOK {
		t.Fatalf("compose-host-ports = %q (%s), want ok under --obs none", c.Status, c.Subject)
	}
	if !strings.Contains(c.Subject, "not applicable: --obs none starts no shared obs stack") {
		t.Errorf("subject %q must say why the check did not apply", c.Subject)
	}
	if read != 0 || asked != 0 {
		t.Errorf("port probes ran under --obs none: ComposeHostPorts x%d, HostPortHolder x%d", read, asked)
	}
	if r.Verdict != "ready" {
		t.Errorf("verdict = %q, blocking %v", r.Verdict, r.Blocking)
	}
}

func TestCompose_ObsNoneStillChecksDocker(t *testing.T) {
	p := composeProbes()
	p.dockerErr = errors.New("daemon down")
	in := composeInput()
	in.Obs = "none"
	r := Run(in, p, stubClassFor)
	if c := find(t, r, "docker"); c.Status != StatusUnknown {
		t.Fatalf("docker = %q, want unknown — --obs none does not remove the compose tier's need for Docker", c.Status)
	}
	if r.Verdict != "blocked" {
		t.Errorf("verdict = %q, want blocked", r.Verdict)
	}
}

func TestCompose_ObsBundledOrEmptyStillChecksPorts(t *testing.T) {
	for _, mode := range []string{"", "bundled"} {
		p := composeProbes()
		p.holders = map[int]PortHolder{3000: {Held: true, Container: "my-grafana"}}
		in := composeInput()
		in.Obs = mode
		r := Run(in, p, stubClassFor)
		if c := find(t, r, "compose-host-ports"); c.Status != StatusMissing {
			t.Errorf("--obs %q: compose-host-ports = %q, want missing (bundled behaviour is unchanged)", mode, c.Status)
		}
	}
}

func TestValidateObs(t *testing.T) {
	for _, ok := range []string{"", "bundled", "adopt", "export", "shared", "none"} {
		if err := ValidateObs(ok); err != nil {
			t.Errorf("ValidateObs(%q) = %v, want nil", ok, err)
		}
	}
	err := ValidateObs("grafana")
	if err == nil {
		t.Fatal("ValidateObs(\"grafana\") = nil, want a refusal")
	}
	for _, v := range []string{"bundled", "adopt", "export", "shared", "none"} {
		if !strings.Contains(err.Error(), v) {
			t.Errorf("refusal %q does not list the valid value %q", err, v)
		}
	}
}
