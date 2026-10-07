package mcpserver

import (
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/OneDro1d/argus-runner/internal/argus"
	"github.com/OneDro1d/argus-runner/internal/auth"
	"github.com/OneDro1d/argus-runner/internal/role"
)

type sessState int

const (
	sConnected    sessState = iota // GET /sse done; awaiting initialize
	sInitializing                  // initialize received; awaiting notifications/initialized
	sReady                         // bootup complete; tools callable
)

type session struct {
	state    sessState
	lastSeen time.Time // S8 (CP-M3-123): eviction clock — touched on every Dispatch
}

// maxSessions bounds the session map (S8, the gate-118 MED: entries were planted pre-auth and never
// evicted on the streamable path → unbounded growth under an unauthenticated flood). On overflow the
// stalest ~10% are evicted; an evicted live streamable client self-heals via AdoptSession on its
// next POST (the SSE transport ends its session on disconnect as before).
const maxSessions = 4096

// Server is the hand-rolled JSON-RPC MCP server. It is transport-agnostic at the
// Dispatch level (the HTTP+SSE transport in transport.go is a thin wrapper); this
// makes the protocol behaviour unit-testable without a live socket.
type Server struct {
	auth  Authenticator
	tools map[string]Tool
	order []string // registration order, for a stable tools/list

	mu        sync.Mutex
	sessions  map[string]*session
	runActive bool // single-tenant run-state guard (DF-DEC-M25-01)
	metrics   *metrics

	// RunBeginner, when set (the CP-wired serve process), is called SYNCHRONOUSLY before the async
	// runner__run dispatch with the minted run_id + the computed scope — the W1 direct-path fence
	// (M3.1 §D-3.1.2/UC188): nil return = the CP instance_run_lock is held, proceed; an error =
	// REFUSE the run (the caller sees the distinguished run-in-progress error and the local lock is
	// released). Nil field (standalone / non-cloud) = unchanged M2.5 behavior. The wrapper in
	// cmd/argus maps a CP transport failure to nil (proceed offline — the documented residual,
	// the local runActive lock is the second line) and only a real 409 to an error.
	RunBeginner func(runID, scope string) error
	// RunDone, when set, is called EXACTLY ONCE when the background runner__run handler returns:
	// ok=false on an ERROR outcome (infra/tool error — toolcore never reached the report-up push,
	// so the wrapper pushes a minimal terminal 'failed' that releases the CP lock and records the
	// aborted run honestly), ok=true on a clean outcome (the run's own terminal push released; the
	// wrapper only stops the mid-run heartbeat ticker it started at RunBeginner time — W1/3).
	RunDone func(runID string, ok bool)
	// RunPreflight, when set, is called SYNCHRONOUSLY before a runner__run is accepted, with the
	// call's selection. A non-nil error REFUSES the run and the caller is told why immediately.
	//
	// It exists because runner__run is ASYNC: without it, a run that can only ever execute nothing
	// still returns a run_id, and the agent has to infer the problem from a later report showing
	// total:0 — which reads like a run that happened and found nothing wrong. The requirement is
	// that the agent SAY the run cannot be run, so the answer must be available at call time.
	// Nil field = unchanged behavior.
	RunPreflight func(layer, tag, scenarioID string) error
}

// staticAuth wraps a static-token auth.Config as an Authenticator (the M2.5 path — hat only, no
// workspace/subject).
type staticAuth struct{ cfg auth.Config }

func (s staticAuth) Authenticate(token string) (Principal, error) {
	hat, err := s.cfg.Verify(token)
	if err != nil {
		return Principal{}, err
	}
	return Principal{Hat: hat}, nil
}

// NewServer validates the static-token auth config (DF-DEC-M25-05: refuse to start on an ambiguous
// token configuration) and registers the tools. The M2.5 entry point.
func NewServer(authCfg auth.Config, tools ...Tool) (*Server, error) {
	if err := authCfg.Validate(); err != nil {
		return nil, err
	}
	return NewServerWithAuth(staticAuth{authCfg}, tools...)
}

