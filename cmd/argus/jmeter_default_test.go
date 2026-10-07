package main

import (
	"flag"
	"testing"
)

// jmeter_default_test.go — V31-001 §G-2: `--jmeter` DEFAULTS TO `local`.
//
// The default was `docker`, which means "shell out to `docker compose exec` and run JMeter in another
// container". That is the M2.5 shape, and it is wrong for every tier this release ships to: the
// executor image carries JMeter itself, the k8s tiers have no docker socket to exec through, and a
// forgotten flag therefore produced a run that could not start rather than one that ran differently.
//
// ⛔ THE `docker` MODE STAYS. It is a legitimate choice on a compose rig with a jmeter service, and
// removing it would break a shape that works. What changes is which way an unset flag falls.

func TestJMeterModeDefaultsToLocal(t *testing.T) {
	cf := &commonFlags{}
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	cf.bind(fs)
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	if cf.jmeterMode != "local" {
		t.Fatalf("--jmeter defaults to %q, want \"local\".\n"+
			"  `docker` means shelling out to `docker compose exec`: there is no socket to do that\n"+
			"  through on k3d or managed, and the executor image carries JMeter itself. A forgotten\n"+
			"  flag should run, not fail to start.", cf.jmeterMode)
	}
	// and env() must carry the choice through
	if !env(cf).JMeterLocal {
		t.Error("the default does not reach Env.JMeterLocal — the flag would be cosmetic")
	}
}

// ⛔ THE `docker` MODE IS STILL SELECTABLE. A default flip that quietly removed the other mode would
// break every compose rig that runs a jmeter service on purpose.
func TestJMeterModeDockerIsStillSelectable(t *testing.T) {
	cf := &commonFlags{}
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	cf.bind(fs)
	if err := fs.Parse([]string{"--jmeter", "docker"}); err != nil {
		t.Fatal(err)
	}
	if cf.jmeterMode != "docker" {
		t.Fatalf("--jmeter docker was not honoured (%q)", cf.jmeterMode)
	}
	if env(cf).JMeterLocal {
		t.Error("--jmeter docker still reported JMeterLocal — the mode is gone, not defaulted")
	}
}
