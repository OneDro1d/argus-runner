// Package mcpserver is the Argus MCP server: a hand-rolled native JSON-RPC 2.0
// server (DEC-002 = Camp B; NOT an external SDK) that wraps the SAME internal/* runner-core the
// CLI wraps. The IN-ENV instance exposes the 6 runner__* tools (mcpserver.DefaultTools; C1 dropped
// the M2.5 author__* leftover — authoring moved to the cloud plane). The CLOUD control-plane
// instance exposes the author__* tools (control.CloudTools). It speaks the legacy HTTP+SSE
// 2-endpoint transport (GET /sse + POST /message?sessionId), protocolVersion "2024-11-05".
//
// This file holds the wire types + the tool-registry abstraction. The protocol
// state machine + dispatch live in server.go; the HTTP+SSE transport in
// transport.go; the in-env runner tools are registered from tools.go.
package mcpserver

import (
	"encoding/json"

	"github.com/OneDro1d/argus-runner/internal/role"
)

// ProtocolVersion is the single revision Argus's own server emits (Camp-B).
const ProtocolVersion = "2024-11-05"

// JSON-RPC + MCP error codes. -32700..-32603 are the JSON-RPC standard set;
// -32000..-32099 is the implementation-defined "server error" range.
const (
	CodeParse          = -32700
	CodeInvalidReq     = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternal       = -32603
	CodeUnauthorized   = -32001 // no / wrong-scope / unknown token (the auth plane)
	CodeRunInProgress  = -32002 // DF-DEC-M25-01: a second runner__run while one is in flight
	CodeNoSession      = -32003 // tools/call before initialize / unknown-or-expired session (VR-H8)
)

// Request is an inbound JSON-RPC 2.0 request (a notification has a nil ID).
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Response is an outbound JSON-RPC 2.0 response. Exactly one of Result / Error is set.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// RPCError is a top-level JSON-RPC error (the protocol plane).
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// ToolResult is the MCP tools/call success shape (a JSON-RPC result). isError:true
// is the tool-error plane (the call succeeded at protocol level; the tool failed).
type ToolResult struct {
	Content []ContentBlock `json:"content"`
	IsError bool           `json:"isError"`
}

// ContentBlock carries the tool's structured payload as text (JSON when structured).
type ContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// Namespace is a tool's scope group.
type Namespace string

const (
	NSRunner   Namespace = "runner"   // both hats may call
	NSAuthor   Namespace = "author"   // test hat only (the dark-factory holdout)
	NSExecutor Namespace = "executor" // AC-16: the executor hat only — invisible to runner and author
	// NSBuilder (AC-17) is a FOURTH, disjoint scope like NSExecutor: the builder hat is the runner-scope
	// credential a hub member holds — today the SAME principal shape a runner-scope OAuth token already
	// resolves to (cloudauth.go: Principal{Hat: role.Product, Workspace, Subject}), reused rather than
	// inventing a new role.Role. What distinguishes a builder from the in-environment product hat that
	// also carries role.Product is the WORKSPACE: only a workspace-bound Product principal (the hub's
	// own runner-scope token) sees this namespace — a local, workspace-less static product token never
	// does, so the in-env local router's NSRunner tools and the control plane's relayed NSBuilder tools
	// never collide even though both compile down to the same role.Role. The one workspace-less
	// builder is the opt-in owner-wide PAT, recognised by Principal.OwnerWide, a
	// marker only the control plane's PAT authenticator sets.
	NSBuilder Namespace = "builder"
)

// Outcome is what a tool Handler returns. If RPCErr is set it becomes a top-level
// JSON-RPC error (protocol plane) and Payload/IsError are ignored. Otherwise the
// Payload is enveloped; IsError true sets result.isError (the tool plane).
type Outcome struct {
	Payload any
	IsError bool
	RPCErr  *RPCError
}

// Principal is the authenticated caller (M3): the hat (scope) + the workspace the token is bound to +
// the subject. For the M2.5 static-token path Workspace/Subject are empty (single-tenant); an OAuth
// access token fills them (the workspace claim → tenancy enforcement at the cloud tools).
type Principal struct {
	Hat       role.Role
	Workspace string
	Subject   string
	// OwnerWide marks the ONE principal shape that is a builder without a workspace: an owner-wide
	// builder PAT (, runner scope, workspace NULL). It is set ONLY by the control plane's
	// PAT authenticator (cloudauth.go), and isBuilder additionally needs a Subject with it. It is a marker
	// rather than "Subject != empty" on purpose: the local router builds Product-hat principals with a
	// Subject (the router token) and no workspace (router/proxy.go), and an own-OAuth runner token may
	// carry a subject with an empty workspace claim; neither may ever read as a builder.
	OwnerWide bool
	// BoundPAT is true ONLY for an author PAT (odts_...) minted bound to one workspace. It is the
	// credential KIND, which Workspace alone cannot say: the owner's own-OAuth session carries a
	// workspace too. Set in one place (control/cloudauth.go, the PAT branch); read by control's
	// refuseBoundPAT (tenancy.go) and the handlers in web.go that narrow a listing.
	BoundPAT bool
}

