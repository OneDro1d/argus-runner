package scenario

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// THE "fault" CHAIN STEP: its authoring-time shape rules.
//
// A fault step breaks something real (a Deployment's replicas, a Toxiproxy link) and the run puts it back on
// every path. What it may touch is decided by the OPERATOR in argus-config.yaml (fault_allowed_targets), not
// by the scenario; what this file refuses is a step whose SHAPE could not be run or would silently mean
// something other than what it says.

// FaultOps are the ops of a fault step, in the order the docs list them.
var FaultOps = []string{"stop", "start", "scale", "wait_ready", "delay", "drop", "truncate", "restore"}

// Bounds of the numbers a fault step may carry.
const (
	FaultReplicasMax   = 10
	FaultLatencyMaxMs  = 60000
	FaultBytesMax      = 1 << 20
	FaultWaitDefault   = 120 * time.Second
	FaultWaitMax       = 600 * time.Second
	FaultTargetPattern = `^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`
)

var faultTargetRe = regexp.MustCompile(FaultTargetPattern)

// faultAllowedKeys: the wire keys each op may carry beside type/name/op/always.
var faultAllowedKeys = map[string][]string{
	"stop":       {"deployment"},
	"start":      {"deployment", "replicas"},
	"scale":      {"deployment", "replicas"},
	"wait_ready": {"deployment", "timeout"},
	"delay":      {"link", "latency_ms", "jitter_ms"},
	"drop":       {"link"},
	"truncate":   {"link", "bytes"},
	"restore":    {},
}

func faultKnownOp(op string) bool {
	for _, o := range FaultOps {
		if o == op {
			return true
		}
	}
	return false
}

// FaultStepProblems lists everything wrong with one fault step's shape, as short sentences without the step
// name. The runner calls it too (the second door: a file can reach a run without passing the validator).
func FaultStepProblems(st ChainStep, raw map[string]json.RawMessage) []string {
	var out []string
	if !faultKnownOp(st.Op) {
		if st.Op == "" {
			return []string{"has no `op` (one of " + strings.Join(FaultOps, ", ") + ")"}
		}
		return []string{fmt.Sprintf("has an unknown `op` %q (one of %s)", st.Op, strings.Join(FaultOps, ", "))}
	}
	if raw != nil {
		ok := map[string]bool{"type": true, "name": true, "op": true, "always": true}
		for _, k := range faultAllowedKeys[st.Op] {
			ok[k] = true
		}
		var extra []string
		for k := range raw {
			if !ok[k] {
				extra = append(extra, k)
			}
		}
		sort.Strings(extra)
		for _, k := range extra {
			out = append(out, fmt.Sprintf("carries `%s`, which a fault step with op %q does not take", k, st.Op))
		}
	}
	name := func(field, v string) {
		if v == "" {
			out = append(out, fmt.Sprintf("op %q needs `%s`, an allowlist name from fault_allowed_targets", st.Op, field))
		} else if !faultTargetRe.MatchString(v) || len(v) > 253 {
			out = append(out, fmt.Sprintf("`%s` must be an allowlist name (lower-case letters, digits, '-' and '.'), not a variable, namespace or URL", field))
		}
	}
	inRange := func(field string, p *int, lo, hi int, required bool) {
		switch {
		case p == nil && required:
			out = append(out, fmt.Sprintf("op %q needs `%s` (%d..%d)", st.Op, field, lo, hi))
		case p != nil && (*p < lo || *p > hi):
			out = append(out, fmt.Sprintf("`%s` is %d, outside %d..%d", field, *p, lo, hi))
		}
	}
	switch st.Op {
	case "stop":
		name("deployment", st.Deployment)
	case "start":
		name("deployment", st.Deployment)
		inRange("replicas", st.Replicas, 1, FaultReplicasMax, false)
	case "scale":
		name("deployment", st.Deployment)
		inRange("replicas", st.Replicas, 1, FaultReplicasMax, true)
	case "wait_ready":
		name("deployment", st.Deployment)
		if st.Timeout != "" {
			if d, err := time.ParseDuration(st.Timeout); err != nil || d <= 0 {
				out = append(out, fmt.Sprintf("`timeout` %q is not a positive Go duration (\"90s\", \"2m\")", st.Timeout))
			} else if d > FaultWaitMax {
				out = append(out, fmt.Sprintf("`timeout` of %s is more than the %s bound", d, FaultWaitMax))
			}
		}
	case "delay":
		name("link", st.Link)
		inRange("latency_ms", st.LatencyMs, 1, FaultLatencyMaxMs, true)
		inRange("jitter_ms", st.JitterMs, 0, FaultLatencyMaxMs, false)
	case "drop":
		name("link", st.Link)
	case "truncate":
		name("link", st.Link)
		inRange("bytes", st.Bytes, 1, FaultBytesMax, true)
	}
	if st.Poll != nil {
		out = append(out, "may not carry `poll` - a fault is applied once")
	}
	if len(st.Save) > 0 {
		out = append(out, "may not carry `save` - a fault step returns nothing to save")
	}
	return out
}

// faultStepErrors is the validator's door for fault steps: shape, and no claim (a fault step is judged by
// whether it could be applied; a `- step <name>:` bullet on it would be executed by nothing).
func faultStepErrors(steps []ChainStep, rawSteps []map[string]json.RawMessage, claims map[string][]string, trigLine, expLine int) []Error {
	var errs []Error
	for i, st := range steps {
		if st.Type != "fault" {
			continue
		}
		var raw map[string]json.RawMessage
		if i < len(rawSteps) {
			raw = rawSteps[i]
		}
		for _, p := range FaultStepProblems(st, raw) {
			errs = append(errs, Error{Line: trigLine, Message: fmt.Sprintf("step %q %s", st.Name, p)})
		}
		if len(claims[st.Name]) > 0 {
			errs = append(errs, Error{Line: expLine, Message: fmt.Sprintf(
				"step %q is a fault step and takes no claim: a fault step passes when the fault was applied and fails when it was refused or could not be applied - "+
					"put the claim on the step that observes the effect", st.Name)})
		}
	}
	return errs
}

// HasFaultStep reports whether the chain carries any fault step.
func HasFaultStep(steps []ChainStep) bool {
	for _, st := range steps {
		if st.Type == "fault" {
			return true
		}
	}
	return false
}
