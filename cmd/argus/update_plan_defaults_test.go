package main

import (
	"os"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/buildinfo"
	"github.com/OneDro1d/argus-runner/internal/updatecmd"
)

// The page's block passes no --version and no --control-plane (SA §0.7). `update plan` must then fill
// both from what it knows for certain — and never from a guess.
func TestUpdatePlan_DefaultsTheVersionAndTheControlPlaneFromWhatItKnows(t *testing.T) {
	obs := updatecmd.Observed{Env: map[string]string{"ARGUS_CP_URL": " https://cp.example "}}

	t.Run("a forward update installs the version of the image running the verb", func(t *testing.T) {
		a := updateArgs{}
		applyPlanDefaults(&a, obs)
		want := buildinfo.Resolve(os.Getenv("ARGUS_VERSION"))
		if a.Version == "" || a.Version != want {
			t.Errorf("version = %q, want this binary's own %q", a.Version, want)
		}
		if a.CPURL != "https://cp.example" {
			t.Errorf("control plane = %q, want the one the instance's env file records", a.CPURL)
		}
	})

	t.Run("a rollback installs the version it names", func(t *testing.T) {
		a := updateArgs{RollbackTo: "0.3.31"}
		applyPlanDefaults(&a, obs)
		if a.Version != "0.3.31" {
			t.Errorf("version = %q, want the --rollback-to version", a.Version)
		}
	})

	t.Run("an explicit flag always wins", func(t *testing.T) {
		a := updateArgs{Version: "9.9.9", CPURL: "https://other.example"}
		applyPlanDefaults(&a, obs)
		if a.Version != "9.9.9" || a.CPURL != "https://other.example" {
			t.Errorf("an explicit flag was overridden: %+v", a)
		}
	})

	t.Run("no recorded control plane stays empty — never invented", func(t *testing.T) {
		a := updateArgs{}
		applyPlanDefaults(&a, updatecmd.Observed{Env: map[string]string{}})
		if a.CPURL != "" {
			t.Errorf("control plane = %q from an env file that records none", a.CPURL)
		}
	})
}