// NewServerWithAuth registers the tools behind a custom Authenticator (the M3 cloud path: static
// tokens + own-OAuth access tokens).
func NewServerWithAuth(a Authenticator, tools ...Tool) (*Server, error) {
	if a == nil {
		return nil, fmt.Errorf("mcpserver: nil authenticator")
	}
	s := &Server{
		auth:     a,
		tools:    make(map[string]Tool, len(tools)),
		sessions: make(map[string]*session),
		metrics:  newMetrics(),
	}
	for _, t := range tools {
		if _, dup := s.tools[t.Name]; dup {
			return nil, fmt.Errorf("mcpserver: duplicate tool %q", t.Name)
		}
		s.tools[t.Name] = t
		if !t.Hidden {
			s.order = append(s.order, t.Name)
		}
	}
	// Aliases are added in a SECOND pass so an alias can never shadow a real tool registered later
	// in the list — one pass would make the outcome depend on declaration order, which is precisely
	// the kind of silent, ordering-dependent behaviour a scope boundary must not have.
	for _, t := range tools {
		for _, a := range t.Aliases {
			if _, dup := s.tools[a]; dup {
				return nil, fmt.Errorf("mcpserver: alias %q of tool %q collides with an existing tool", a, t.Name)
			}
			// The alias dispatches to the SAME Tool value, so it carries the same Namespace and
			// therefore the same scope check. A back-compat name must not be a back door.
			s.tools[a] = t
		}
	}
	return s, nil
}

// NewSession registers a freshly-connected session (the SSE connect mints the id).
// AdoptSession makes a CLIENT-SUPPLIED session id servable on THIS replica (R3/R2 stateless
// sessions, M3.1 §D-3.1.2/CP-M3-118): a streamable-HTTP client whose initialize handshake landed on
// another replica arrives here with an Mcp-Session-Id this process has never seen — it is adopted
// as READY (auth is per-request Bearer; the handshake state machine negotiates protocol only and
// carries no security). The legacy SSE transport never adopts (its stream IS the session; the R3
// decision routes it with gateway cookie-affinity).
//
// CP-M3-III-9: it also PROMOTES a session this replica already knows but has not seen finish the
// handshake. The old "a session id already known keeps its state" rule made the ORIGIN replica the
// single strict one in the fleet, and behind a round-robin gateway that is a live data-loss bug:
//
//	initialize                -> replica A   (A: sConnected -> sInitializing)
//	notifications/initialized -> replica B   (B adopts as sReady; A never learns)
//	tools/call                -> replica A   -> -32003
//
// Measured on example-cluster (3 replicas, 2026-08-04): 30 same-session tools/list -> 20 ok /
// 10 -32003, and the onboarder's bulk scenario import silently lost 9 of 27 individually-VALID
// scenarios, leaving an instance that looks healthy with a third of its catalog missing.
//
// Promotion adds no capability an attacker did not already have: an unknown id is adopted as ready
// anyway, so any client could always mint a servable session simply by inventing an id. What it
// removes is replicas DISAGREEING about the same id. The handshake requirement itself survives
// where it is meaningful — a request presenting NO session id still gets a fresh sConnected session
// and the VR-H8/UC-65 error (TestStreamable_NoSessionHeaderStillRefusesToolsCall), and the strict
// SSE path is untouched.
func (s *Server) AdoptSession(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess, ok := s.sessions[id]; ok {
		sess.state = sReady // promote: never let this replica be stricter than the ones that adopt
		sess.lastSeen = time.Now()
		return
	}
	s.evictIfFullLocked()
	s.sessions[id] = &session{state: sReady, lastSeen: time.Now()}
}

func (s *Server) NewSession(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evictIfFullLocked()
	s.sessions[id] = &session{state: sConnected, lastSeen: time.Now()}
}

// evictIfFullLocked (S8, CP-M3-123) bounds the session map: at maxSessions the stalest ~10% go.
// Callers hold s.mu.
func (s *Server) evictIfFullLocked() {
	if len(s.sessions) < maxSessions {
		return
	}
	type cand struct {
		id string
		at time.Time
	}
	cands := make([]cand, 0, len(s.sessions))
	for id, sess := range s.sessions {
		cands = append(cands, cand{id, sess.lastSeen})
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].at.Before(cands[j].at) })
	for i := 0; i < len(cands)/10+1; i++ {
		delete(s.sessions, cands[i].id)
	}
}

