package mcpserver

import (
	"encoding/json"
	"log"
	"time"
)

// Per-session tools (AC-D69, design B2) — used by the local router only. The executor and the control plane leave
// Server.SessionTools and Server.UnknownTool nil and serve their static tools exactly as before.
//
// WHY A SERVER WOULD WANT THEM. The router passes on the tools other servers define (the control plane's author tools
// today, each executor's runner tools in part 2), so what one agent sees depends on WHICH folder it is and on what those
// servers listed when its session started. A tool list fixed when the process starts cannot express either.

// SessionSet is one session's tools, as the SessionTools hook built them for one caller.
type SessionSet struct {
	// Tools is the set the session lists and dispatches. A nil Tools is an empty set.
	Tools *ToolSet
	// Instructions, when not empty, is returned in initialize's "instructions" field: the session-start text that
	// explains a part whose tools are missing (design A6). MCP clients pass it to the model (Claude Code does).
	Instructions string
	// State is opaque to the server: stored with the set by reference, never copied or read here, and handed back to
	// UnknownTool. Whatever builds it owns its synchronisation — handlers and UnknownTool of one session may run at once.
	State any
	// RetryAfter, when > 0, lets a set that was built on FIRST USE (a session adopted without an initialize, so its
	// client never received Instructions) be built again once it is that old. The builder sets it when a part failed:
	// a session adopted during a short outage would otherwise keep the outage for its whole life. A set built at
	// initialize is kept for the session regardless — its client was told to reconnect.
	RetryAfter time.Duration
}

// ToolSet is an immutable, indexed set of tools. It is built once and may be shared by every session that lists the
// same tools: the session stores a pointer, so a thousand sessions of one folder hold one copy of the definitions.
type ToolSet struct {
	byName map[string]Tool // names AND aliases
	order  []string        // listed (non-hidden) names, in the given order
}

// NewToolSet indexes tools with NewServerWithAuth's rules — every name first, then every alias in a second pass, so an
// alias can never shadow a tool — except that a conflict is SKIPPED rather than refused: a list read from another
// server at session start must not be able to fail the whole session. Each skipped name or alias is returned so the
// caller can say so; the first occurrence of a name wins.
func NewToolSet(tools ...Tool) (*ToolSet, []string) {
	ts := &ToolSet{byName: make(map[string]Tool, len(tools))}
	var skipped []string
	accepted := make([]bool, len(tools))
	for i, t := range tools {
		if _, dup := ts.byName[t.Name]; dup || t.Name == "" {
			skipped = append(skipped, t.Name)
			continue
		}
		accepted[i] = true
		ts.byName[t.Name] = t
		if !t.Hidden {
			ts.order = append(ts.order, t.Name)
		}
	}
	for i, t := range tools {
		if !accepted[i] {
			continue // a skipped entry brings no aliases either
		}
		for _, a := range t.Aliases {
			if _, dup := ts.byName[a]; dup {
				skipped = append(skipped, a)
				continue
			}
			ts.byName[a] = t
		}
	}
	return ts, skipped
}

// Names lists the set's listed tools, in order (aliases and hidden tools are not listed).
func (ts *ToolSet) Names() []string {
	if ts == nil {
		return nil
	}
	return append([]string(nil), ts.order...)
}

// Tools returns the set's listed tools, in order.
func (ts *ToolSet) Tools() []Tool {
	if ts == nil {
		return nil
	}
	out := make([]Tool, 0, len(ts.order))
	for _, n := range ts.order {
		out = append(out, ts.byName[n])
	}
	return out
}

func (ts *ToolSet) lookup(name string) (Tool, bool) {
	if ts == nil {
		return Tool{}, false
	}
	t, ok := ts.byName[name]
	return t, ok
}

var emptyToolSet = &ToolSet{byName: map[string]Tool{}}

// buildFailedText is what a session gets when the SessionTools hook itself failed (panicked). It holds nothing the
// hook was working on — no session id, no caller, no credential.
const buildFailedText = "Argus could not build this session's tool list. Reconnect (in Claude Code: /mcp)."

// setKey identifies one stored set: (session id, the caller's Subject). Bound to the CALLER, not only the session: session
// ids come from the client and an unknown one is adopted, and every message is authenticated on its own, so a message
// whose token resolves to another subject must never be served another subject's set. ⚠ For the router the Subject is
// the router token itself — a key is never formatted into any text or log.
type setKey struct{ sid, subject string }

type storedSet struct {
	set      SessionSet
	gen      uint64
	built    time.Time
	firstUse bool
	attempt  int // failed first-use rebuilds in a row: each one doubles the wait before the next
}

type buildCall struct {
	done   chan struct{}
	gen    uint64
	result SessionSet
}

// maxRetryDoublings caps the back-off of a failing first-use set: RetryAfter × 2^5 at most (16 minutes for 30 s).
const maxRetryDoublings = 5

