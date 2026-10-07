package router

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/OneDro1d/argus-runner/internal/mcpserver"
	"github.com/OneDro1d/argus-runner/internal/role"
)

// The router's transport half. The protocol is NOT reimplemented here: internal/mcpserver already
// serves Streamable HTTP + legacy SSE with a handshake, session state and a hat-filtered tools/list,
// and internal/mcp already speaks both transports as a client. The router is a routing table
// (table.go) plus the glue below.
//
// Reusing mcpserver buys VR-R7 structurally rather than by re-implementation: its visibleTools()
// already drops NSAuthor tools for a hat that cannot access scenarios, so a product folder's
// tools/list cannot contain author__* even if the glue below were wrong.

// Forwarder sends one tool call to a resolved upstream. It is an interface so the routing and scope
// logic can be tested without a network, and so the real client (internal/mcp) stays swappable —
// the router speaks to two different planes and both are ordinary MCP servers.
type Forwarder interface {
	// VR2-13: returns the upstream's OWN payload (already unwrapped) plus its isError, so the proxy
	// wraps EXACTLY ONCE. It used to return the upstream's whole envelope, which the proxy then
	// wrapped again — two envelopes deep, measured on every tier.
	Forward(target Target, tool string, args json.RawMessage) (any, bool, error)
}

// auth resolves a presented ROUTER token to a Principal. The token IS the folder's identity: the hat
// comes from the folder record, never from anything the caller says.
//
// Subject carries the router token so a handler can find its folder again. That is safe here — this
// server is loopback-only and single-user — and it keeps mcpserver.Principal unchanged.
type auth struct{ tbl *Table }

func (a auth) Authenticate(token string) (mcpserver.Principal, error) {
	f, err := a.tbl.Resolve(token)
	if err != nil {
		return mcpserver.Principal{}, err
	}
	return mcpserver.Principal{Hat: f.Hat, Subject: token}, nil
}

// Authenticator exposes the table as an mcpserver.Authenticator.
func Authenticator(tbl *Table) mcpserver.Authenticator { return auth{tbl} }

func schemaFor(tool string) map[string]any {
	props := map[string]any{
		"instance_id": map[string]any{
			"type": "string",
			"description": "the instance to act on — REQUIRED, and exactly ONE (a joined list is refused; " +
				"call once per instance)",
		},
	}
	// Pass-through arguments the runner tools accept. The router does not validate them: the
	// upstream owns its own contract, and duplicating it here would be a second contract to drift.
	// "limit" and "since" arrived with author__list_runs (VR10-R2): the control plane validates them
	// (a bad one is refused by name, never clamped or dropped), and this list is only what an agent
	// can SEE — an argument absent from it is one the agent never sends.
	for _, k := range []string{"run_id", "scenario_ref", "tag", "layer", "correlation_id", "window",
		"scenario_id", "content", "path", "description", "target_service", "run_request_id",
		"limit", "since"} {
		props[k] = map[string]any{"type": "string"}
	}
	return map[string]any{"type": "object", "properties": props, "required": []string{"instance_id"}}
}

// Tools builds the router's tool set: the 6 runner tools plus the author tools, each a thin proxy.
// mcpserver filters the author set out for a product-hat principal, and Route refuses the call as
// well — listing is a courtesy, the refusal is the boundary.
func Tools(tbl *Table, fwd Forwarder) []mcpserver.Tool {
	var out []mcpserver.Tool
	add := func(name string, ns mcpserver.Namespace, desc string) {
		// PreCheck is NOT optional here, and the reason is worth stating because it cost a live
		// end-to-end run to find.
		//
		// runner__run is ASYNC in mcpserver (DF-06, server.go:383): the dispatcher takes the run
		// lock, runs the handler in a GOROUTINE and returns a canned {"status":"running"} — the
		// handler's Outcome is DISCARDED. So every scope check the proxy performs for runner__run
		// happened in the background, where nobody could see it, and the caller was told "run
		// started in the background".
		//
		// MEASURED against a live router before this was added:
		//   test folder -> runner__run{instance_id:"sutz"}      -> OK "run started…"   (out of folder!)
		//   test folder -> runner__run{instance_id:"suta,sutb"} -> OK "run started…"   (VR-R13!)
		// A half-run reported as success is precisely the failure VR-R13 exists to prevent, so the
		// router's own rule was being defeated by the transport it reuses.
		//
		// checkOnly is side-effect free (Route is pure), so the SAME closure serves as the
		// pre-check and as the handler's first act — one implementation, no drift.
		check := func(args json.RawMessage, prin mcpserver.Principal) *mcpserver.Outcome {
			return checkOnly(tbl, name, args, prin)
		}
		out = append(out, mcpserver.Tool{
			Name: name, Namespace: ns, Description: desc, InputSchema: schemaFor(name),
			PreCheck: check,
			Handler: func(args json.RawMessage, prin mcpserver.Principal) mcpserver.Outcome {
				if o := check(args, prin); o != nil {
					return *o
				}
				return proxy(tbl, fwd, name, args, prin)
			},
		})
	}
	for _, t := range RunnerTools {
		add(t, mcpserver.NSRunner, "Routed to this instance's in-env executor by the local router.")
	}
	for _, t := range AuthorTools {
		add(t, mcpserver.NSAuthor, "Routed to the control plane by the local router (test hat only).")
	}
	return out
}