// EndSession drops a session (SSE disconnect).
func (s *Server) EndSession(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, id)
}

// Metrics renders the server-side tool-call metrics in Prometheus text format.
func (s *Server) Metrics() string { return s.metrics.render() }

// CheckAuth reports whether token authenticates at all — no / wrong / unknown / expired credential —
// WITHOUT dispatching a method. It exists so the HTTP transport (D-CP-MCP.AUTH / the MCP authorization
// spec) can answer 401 + WWW-Authenticate BEFORE it decides what to write, while Dispatch itself still
// runs the SAME Authenticate call and still produces the JSON-RPC -32001 body those transports also
// keep: a client that only reads the JSON-RPC error, not the HTTP status, must see no change.
func (s *Server) CheckAuth(token string) error {
	_, err := s.auth.Authenticate(token)
	return err
}

// Dispatch handles one JSON-RPC message for a session. It returns the response
// bytes and whether there is a response at all (notifications return false). Auth
// is enforced here (the matched token IS the hat); the transport supplies the
// bearer token and the session id.
func (s *Server) Dispatch(sessionID, token string, raw []byte) (resp []byte, hasResp bool) {
	var req Request
	if err := json.Unmarshal(raw, &req); err != nil {
		return s.encode(nil, nil, &RPCError{Code: CodeParse, Message: "parse error"}), true
	}
	isNotification := req.ID == nil

	// S8: touch the session's eviction clock (an active client is never the stalest).
	s.mu.Lock()
	if sess, ok := s.sessions[sessionID]; ok {
		sess.lastSeen = time.Now()
	}
	s.mu.Unlock()

	prin, autherr := s.auth.Authenticate(token)
	if autherr != nil {
		if isNotification {
			return nil, false // notifications get no response; transport already 202'd
		}
		return s.encode(req.ID, nil, &RPCError{
			Code: CodeUnauthorized, Message: "unauthorized: " + autherr.Error(),
			Data: map[string]any{"instance_id": "local"},
		}), true
	}

	switch req.Method {
	case "initialize":
		return s.handleInitialize(sessionID, req), true
	case "notifications/initialized":
		s.markReady(sessionID)
		return nil, false
	case "tools/list":
		return s.handleToolsList(sessionID, req, prin), true
	case "tools/call":
		return s.handleToolsCall(sessionID, req, prin), true
	default:
		return s.encode(req.ID, nil, &RPCError{Code: CodeMethodNotFound, Message: "unknown method " + req.Method}), true
	}
}

func (s *Server) handleInitialize(sessionID string, req Request) []byte {
	s.mu.Lock()
	sess, ok := s.sessions[sessionID]
	if !ok { // tolerate a direct initialize (mint the session) for non-SSE callers/tests
		sess = &session{}
		s.sessions[sessionID] = sess
	}
	sess.state = sInitializing
	s.mu.Unlock()
	return s.encode(req.ID, map[string]any{
		"protocolVersion": ProtocolVersion,
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      map[string]any{"name": "argus", "version": "M2.5"},
	}, nil)
}

func (s *Server) markReady(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess, ok := s.sessions[sessionID]; ok && sess.state == sInitializing {
		sess.state = sReady
	}
}

func (s *Server) sessionReady(sessionID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[sessionID]
	return ok && sess.state == sReady
}

