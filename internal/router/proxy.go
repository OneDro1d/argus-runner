package router

import (
	"encoding/json"

	"github.com/OneDro1d/argus-runner/internal/mcpserver"
)

// The router's transport half. The protocol is NOT reimplemented here: internal/mcpserver already
// serves Streamable HTTP + legacy SSE with a handshake, session state and a hat-filtered tools/list,
// and internal/mcp already speaks both transports as a client. The router is a routing table
// (table.go) plus the per-session tool sets (session.go, runner.go).
//
// Reusing mcpserver buys VR-R7 structurally rather than by re-implementation: its hat filter already
// drops NSAuthor tools for a hat that cannot access scenarios — for a session set exactly as for static
// tools (AC-D69) — so a product folder's tools/list cannot contain an author tool even if the glue were wrong.
//
// The router keeps no tool of its own any more: the author tools come from the control plane (AC-D69)
// and the runner tools from each instance's executor (AC-D70), both passed on as they were listed.

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