// checkOnly performs every refusal the router can make WITHOUT touching the network: folder
// identity, hat scope, instance_id shape and folder membership. It returns nil when the call may
// proceed.
//
// It is deliberately side-effect free so it can run BEFORE an async tool announces itself
// (mcpserver.Tool.PreCheck) as well as inside the handler. Sharing one closure is what keeps the two
// from drifting — the in-env tools do the same thing for the same reason.
func checkOnly(tbl *Table, tool string, args json.RawMessage, prin mcpserver.Principal) *mcpserver.Outcome {
	f, err := tbl.Resolve(prin.Subject)
	if err != nil {
		// The session authenticated, so this means the folder was removed mid-session (a teardown).
		// Reported as a tool error rather than a protocol error: the CALL failed, the connection is fine.
		o := mcpserver.ToolErr(map[string]any{"error": err.Error()})
		return &o
	}
	// Defence in depth. mcpserver has already applied the scope filter using the hat IT was given;
	// re-deriving the hat from the FOLDER means a mismatch between the two cannot silently favour the
	// caller. If they ever disagree, the folder wins and the call is refused.
	if strings.HasPrefix(tool, "author__") && f.Hat != role.Test {
		o := mcpserver.ToolErr(map[string]any{
			"error": fmt.Sprintf("router: %q is not available to a product-agent folder", tool),
		})
		return &o
	}
	if _, err := f.Route(tool, argString(args, "instance_id")); err != nil {
		o := mcpserver.ToolErr(map[string]any{"error": err.Error()})
		return &o
	}
	return nil
}

// proxy is the whole request path: identify the folder, route, forward. Its checks are checkOnly's,
// re-run here because a handler must never assume its pre-check ran.
func proxy(tbl *Table, fwd Forwarder, tool string, args json.RawMessage, prin mcpserver.Principal) mcpserver.Outcome {
	if o := checkOnly(tbl, tool, args, prin); o != nil {
		return *o
	}
	f, err := tbl.Resolve(prin.Subject)
	if err != nil {
		return mcpserver.ToolErr(map[string]any{"error": err.Error()})
	}
	target, err := f.Route(tool, argString(args, "instance_id"))
	if err != nil {
		return mcpserver.ToolErr(map[string]any{"error": err.Error()})
	}
	payload, upstreamIsErr, err := fwd.Forward(target, tool, args)
	if err != nil {
		// An upstream that could not be REACHED is not the same as one that answered "no" — the
		// distinction that matters everywhere in this codebase, applied to the proxy path. Both used to
		// come out as "could not reach", which is a false statement about a healthy instance.
		var refusal *UpstreamRefusal
		if errors.As(err, &refusal) {
			return mcpserver.ToolErr(map[string]any{
				"error":    refusal.Message,
				"code":     refusal.Code,
				"plane":    target.Plane,
				"reached":  true,
				"instance": argString(args, "instance_id"),
			})
		}
		return mcpserver.ToolErr(map[string]any{
			"error":    fmt.Sprintf("router: could not reach the %s plane for this instance: %v", target.Plane, err),
			"plane":    target.Plane,
			"reached":  false,
			"instance": argString(args, "instance_id"),
		})
	}
	// VR2-13: ONE envelope — and the upstream's own isError rides on it. Flattening it to Ok()
	// would make a tool that ANSWERED "failed" arrive looking like a success.
	if upstreamIsErr {
		return mcpserver.ToolErr(payload)
	}
	return mcpserver.Ok(payload)
}

func argString(args json.RawMessage, key string) string {
	if len(args) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(args, &m); err != nil {
		return ""
	}
	s, _ := m[key].(string)
	return s
}