// visibleTools returns the scope-filtered tool list for a hat (DF-DEC-M25-02): the product hat
// sees only runner__ tools; the test hat additionally sees any author__ tools registered on this
// server. Post-C1 the IN-ENV server registers ONLY runner__ tools (both hats see the same 6);
// the author__ scope filter still governs the CLOUD server (control.CloudTools), where the test
// hat sees the author__ tools and a runner-scope credential sees none.
func (s *Server) visibleTools(hat role.Role) []Tool {
	out := make([]Tool, 0, len(s.order))
	for _, name := range s.order {
		t := s.tools[name]
		if t.Namespace == NSAuthor && !hat.CanAccessScenarios() {
			continue
		}
		// AC-16: the executor namespace is a THIRD, disjoint scope — visible ONLY to the executor
		// hat, never additive to runner or author (a runner__/author__ credential must never learn
		// the federation verbs exist, and an executor credential must never learn the reverse).
		if t.Namespace == NSExecutor && hat != role.Executor {
			continue
		}
		out = append(out, t)
	}
	return out
}

// visibleToolsForPrin is visibleTools extended with the ONE namespace whose visibility depends on
// more than the hat: NSBuilder (AC-17) needs the WORKSPACE too (see types.go's NSBuilder doc) — a
// local, workspace-less product token must never see the relay, only the hub's own runner-scope one.
func (s *Server) visibleToolsForPrin(prin Principal) []Tool {
	out := make([]Tool, 0, len(s.order))
	for _, t := range s.visibleTools(prin.Hat) {
		if t.Namespace == NSBuilder && !isBuilder(prin) {
			continue
		}
		out = append(out, t)
	}
	return out
}

// isBuilder reports whether prin presents the builder-relay principal shape (AC-17): the runner-scope
// credential a hub member holds resolves today to Principal{Hat: role.Product, Workspace, Subject}
// (cloudauth.go) — the SAME hat the in-environment product agent carries, distinguished by a
// non-empty workspace, since a local static product token never carries one.
//
// adds ONE workspace-less builder: the opt-in owner-wide PAT, which the control plane's
// authenticator marks OwnerWide and gives a Subject. The marker is required, not just a Subject: the
// local router builds Product principals whose Subject is the router token and whose workspace is empty
// (router/proxy.go), and an own-OAuth runner token can carry a subject with no workspace claim. Neither
// carries the marker, so neither is a builder, whatever its Subject.
func isBuilder(prin Principal) bool {
	if prin.Hat != role.Product {
		return false
	}
	return prin.Workspace != "" || (prin.OwnerWide && prin.Subject != "")
}

// VisibleToolsFor is what `tools/list` would return for a hat — the SAME filter, exported so a
// caller can assert the boundary without driving a full handshake.
//
// Added for the M3-FX local router (VR-R7): its holdout test must check the tools a product-agent
// folder can see against the REAL filter, not against a re-implementation of it. A test that asserts
// its own copy of the rule proves only that the copy agrees with itself.
func (s *Server) VisibleToolsFor(hat role.Role) []Tool { return s.visibleTools(hat) }

// VisibleToolsForPrincipal is VisibleToolsFor extended with the workspace check NSBuilder needs
// (AC-17) — exported for the same reason: a test asserting the builder-relay boundary should drive
// the REAL filter, not a copy of it.
func (s *Server) VisibleToolsForPrincipal(prin Principal) []Tool { return s.visibleToolsForPrin(prin) }

func (s *Server) handleToolsList(sessionID string, req Request, prin Principal) []byte {
	if !s.sessionReady(sessionID) {
		return s.encode(req.ID, nil, &RPCError{Code: CodeNoSession, Message: "no initialized session; complete the handshake first"})
	}
	type listed struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		InputSchema map[string]any `json:"inputSchema"`
	}
	// Never nil: a principal who may see no tool gets `"tools": []`. A null here is rejected by strict
	// clients (Claude Code) the same way a null inputSchema.required is.
	tools := []listed{}
	for _, t := range s.visibleToolsForPrin(prin) {
		tools = append(tools, listed{t.Name, t.Description, t.InputSchema})
	}
	return s.encode(req.ID, map[string]any{"tools": tools}, nil)
}

type callParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