// sessionSetFor returns the caller's set for this session, building it when there is none (or when fresh is set:
// initialize always builds anew, so a tool added upstream appears at the next session start).
//
// The hook runs with NO server lock held — it makes network calls, up to its own timeout — and at most one build runs per
// (session, subject): a second caller, initialize included, waits for the build in flight and takes its result. The
// waiting happens with no lock held either, so a slow build never blocks another session.
//
// A stored set whose RetryAfter has passed is NOT rebuilt on the request path: the request is answered with the stored
// set at once and the rebuild runs in the background. Otherwise a session adopted during an outage would hold every one
// of its calls — runner calls included, which do not depend on the control plane at all — for the hook's whole timeout,
// again and again for as long as the outage lasts.
func (s *Server) sessionSetFor(sessionID string, prin Principal, fresh bool) SessionSet {
	key := setKey{sessionID, prin.Subject}
	s.mu.Lock()
	if !fresh {
		if sess, ok := s.sessions[sessionID]; ok {
			if st, ok := sess.sets[prin.Subject]; ok {
				if st.expired(time.Now()) {
					if _, inFlight := s.building[key]; !inFlight {
						c := s.startBuildLocked(key)
						go s.runBuild(sessionID, prin, c, false, st.attempt+1)
					}
				}
				set := st.set
				s.mu.Unlock()
				return set
			}
		}
	}
	if c, ok := s.building[key]; ok {
		s.mu.Unlock()
		<-c.done
		return c.result
	}
	c := s.startBuildLocked(key)
	s.mu.Unlock()
	return s.runBuild(sessionID, prin, c, fresh, 0)
}

// startBuildLocked registers a build in flight for key. Callers hold s.mu.
func (s *Server) startBuildLocked(key setKey) *buildCall {
	if s.building == nil {
		s.building = map[setKey]*buildCall{}
	}
	s.buildGen++
	c := &buildCall{done: make(chan struct{}), gen: s.buildGen}
	s.building[key] = c
	return c
}

// runBuild runs the hook with no lock held, stores the result, and releases every caller waiting on c.
func (s *Server) runBuild(sessionID string, prin Principal, c *buildCall, fresh bool, attempt int) SessionSet {
	set, ok := s.runSessionTools(sessionID, prin)

	s.mu.Lock()
	// Store only a set the hook really built, only into a session that still exists (one evicted meanwhile is answered
	// with this set and not stored), and never over a newer build's set.
	if sess, present := s.sessions[sessionID]; ok && present {
		if sess.sets == nil {
			sess.sets = map[string]*storedSet{}
		}
		if cur, had := sess.sets[prin.Subject]; !had || cur.gen < c.gen {
			st := &storedSet{set: set, gen: c.gen, built: time.Now(), firstUse: !fresh}
			if set.RetryAfter > 0 {
				st.attempt = attempt
			}
			sess.sets[prin.Subject] = st
		}
	}
	key := setKey{sessionID, prin.Subject}
	if s.building[key] == c {
		delete(s.building, key)
	}
	c.result = set
	s.mu.Unlock()
	close(c.done)
	return set
}

// expired says whether a first-use set that asked for a retry is due for one: RetryAfter after it was built, doubled for
// every failed rebuild in a row (up to maxRetryDoublings), so a long outage costs the upstream a few calls, not one per
// RetryAfter per session.
func (st *storedSet) expired(now time.Time) bool {
	if !st.firstUse || st.set.RetryAfter <= 0 {
		return false
	}
	return now.Sub(st.built) >= st.set.RetryAfter<<min(st.attempt, maxRetryDoublings)
}

// runSessionTools calls the hook in THIS goroutine and turns a panic into an empty set with buildFailedText, so the
// waiters are released and the server keeps serving every other session. ok=false means nothing may be stored.
func (s *Server) runSessionTools(sessionID string, prin Principal) (set SessionSet, ok bool) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("mcpserver: the session tool hook failed: %v", r)
			set, ok = SessionSet{Tools: emptyToolSet, Instructions: buildFailedText}, false
		}
	}()
	set = s.SessionTools(sessionID, prin)
	if set.Tools == nil {
		set.Tools = emptyToolSet
	}
	return set, true
}

// unlistedLabel is the metrics label for an answer UnknownTool gave: a fixed value, so names a caller invents can never
// grow the metrics map.
const unlistedLabel = "(unlisted)"

// encodeOutcome is the ONE encoder for a handler's Outcome and for UnknownTool's: Result wins (passed back as the
// JSON-RPC result, as given), then RPCErr (a JSON-RPC error), then Payload/IsError (wrapped in one text block, as
// always). Metrics are counted under label.
func (s *Server) encodeOutcome(id json.RawMessage, label string, args json.RawMessage, out Outcome) []byte {
	switch {
	case out.Result != nil:
		if !json.Valid(out.Result) {
			s.metrics.inc(label, instanceOf(args), "error")
			return s.encode(id, nil, &RPCError{Code: CodeInternal, Message: "the tool's answer is not valid JSON"})
		}
		outcome := "ok"
		var r struct {
			IsError bool `json:"isError"`
		}
		if json.Unmarshal(out.Result, &r) == nil && r.IsError {
			outcome = "tool_error"
		}
		s.metrics.inc(label, instanceOf(args), outcome)
		return s.encode(id, out.Result, nil)
	case out.RPCErr != nil:
		s.metrics.inc(label, instanceOf(args), "error")
		return s.encode(id, nil, out.RPCErr)
	}
	text, _ := json.Marshal(out.Payload)
	outcome := "ok"
	if out.IsError {
		outcome = "tool_error"
	}
	s.metrics.inc(label, instanceOf(args), outcome)
	return s.encode(id, ToolResult{
		Content: []ContentBlock{{Type: "text", Text: string(text)}},
		IsError: out.IsError,
	}, nil)
}
