package router_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/router"
)

// VR8-K2 (V26-003). Both directions are required, and the second is the one that stops this fix
// becoming a false alarm on every cold start.
func TestStateReadiness(t *testing.T) {
	t.Run("a fresh router with an empty state directory is READY", func(t *testing.T) {
		dir := t.TempDir()
		if err := router.StateReadiness(dir, false)(); err != nil {
			t.Fatalf("a router that has never been wired reported not-ready: %v\n"+
				"  Zero folders is the NORMAL cold start. A probe that fails here turns every first "+
				"run into an alarm, which is the V26-003 defect pointing the other way.", err)
		}
	})

	t.Run("a router that never had folders stays READY even with no state file", func(t *testing.T) {
		dir := t.TempDir()
		if err := router.StateReadiness(dir, false)(); err != nil {
			t.Fatalf("not-ready without a state file and without ever having folders: %v", err)
		}
	})

	t.Run("a router SERVING folders whose state file vanished is NOT READY", func(t *testing.T) {
		dir := t.TempDir()
		// It had a table at startup; the file is now gone (the dangling-bind-mount case).
		err := router.StateReadiness(dir, true)()
		if err == nil {
			t.Fatal("a router that started with a routing table and lost its state file reported READY. " +
				"This is exactly the 13-hours-(healthy) case: zero folders served, every agent on the " +
				"machine unroutable, and every signal green.")
		}
		if !strings.Contains(err.Error(), "state.json") {
			t.Errorf("the reason does not name the missing file: %v", err)
		}
	})

	t.Run("a router SERVING folders with its state file intact is READY", func(t *testing.T) {
		dir := t.TempDir()
		if werr := os.WriteFile(filepath.Join(dir, "state.json"), []byte(`{"port":9765}`), 0o600); werr != nil {
			t.Fatalf("write state: %v", werr)
		}
		if err := router.StateReadiness(dir, true)(); err != nil {
			t.Fatalf("a healthy serving router reported not-ready: %v", err)
		}
	})

	t.Run("a state directory that does not exist is NOT READY, whatever it had", func(t *testing.T) {
		gone := filepath.Join(t.TempDir(), "never-created")
		for _, had := range []bool{false, true} {
			err := router.StateReadiness(gone, had)()
			if err == nil {
				t.Errorf("a missing state directory reported READY (hadFolders=%v)", had)
				continue
			}
			if !strings.Contains(err.Error(), "cannot be read") {
				t.Errorf("the reason does not say what is wrong: %v", err)
			}
		}
	})
}