func (s *Server) handleToolsCall(sessionID string, req Request, prin Principal) []byte {
	if !s.sessionReady(sessionID) {
		// VR-H8 / UC-65: a call before initialize (or on an unknown/expired session)
		// is a protocol-plane error, before any tool runs and with no side effect.
		return s.encode(req.ID, nil, &RPCError{Code: CodeNoSession, Message: "tools/call before initialize or unknown session"})
	}
	var p callParams
	if err := json.Unmarshal(req.Params, &p); err != nil || p.Name == "" {
		return s.encode(req.ID, nil, &RPCError{Code: CodeInvalidParams, Message: "invalid params: need {name, arguments}"})
	}
	t, ok := s.tools[p.Name]
	if !ok {
		return s.encode(req.ID, nil, &RPCError{Code: CodeMethodNotFound, Message: "unknown tool " + p.Name})
	}
	// scope check (VR-AUTH9 / VR-I3): author tools require the test hat.
	if t.Namespace == NSAuthor && !prin.Hat.CanAccessScenarios() {
		s.metrics.inc(p.Name, instanceOf(p.Arguments), "denied")
		return s.encode(req.ID, nil, &RPCError{
			Code: CodeUnauthorized, Message: "not permitted for the product scope: " + p.Name,
			Data: map[string]any{"instance_id": instanceOf(p.Arguments)},
		})
	}
	// AC-16: executor tools require the executor hat — refused BY NAME to a runner or author
	// credential exactly like the author check above, not merely hidden from tools/list.
	if t.Namespace == NSExecutor && prin.Hat != role.Executor {
		s.metrics.inc(p.Name, instanceOf(p.Arguments), "denied")
		return s.encode(req.ID, nil, &RPCError{
			Code: CodeUnauthorized, Message: "not permitted outside the executor scope: " + p.Name,
			Data: map[string]any{"instance_id": instanceOf(p.Arguments)},
		})
	}
	// AC-17: the builder relay requires the builder principal shape (isBuilder) — refused BY NAME to
	// everyone else, including the in-environment product hat's own local (workspace-less) token.
	if t.Namespace == NSBuilder && !isBuilder(prin) {
		s.metrics.inc(p.Name, instanceOf(p.Arguments), "denied")
		return s.encode(req.ID, nil, &RPCError{
			Code: CodeUnauthorized, Message: "not permitted outside the builder scope: " + p.Name,
			Data: map[string]any{"instance_id": instanceOf(p.Arguments)},
		})
	}
	// VR10-S1 (V28-015): refuse every argument the tool does not declare. AFTER auth and the scope
	// check (an unauthorised caller must not learn a tool's argument list), BEFORE PreCheck, the run
	// lock, run-id minting and injectRunID — a refused call must cost nothing, and reading the
	// CALLER's bytes means the server never rejects its own run_id. Both planes share this
	// dispatcher, so one check covers every tool whose schema closes its object (both plane builders
	// do; the local router's proxy schemas stay open and forward — the upstream refuses). The refusal
	// is a TOOL error naming the unknown, the accepted (required marked) and the nearest match; a
	// suggestion is never an acceptance.
	if unknown, accepted, uerr := UnknownArgs(t.InputSchema, p.Arguments); uerr == nil && len(unknown) > 0 {
		s.metrics.inc(p.Name, instanceOf(p.Arguments), "rejected")
		o := RefuseUnknown(p.Name, unknown, accepted, RequiredArgs(t.InputSchema))
		text, _ := json.Marshal(o.Payload)
		return s.encode(req.ID, ToolResult{Content: []ContentBlock{{Type: "text", Text: string(text)}}, IsError: true}, nil)
	}
	// run-state guard (VR-H17 / DF-DEC-M25-01): reject a concurrent runner__run.
	// DF-06: runner__run is ASYNC — acquire the run lock, execute the handler in the
	// BACKGROUND (holding the lock for the whole run so a concurrent run is still rejected),
	// and return {status:"running", run_id} IMMEDIATELY so the MCP client never times out.
	// The caller polls runner__get_report{run_id} (DF-07) for the terminal report.
	if t.Async {
		// ARGUMENT validation comes first of all (gate-2026-08-06). The handler's own guard runs
		// inside the goroutine, i.e. AFTER the run has been announced, locked and fenced — and its
		// Outcome is discarded there, so a bad argument surfaced as {"status":"running"} plus a
		// run_id that never resolves. Refuse it synchronously instead.
		if t.PreCheck != nil {
			if o := t.PreCheck(p.Arguments, prin); o != nil {
				s.metrics.inc(p.Name, instanceOf(p.Arguments), "rejected")
				if o.RPCErr != nil {
					return s.encode(req.ID, nil, o.RPCErr)
				}
				text, _ := json.Marshal(o.Payload)
				return s.encode(req.ID, ToolResult{
					Content: []ContentBlock{{Type: "text", Text: string(text)}},
					IsError: o.IsError,
				}, nil)
			}
		}
		// Refuse a run that would execute NOTHING before anything else happens — before the local
		// lock, before the CP fence, before a run_id exists. There is no run to report on, so the
		// caller gets the reason directly instead of a run_id to poll.
		if s.RunPreflight != nil {
			if perr := s.RunPreflight(layerOf(p.Arguments), tagOf(p.Arguments), scenarioRefOf(p.Arguments)); perr != nil {
				s.metrics.inc(p.Name, instanceOf(p.Arguments), "rejected")
				return s.encode(req.ID, nil, &RPCError{Code: CodeInvalidParams, Message: perr.Error()})
			}
		}
		if !s.acquireRun() {
			s.metrics.inc(p.Name, instanceOf(p.Arguments), "rejected")
			return s.encode(req.ID, nil, &RPCError{Code: CodeRunInProgress, Message: "run already in progress on this instance"})
		}
		runID := newRunID()
		// The W1 CP fence (§D-3.1.2/UC188): acquire the CP-side instance_run_lock BEFORE any SUT
		// effect. A busy CP → refuse (release the local lock, distinguished error); nil hook =
		// standalone, unchanged.
		if s.RunBeginner != nil {
			if berr := s.RunBeginner(runID, scopeOf(p.Arguments)); berr != nil {
				s.releaseRun()
				s.metrics.inc(p.Name, instanceOf(p.Arguments), "rejected")
				return s.encode(req.ID, nil, &RPCError{Code: CodeRunInProgress,
					Message: "instance busy — a run is already in flight for this instance: " + berr.Error()})
			}
		}
		args := injectRunID(p.Arguments, runID)
		s.metrics.inc(p.Name, instanceOf(p.Arguments), "ok")
		go func() {
			defer s.releaseRun()
			out := t.Handler(args, prin) // toolcore.Run in the background; stamps report.run_id = runID
			// RunDone always fires (stops the heartbeat ticker); ok=false on an ERROR outcome means
			// toolcore never reached its terminal report-up push — the wrapper releases the CP lock
			// via a minimal terminal 'failed' push so the fence cannot leak on a graceful infra
			// failure (§D-3.1.2; the watchdog covers hard crashes).
			if s.RunDone != nil {
				s.RunDone(runID, out.RPCErr == nil && !out.IsError)
			}
		}()
		body, _ := json.Marshal(map[string]any{"status": "running", "run_id": runID, "instance": "local",
			"note": "run started in the background — poll runner__get_report{run_id} until status is passed/failed"})
		return s.encode(req.ID, ToolResult{Content: []ContentBlock{{Type: "text", Text: string(body)}}}, nil)
	}

	out := t.Handler(p.Arguments, prin)
	if out.RPCErr != nil {
		s.metrics.inc(p.Name, instanceOf(p.Arguments), "error")
		return s.encode(req.ID, nil, out.RPCErr)
	}
	text, _ := json.Marshal(out.Payload)
	outcome := "ok"
	if out.IsError {
		outcome = "tool_error"
	}
	s.metrics.inc(p.Name, instanceOf(p.Arguments), outcome)
	return s.encode(req.ID, ToolResult{
		Content: []ContentBlock{{Type: "text", Text: string(text)}},
		IsError: out.IsError,
	}, nil)
}

