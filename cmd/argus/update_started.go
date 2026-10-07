package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/OneDro1d/argus-runner/internal/federation"
	"github.com/OneDro1d/argus-runner/internal/runner"
)

// cmdUpdateStarted tells the control plane that an update (or rollback) block has BEGUN (AC-D63, #407).
//
// apply.sh calls it once, after its preflight has passed and BEFORE the first step replaces anything, so the
// Environments page can say "update in progress" for the whole of the block rather than from A-3 on. The end is
// not reported here: the block's final `update report` (every exit path) posts the manifest to /fed/installed, and
// the control plane clears the started marker in that same write.
//
// ⛔ IT IS BEST-EFFORT AND NEVER A REASON TO STOP (C-13, as `report`): a control plane that is down, or an OLD one
// that has no /fed/update-started route (404), must not prevent an update the operator started. It exits 0 and
// says what happened. ⛔ IT SENDS NO TIME: the control plane stamps its own clock.
func cmdUpdateStarted(a updateArgs) int {
	if a.Version == "" {
		return emitErr(exitUsage, "update started: --version (the version this block moves the instance to) is required")
	}
	if a.InstanceID == "" {
		return emitErr(exitUsage, "update started: --instance-id is required")
	}
	notSent := func(reason string) int {
		emit(map[string]any{"started_reported": false, "reason": reason, "target": a.Version})
		return exitOK
	}
	if a.CPURL == "" {
		return notSent("no control plane configured")
	}
	if a.Identity == "" {
		return notSent("no --identity key to sign the report with")
	}
	priv, err := runner.LoadKey(a.Identity)
	if err != nil {
		return notSent(fmt.Sprintf("the instance identity could not be read: %v", err))
	}
	tok, err := federation.MintJWT(priv, a.InstanceID, time.Now())
	if err != nil {
		return notSent(fmt.Sprintf("could not sign the report: %v", err))
	}
	kind := "update"
	if a.RollbackTo != "" {
		kind = "rollback"
	}
	body, _ := json.Marshal(map[string]string{"target": a.Version, "kind": kind})
	req, err := http.NewRequest("POST", strings.TrimRight(a.CPURL, "/")+"/fed/update-started", bytes.NewReader(body))
	if err != nil {
		return notSent(err.Error())
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return notSent("the control plane did not answer")
	}
	defer resp.Body.Close()
	emit(map[string]any{"started_reported": resp.StatusCode < 300, "status": resp.StatusCode, "target": a.Version, "kind": kind})
	return exitOK
}
