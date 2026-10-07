package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/OneDro1d/argus-runner/internal/router"
)

// applyRotatedToken is the router's write half of a rotation: it stores the rotated author token (minted during
// onboarding) on THIS machine's record for (control plane, user) — V27-009 redesign: one record, one write, no
// per-folder copies to keep in step.
//
// ⛔ A MISSING RECORD IS AN ERROR, NOT A SILENT NO-OP. Returning nil here is what acknowledges the rotation on the
// next beat, and the acknowledgement REVOKES the old token. A router that stored nothing and acknowledged anyway
// would leave the machine with no credential and the account with no way back — VR5-C2's trap. So "no record"
// refuses, the old token stays valid (VR-B6), and the next beat is offered the same rotation again.
func applyRotatedToken(stateDir, url, user, tok string, emitFn func(any)) error {
	err := router.SetRecordToken(stateDir, url, user, tok, time.Time{})
	if err != nil {
		if strings.Contains(err.Error(), "no record for") {
			return fmt.Errorf("refusing to acknowledge the rotation: this machine holds no record for %s (user %q), "+
				"so the replacement was stored nowhere (router state %s). Acknowledging would revoke the old token and "+
				"leave this machine unable to reach the author plane. Run `router register` (onboarding step 8a) or "+
				"re-onboard a SUT here; the next heartbeat is offered the rotation again", url, user, stateDir)
		}
		return err
	}
	emitFn(map[string]any{"router": "author token rotated", "control_plane": url, "owner": user, "records_updated": 1})
	return nil
}