func (s *Server) acquireRun() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runActive {
		return false
	}
	s.runActive = true
	return true
}

func (s *Server) releaseRun() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runActive = false
}

// newRunID is the run-level handle returned by an async runner__run (DF-06). It uses the same
// shortest UTC date-time format as the CLI (argus.NewRunID), so the run_id — and the
// correlation ids that embed it (tr-<run_id>-<scenario_id>-<hex>) — are uniform across BOTH
// entry points (CLI + MCP server).
func newRunID() string { return argus.NewRunID() }

// scopeOf computes a runner__run call's scope from its selection arguments — the same precedence as
// toolcore's runScope (single > tag > layer > full), inlined to keep the protocol layer decoupled.
// selectionOf pulls the run selection out of a runner__run call's arguments — the same three
// fields scopeOf classifies, returned verbatim for RunPreflight.
func selectionOf(args json.RawMessage) (layer, tag, scenarioRef string) {
	var a struct {
		ScenarioRef string `json:"scenario_ref"`
		Tag         string `json:"tag"`
		Layer       string `json:"layer"`
	}
	_ = json.Unmarshal(args, &a)
	return a.Layer, a.Tag, a.ScenarioRef
}

func layerOf(args json.RawMessage) string       { l, _, _ := selectionOf(args); return l }
func tagOf(args json.RawMessage) string         { _, t, _ := selectionOf(args); return t }
func scenarioRefOf(args json.RawMessage) string { _, _, r := selectionOf(args); return r }

