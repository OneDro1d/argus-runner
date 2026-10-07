package updatecmd

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// discover_exec_test.go — V32 Release QA finding (d): discover.sh RECORDS THE VERSION THE RUNNING
// EXECUTOR REPORTS, and it is RUN here rather than read, because observed.json is JSON printed by bash
// one fragment at a time and a missing comma is invisible to any string match.

func runDiscover(t *testing.T, tier string, prep func(root string)) Observed {
	t.Helper()
	b := runDiscoverRaw(t, tier, prep)
	var obs Observed
	if err := json.Unmarshal(b, &obs); err != nil {
		t.Fatalf("observed.json is not valid JSON — the planner would refuse it as a hard stop: %v\n%s", err, b)
	}
	return obs
}

// runDiscoverRaw is observed.json exactly as discover.sh wrote it.
func runDiscoverRaw(t *testing.T, tier string, prep func(root string)) []byte {
	t.Helper()
	root := t.TempDir()
	in := fakeMachine(t, root, tier)
	if prep != nil {
		prep(root)
	}
	sh := filepath.Join(root, "discover.sh")
	body := RenderDiscover(DiscoverInput{InstanceID: in.InstanceID, Tier: tier, KitDir: in.KitDir,
		StageDir: in.StageDir, KubeContext: in.KubeContext, Folders: in.Observed.Folders,
		GrafanaURL: "http://localhost:3000"})
	if err := os.WriteFile(sh, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", sh)
	cmd.Dir = root
	cmd.Env = stubEnv(t, root, nil)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("discover.sh failed on a healthy fake machine: %v\n%s", err, out)
	}
	b, err := os.ReadFile(filepath.Join(in.StageDir, "observed.json"))
	if err != nil {
		t.Fatalf("discover.sh wrote no observed.json: %v", err)
	}
	return b
}