// Authenticator maps a bearer token to a Principal (or an error). The M2.5 path wraps a static
// auth.Config; the M3 cloud path also accepts own-OAuth access tokens (AUTH_MODE gateway).
type Authenticator interface {
	Authenticate(token string) (Principal, error)
}

// Handler runs a tool. It receives the raw `arguments` params and the authenticated Principal (already
// authorized for this tool's namespace). It MUST NOT do its own auth.
type Handler func(args json.RawMessage, p Principal) Outcome

// Tool is one registered MCP tool.
type Tool struct {
	Name        string
	Namespace   Namespace
	Description string
	InputSchema map[string]any // JSON Schema; instance_id:string is present on every one
	Handler     Handler
	// PreCheck validates the ARGUMENTS alone, cheaply and with no side effect, so an argument
	// error can be refused BEFORE an async tool announces itself. runner__run is dispatched in
	// the background: it takes the local run lock, mints a run_id and acquires the CP run fence
	// before the handler ever runs, and the handler's Outcome is then DISCARDED (only ok/not-ok
	// survives). Without this hook a bad argument produced {"status":"running"} + a run_id that
	// never resolves, a fenced instance, and a phantom failed run in the cloud ledger — while
	// get_report told the caller to keep polling. Set by the wrapper in tools.go; nil = nothing
	// to pre-check.
	PreCheck func(args json.RawMessage, prin Principal) *Outcome
	// Aliases are additional names that DISPATCH to this tool but are NOT published by tools/list.
	//
	// This exists for exactly one job: renaming a shipped tool without a window in which every
	// caller is broken. A rename is otherwise un-deployable in a fleet, because the server and its
	// callers cannot change in the same instant — whichever ships first speaks a name the other
	// does not know, and "unknown tool" is what the user sees in between.
	//
	// Invisible on purpose. An alias that appeared in tools/list would be a second tool as far as
	// any client is concerned: agents would pick either name at random, and the catalogue would
	// advertise 18 tools where there are 9. Old callers keep working; nobody new learns the old
	// name; the alias can be deleted once no caller uses it.
	Aliases []string
	// Async runs the tool in the BACKGROUND: the dispatcher mints a run id, takes the run lock,
	// starts the handler in a goroutine and answers {"status":"running", run_id} immediately, without
	// waiting. The handler's Outcome is DISCARDED — nothing it returns reaches the caller.
	//
	// It is a FIELD because it used to be a name match on "runner__run", and the name is shared by two
	// very different servers. The in-env executor is the one that runs scenarios and must not make an
	// MCP client wait minutes for a reply. The local ROUTER registers a tool with the same name whose
	// handler merely FORWARDS — and inherited the async branch by accident, which cost two defects:
	//
	//   * the router's scope checks ran in the background goroutine where nobody could see them, so an
	//     out-of-folder instance_id was answered "run started…" (fixed by PreCheck, see router/proxy.go)
	//   * the router MINTED ITS OWN run id and returned it, while the executor minted another and used
	//     that one for the results, the report and the ledger — so the id the caller received could
	//     never fetch its own report (INT-037)
	//
	// Declaring it per-tool means a server opts IN to backgrounding rather than being opted in by a
	// string it happens to share.
	Async bool
	// Hidden (AC-17) keeps a tool DISPATCHABLE (present in the tools/call table, same scope check as
	// any other tool of its Namespace) while never appearing in tools/list — generalizing the Aliases
	// mechanism's own invisibility for a different reason. Aliases exist so an old NAME still reaches a
	// tool that answers normally; Hidden exists for a tool whose entire product answer IS refusal
	// (get_tail_logs / get_sagas stay published nowhere on the hub path, AC-17, yet a caller who names
	// them by hand still gets the distinguished refusal text, never a generic "unknown tool").
	Hidden bool
}

// okPayload / toolErr / rpcErr are small Outcome constructors used by handlers.
func Ok(payload any) Outcome           { return Outcome{Payload: payload} }
func ToolErr(payload any) Outcome      { return Outcome{Payload: payload, IsError: true} }
func RPC(code int, msg string) Outcome { return Outcome{RPCErr: &RPCError{Code: code, Message: msg}} }
