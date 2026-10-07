package router

import (
	"fmt"
	"os"
	"path/filepath"
)

// StateReadiness returns a readiness probe for a serving router (VR8-K2 / V26-003).
//
// THE DEFECT IT REPORTS. `argus-router` ran `Up 13 hours (healthy)` with restarts=0 while its
// state directory had been deleted from the HOST, leaving the container's bind mount dangling and
// reading empty. It held zero folders, so every agent on the machine was silently unroutable, and
// `argus router status` answered `{"folders": []}` — which is also exactly what a brand-new
// router says. Nothing anywhere could tell the two apart.
//
// ⛔ THE PREDICATE IS NOT "ZERO FOLDERS". A freshly started router legitimately holds none, and a
// probe that failed on an empty table would turn every normal cold start into an alarm — the same
// defect pointing the other way, and the reason StateDir()'s own comment about this hazard never
// became a check. The predicate is: THE ROUTER CAN STILL READ THE STATE IT IS SERVING FROM.
//
//   - the state DIRECTORY must exist and be readable, always. A mount that vanished entirely fails
//     here.
//   - if the router loaded a NON-EMPTY table at startup, state.json must still be readable. That is
//     what separates "the state I was serving is gone" from "I never had any" — and it is the case
//     that actually happened, because the estate was live when the directory was wiped.
//
// hadFolders is captured ONCE, at startup, by the caller. It must not be re-read: a probe that
// recomputed it from the current state would decide that a router which just lost everything never
// had anything, which is precisely the confusion being fixed.
func StateReadiness(dir string, hadFolders bool) func() error {
	return func() error {
		fi, err := os.Stat(dir)
		if err != nil {
			return fmt.Errorf("the router state directory %s cannot be read (%v) — every agent folder "+
				"on this machine routes through state kept here, so nothing can be served until it is back", dir, err)
		}
		if !fi.IsDir() {
			return fmt.Errorf("the router state path %s is not a directory", dir)
		}
		if !hadFolders {
			// Nothing was being served, so nothing has been lost. A router that has not been wired
			// into any folder yet is ready: it is waiting, not broken.
			return nil
		}
		sf := filepath.Join(dir, "state.json")
		if _, err := os.Stat(sf); err != nil {
			return fmt.Errorf("this router started with a routing table and %s is now unreadable (%v) — "+
				"the folders it was serving cannot be resolved. On a bind mount this usually means the "+
				"host directory was deleted underneath the container", sf, err)
		}
		return nil
	}
}
