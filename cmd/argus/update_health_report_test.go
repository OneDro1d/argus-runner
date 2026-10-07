package main

import (
	"crypto/ed25519"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/updatecmd"
)

// update_health_report_test.go — AC-D53, design step 8: THE CONTROL PLANE LEARNS HOW A-3's HEALTH CHECK FAILED.
//
// apply.sh records the verdict in progress.json — `{"id":"A-3","state":"unconfirmed","health":"unreachable",
// "detail":"…"}` — but the re-hash read only id and state, so the record and the report said `rolled-back` + `A-3`
// for an executor the runtime could not be asked about exactly as for one it SAW unhealthy. An estate of seven
// "could not reach" rollbacks looked like seven broken images. The verdict now rides in last_outcome.

const acd53Detail = "docker could not list the executor of argus-inst-i1: Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?"

func acd53Stage(t *testing.T, progress ...string) string {
	t.Helper()
	stage := t.TempDir()
	plan := updatecmd.Plan{
		InstanceID: "i1", Tier: "compose", Version: "0.3.32", ImageDigest: "ghcr.io/x/exec@sha256:new",
		Steps: []updatecmd.Step{{ID: "A-1"}, {ID: "A-3"}},
		Artefacts: []updatecmd.Artefact{
			{Kind: "kit", Name: "argus-kit", Version: "0.3.32"},
			{Kind: "executor", Name: "executor", Version: "0.3.32", Image: "ghcr.io/x/exec@sha256:new"},
		},
	}
	b, _ := json.MarshalIndent(plan, "", "  ")
	if err := os.WriteFile(filepath.Join(stage, "plan.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, "observed.json"),
		[]byte(`{"tier":"compose","executor_version":"0.3.31","env":{"ARGUS_INSTALLED_VERSION":"0.3.31"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	writeLines(t, filepath.Join(stage, "progress.json"), progress...)
	return stage
}

func rehashed(t *testing.T, stage, outcome, failed string) updatecmd.Manifest {
	t.Helper()
	args := []string{"update", "rehash", "--stage", stage, "--outcome", outcome}
	if failed != "" {
		args = append(args, "--failed-step", failed)
	}
	if code := dispatch(args); code != exitOK {
		t.Fatalf("update rehash exited %d", code)
	}
	raw, _ := os.ReadFile(filepath.Join(stage, "manifest.next.json"))
	var m updatecmd.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestUpdateRehash_ACD53_CarriesTheA3HealthVerdict(t *testing.T) {
	t.Setenv("ARGUS_RUNNER_TOKEN", "")
	t.Setenv("ARGUS_AUTHOR_TOKEN", "")
	t.Setenv("ARGUS_TOKEN", "")

	t.Run("could not reach, then a clean undo", func(t *testing.T) {
		stage := acd53Stage(t,
			`{"id":"A-1","state":"applied","at":"2026-09-30T10:00:00Z"}`,
			`{"id":"A-3","state":"unconfirmed","at":"2026-09-30T10:00:01Z"}`,
			`{"id":"A-3","state":"unconfirmed","health":"unreachable","detail":"`+acd53Detail+`","at":"2026-09-30T10:03:01Z"}`,
			`{"id":"A-3","state":"unconfirmed","at":"2026-09-30T10:03:02Z"}`, // the undo, before its own recreate
			`{"id":"A-3","state":"undone","at":"2026-09-30T10:03:40Z"}`,
			`{"id":"A-1","state":"undone","at":"2026-09-30T10:03:41Z"}`)
		m := rehashed(t, stage, "rolled-back", "A-3")
		if m.LastOutcome == nil || m.LastOutcome.Health != "unreachable" || m.LastOutcome.Detail != acd53Detail {
			t.Errorf("the re-hash dropped A-3's verdict: last_outcome = %+v", m.LastOutcome)
		}
	})
	t.Run("seen unhealthy", func(t *testing.T) {
		stage := acd53Stage(t,
			`{"id":"A-1","state":"applied","at":"2026-09-30T10:00:00Z"}`,
			`{"id":"A-3","state":"unconfirmed","at":"2026-09-30T10:00:01Z"}`,
			`{"id":"A-3","state":"unconfirmed","health":"unhealthy","detail":"its own healthcheck says unhealthy","at":"2026-09-30T10:03:01Z"}`,
			`{"id":"A-3","state":"undone","at":"2026-09-30T10:03:40Z"}`)
		m := rehashed(t, stage, "rolled-back", "A-3")
		if m.LastOutcome == nil || m.LastOutcome.Health != "unhealthy" {
			t.Errorf("last_outcome = %+v, want health unhealthy", m.LastOutcome)
		}
	})
	t.Run("A-3 passed its check: no verdict to carry", func(t *testing.T) {
		stage := acd53Stage(t,
			`{"id":"A-1","state":"applied"}`,
			`{"id":"A-3","state":"unconfirmed"}`,
			`{"id":"A-3","state":"applied"}`)
		m := rehashed(t, stage, "updated", "")
		if m.LastOutcome == nil || m.LastOutcome.Health != "" || m.LastOutcome.Detail != "" {
			t.Errorf("a health verdict was invented for a check that passed: %+v", m.LastOutcome)
		}
	})
}

// the report is what the control plane stores (fed.go handleInstalled stores the body as it arrives)
func TestUpdateReport_ACD53_CarriesTheHealthVerdict(t *testing.T) {
	t.Setenv("ARGUS_RUNNER_TOKEN", "")
	t.Setenv("ARGUS_AUTHOR_TOKEN", "")
	t.Setenv("ARGUS_TOKEN", "")

	var mu sync.Mutex
	var got []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/fed/installed" {
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			got = b
			mu.Unlock()
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(t.TempDir(), "identity.i1.key")
	if err := os.WriteFile(key, priv, 0o600); err != nil {
		t.Fatal(err)
	}
	stage := acd53Stage(t,
		`{"id":"A-3","state":"unconfirmed","health":"unreachable","detail":"`+acd53Detail+`"}`,
		`{"id":"A-3","state":"undone"}`)
	report := func(t *testing.T, state string) updatecmd.LastOutcome {
		t.Helper()
		if code := dispatch([]string{"update", "report", "--control-plane", srv.URL, "--instance-id", "i1",
			"--router-state", state, "--stage", stage, "--identity", key,
			"--outcome", "rolled-back", "--failed-step", "A-3"}); code != exitOK {
			t.Fatalf("update report exited %d", code)
		}
		mu.Lock()
		defer mu.Unlock()
		var m updatecmd.Manifest
		if err := json.Unmarshal(got, &m); err != nil || m.LastOutcome == nil {
			t.Fatalf("the control plane got no manifest with a last outcome: %s (%v)", got, err)
		}
		return *m.LastOutcome
	}

	t.Run("from the record A-8a committed", func(t *testing.T) {
		state := t.TempDir()
		if _, err := updatecmd.CommitManifest(state, updatecmd.Manifest{InstanceID: "i1", Tier: "compose", Version: "0.3.31",
			LastOutcome: &updatecmd.LastOutcome{Health: "unreachable", Detail: acd53Detail}},
			updatecmd.CommitOpts{Outcome: "rolled-back", FailedStep: "A-3"}); err != nil {
			t.Fatal(err)
		}
		if lo := report(t, state); lo.Health != "unreachable" || lo.Detail != acd53Detail {
			t.Errorf("the report dropped the verdict the record carries: %+v", lo)
		}
	})
	t.Run("the record is another attempt's: from this attempt's own progress", func(t *testing.T) {
		state := t.TempDir() // no record at all — A-8a's commit did not happen
		if lo := report(t, state); lo.Word != "rolled-back" || lo.Health != "unreachable" {
			t.Errorf("the report rebuilt this attempt's outcome without its verdict: %+v", lo)
		}
	})
	// ⛔ R8-2: THE RECORD IS AN EARLIER ATTEMPT'S WITH THE SAME WORD AND STEP. The report took "this attempt's record"
	// to mean "a record ending the same way" — so this attempt's A-8a not committing (the runtime down) posted the
	// EARLIER attempt's verdict for this one. The stage is recreated on every paste: its progress.json is always
	// this attempt's own.
	t.Run("the record is an earlier attempt's that ended the same way: this attempt's verdict", func(t *testing.T) {
		state := t.TempDir()
		earlier := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
		if _, err := updatecmd.CommitManifest(state, updatecmd.Manifest{InstanceID: "i1", Tier: "compose", Version: "0.3.31",
			LastOutcome: &updatecmd.LastOutcome{Health: "unhealthy", Detail: "EARLIER attempt: its own healthcheck says unhealthy"}},
			updatecmd.CommitOpts{Outcome: "rolled-back", FailedStep: "A-3", Now: earlier}); err != nil {
			t.Fatal(err)
		}
		began := time.Now().UTC().Truncate(time.Second)
		lo := report(t, state)
		if lo.Health != "unreachable" || lo.Detail != acd53Detail {
			t.Errorf("the report posted another attempt's verdict for this one: %+v", lo)
		}
		// L2-3: nor the earlier attempt's TIME — the control plane serves it as last_update.at
		// (internal/control/instanceview.go lastUpdate), beside this attempt's verdict. THIS attempt's time: a real
		// RFC 3339 time taken when the report was made (design check O-2 — "not the earlier one" alone let "" pass)
		if lo.At == earlier.Format(time.RFC3339) {
			t.Errorf("the report posted this attempt's verdict with the earlier attempt's time %s: %+v", lo.At, lo)
		}
		if at, err := time.Parse(time.RFC3339, lo.At); err != nil || at.Before(began) {
			t.Errorf("the report's time %q is not this attempt's (not RFC 3339, or before %s)", lo.At, began.Format(time.RFC3339))
		}
	})
}