func scopeOf(args json.RawMessage) string {
	var a struct {
		ScenarioRef string `json:"scenario_ref"`
		Tag         string `json:"tag"`
		Layer       string `json:"layer"`
	}
	_ = json.Unmarshal(args, &a)
	switch {
	case a.ScenarioRef != "":
		return "single"
	case a.Tag != "":
		return "tag"
	case a.Layer != "":
		return "layer"
	default:
		return "full"
	}
}

// injectRunID adds run_id to the tool-call arguments object so the background runner__run
// handler stamps it into the report (DF-06/07). Best-effort: returns args unchanged on error.
func injectRunID(args json.RawMessage, runID string) json.RawMessage {
	m := map[string]any{}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &m); err != nil {
			return args
		}
	}
	m["run_id"] = runID
	if b, err := json.Marshal(m); err == nil {
		return b
	}
	return args
}

func (s *Server) encode(id json.RawMessage, result any, rpcErr *RPCError) []byte {
	r := Response{JSONRPC: "2.0", ID: id, Result: result, Error: rpcErr}
	b, _ := json.Marshal(r)
	return b
}

// instanceOf best-effort extracts instance_id from a tool's arguments (default "local").
func instanceOf(args json.RawMessage) string {
	var m struct {
		InstanceID string `json:"instance_id"`
	}
	if err := json.Unmarshal(args, &m); err == nil && m.InstanceID != "" {
		return m.InstanceID
	}
	return "local"
}

// --- metrics (VR-H11): argus_mcp_tool_calls_total / _duration_seconds ---

type metrics struct {
	mu    sync.Mutex
	calls map[string]int // key "tool|instance|outcome"
}

func newMetrics() *metrics { return &metrics{calls: map[string]int{}} }

func (m *metrics) inc(tool, instance, outcome string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls[tool+"|"+instance+"|"+outcome]++
}

func (m *metrics) snapshot() map[string]int {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]int, len(m.calls))
	for k, v := range m.calls {
		out[k] = v
	}
	return out
}

func (m *metrics) render() string {
	keys := make([]string, 0)
	snap := m.snapshot()
	for k := range snap {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := "# HELP argus_mcp_tool_calls_total MCP tool calls.\n# TYPE argus_mcp_tool_calls_total counter\n"
	for _, k := range keys {
		parts := splitPipe(k)
		out += fmt.Sprintf("argus_mcp_tool_calls_total{tool=%q,instance_id=%q,outcome=%q} %d\n", parts[0], parts[1], parts[2], snap[k])
	}
	return out
}

func splitPipe(s string) [3]string {
	var r [3]string
	i := 0
	start := 0
	for j := 0; j < len(s) && i < 2; j++ {
		if s[j] == '|' {
			r[i] = s[start:j]
			i++
			start = j + 1
		}
	}
	r[2] = s[start:]
	return r
}
