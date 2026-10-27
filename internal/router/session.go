package router

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/OneDro1d/argus-runner/internal/mcp"
	"github.com/OneDro1d/argus-runner/internal/mcpserver"
	"github.com/OneDro1d/argus-runner/internal/role"
)

// THE PASS-THROUGH ROUTER, PART 1 — THE AUTHOR TOOLS (AC-D69, design #678 B4).
//
// The router used to keep its own fixed list of 10 author tools, with one shared description and one shared input list,
// copied from a control plane that had 9 at the time and 36 now. It no longer keeps any list of author tools: when an
// agent's session starts, the router reads the tools the control plane lists for this machine's author token and passes
// them on UNCHANGED — the same names, descriptions and input schemas, so an agent behind the router sees what an agent
// connected straight to the control plane sees (decision 1). Calls go back to the control plane unchanged and its
// answers come back unchanged (decision 14). A tool the control plane adds appears at the next session start, with no
// router change (decision 3).
//
// What the router still decides is not a list of tools but a set of rules (A3): a product folder never gets author tools
// and never holds a control-plane token (VR-R4, VR-R7); an instance_id names one instance onboarded into this folder
// (VR-R5, VR-R13); names starting runner_ belong to the executors (decision 13).
//
// The runner tools are passed on the same way since part 2 (AC-D70, runner.go): each instance's executor is read when the
// session starts, and its tools are listed with its own definitions. Every part is read IN PARALLEL — the control plane
// and each executor — so a session start waits for the slowest part, not for their sum.
//
// The session set holds names, definitions and the state of each part — never a URL and never a token (B4). Every call
// finds the folder, its record and the address again from the router token, so a reload, a rotated token or a removed
// folder takes effect on the next call, and one folder's token can never be used for another folder's call.

const (
	// DefaultListTimeout bounds one control-plane list read at session start (B4 "a short timeout per part").
	DefaultListTimeout = 10 * time.Second
	// MaxListTimeout caps ListTimeoutEnv. initialize waits for the read, and an MCP client gives up on a server that does
	// not answer initialize in time (Claude Code: MCP_TIMEOUT, 30 s by default) — losing the runner tools with it.
	MaxListTimeout = 20 * time.Second
	// ListTimeoutEnv overrides DefaultListTimeout: a Go duration ("15s"), in (0, MaxListTimeout].
	ListTimeoutEnv = "ARGUS_ROUTER_LIST_TIMEOUT"
	// DefaultAuthorCallTimeout bounds one author call forwarded to the control plane. It is NOT the client's 30 s default:
	// the router now passes on every author tool, and the control plane sizes its slowest ones (the ledger writes of
	// author_anchor_commit, the relay wait of author_get_report) to answer inside the 120 s an MCP hub allows an upstream
	// (internal/control/cloudtools.go, the author_anchor_commit budget). A shorter bound here would cut those calls off
	// while the control plane is still working, and the router path would answer differently from the direct one.
	DefaultAuthorCallTimeout = 150 * time.Second
	// MinAuthorCallTimeout and MaxAuthorCallTimeout bound AuthorCallTimeoutEnv. The minimum is the bound author calls had
	// before (the client's default); the maximum keeps a control plane that never answers from holding a call forever.
	MinAuthorCallTimeout = 30 * time.Second
	MaxAuthorCallTimeout = 30 * time.Minute
	// AuthorCallTimeoutEnv overrides DefaultAuthorCallTimeout: a Go duration ("5m"), in [MinAuthorCallTimeout,
	// MaxAuthorCallTimeout]. A control plane whose ledger writes are allowed longer (ARGUS_LEDGER_WRITE_TIMEOUT) needs it.
	AuthorCallTimeoutEnv = "ARGUS_ROUTER_AUTHOR_CALL_TIMEOUT"
	// DefaultRunnerCallTimeout bounds one runner call forwarded to an instance's executor (AC-D70). It is NOT the
	// client's 30 s default either: the executor's synchronous part of runner_run and runner_validate_config can take about
	// 65 s — the catalog fetch (internal/toolcore, catalogCallTimeout: retry.Budget + 10 s = 55 s) and then the control
	// plane's BeginRun (10 s, cmd/argus cpWireFence). Under 30 s the agent was told "could not reach" while the executor
	// went on, and a run could start whose run_id the agent never saw.
	DefaultRunnerCallTimeout = 150 * time.Second
	// MinRunnerCallTimeout and MaxRunnerCallTimeout bound RunnerCallTimeoutEnv, as for author calls.
	MinRunnerCallTimeout = 30 * time.Second
	MaxRunnerCallTimeout = 30 * time.Minute
	// RunnerCallTimeoutEnv overrides DefaultRunnerCallTimeout: a Go duration ("5m"), in [MinRunnerCallTimeout,
	// MaxRunnerCallTimeout] — for an executor whose catalog retries are allowed longer.
	RunnerCallTimeoutEnv = "ARGUS_ROUTER_RUNNER_CALL_TIMEOUT"
	// retryFailedFirstUse lets a session adopted during a short outage (a router restart under an open session) recover
	// without a reconnect: its set is built again after this long. See mcpserver.SessionSet.RetryAfter.
	retryFailedFirstUse = 30 * time.Second
	// maxCachedToolSets bounds the shared ToolSets: one per distinct control-plane list, so a handful in practice.
	maxCachedToolSets = 32
)

// The texts an agent gets when the author part cannot be used (design A6). One per cause, and only the first says
// "could not reach". None of them holds a token, a URL or a user — the session's state keeps only the cause.
const (
	textNoRecord        = "No control plane is configured for this folder, so it has no author tools."
	textRecordGone      = "This folder refers to a control-plane record this machine no longer holds — onboard again."
	textNoToken         = "This machine holds no author token for this control plane yet — onboarding mints it at step 8b."
	textCPUnreachable   = "Argus could not reach the control plane when it read the tools for this session, so the author tools are not listed. Make sure the control plane is up, then reconnect (in Claude Code: /mcp)."
	textCPRefused       = "The control plane refused this machine's author token, so the author tools are not listed. Onboard this folder again (onboarding step 8b mints a new token), then reconnect."
	textCPErrorFmt      = "The control plane answered with an error (%s) when Argus read the tools for this session. Try again later, then reconnect."
	textRunnerBreach    = "The control plane listed a runner tool for this machine's author token. That should not happen, so no runner tools are shown in this session. Report it to the Argus team."
	textBecameAvailable = "The author tools became available after this session started. Reconnect (in Claude Code: /mcp) to see them."
	textNotPassedOnFmt  = "The control plane listed %s in a form Argus cannot pass on (%s), so it is not listed in this session. Report it to the Argus team."
)

// cpCouldNotVerify is how the control plane says it could not CHECK a credential — its token store did not answer —
// as opposed to having checked it and found it wrong (internal/control/cloudauth.go). Both arrive as HTTP 401 + -32001;
// telling the operator to onboard again for the first would mint a second token for an account that has one.
const cpCouldNotVerify = "could not verify the credential"

// AuthorCause says why a folder's author plane cannot be used.
type AuthorCause int

const (
	AuthorUsable AuthorCause = iota
	AuthorNotATestFolder
	AuthorNoRecord
	AuthorRecordGone
	AuthorNoToken
)

// AuthorPlaneError is a folder whose author plane cannot be used, and why. Its text is the A6 sentence for the cause.
type AuthorPlaneError struct{ Cause AuthorCause }

func (e *AuthorPlaneError) Error() string {
	switch e.Cause {
	case AuthorNotATestFolder:
		return "router: this folder is registered as a product-agent folder, which carries the runner tools only"
	case AuthorNoRecord:
		return textNoRecord
	case AuthorRecordGone:
		return textRecordGone
	default:
		return textNoToken
	}
}

// AuthorTarget is where this folder's author calls go, and with which token: the control-plane record the folder refers
// to, read LIVE (a reload replaces the record, a rotation replaces its token). It is the one place both the session
// builder and the call path take the address and the credential from.
func (f *Folder) AuthorTarget() (Target, error) {
	switch {
	case !f.Hat.CanAccessScenarios():
		return Target{}, &AuthorPlaneError{AuthorNotATestFolder}
	case f.Cloud == nil:
		return Target{}, &AuthorPlaneError{AuthorNoRecord}
	case f.record == nil:
		return Target{}, &AuthorPlaneError{AuthorRecordGone}
	case strings.TrimSpace(f.record.Token) == "":
		return Target{}, &AuthorPlaneError{AuthorNoToken}
	}
	return Target{URL: f.record.URL, Token: f.record.Token, Plane: "author"}, nil
}

// Options configures the router's MCP server.
type Options struct {
	// HostAlias is the vantage rewrite for loopback URLs (vantage.go); the same value the forwarder uses.
	HostAlias string
	// Transport is how the control plane is addressed; "" = Streamable, as MCPForwarder.
	Transport mcp.Transport
	// ListTimeout bounds one list read at session start; 0 = DefaultListTimeout.
	ListTimeout time.Duration
	// CallTimeout bounds one forwarded author call; 0 = DefaultAuthorCallTimeout.
	CallTimeout time.Duration
	// RunnerCallTimeout bounds one forwarded runner call (each call of a stale-name retry too); 0 =
	// DefaultRunnerCallTimeout. List reads keep ListTimeout.
	RunnerCallTimeout time.Duration
	// Logf receives the router's own warnings (a list that could not be read, a schema not passed on, decision 13). It
	// is given folder paths and causes only — never a token. nil = no logging.
	Logf func(format string, args ...any)
	// Version is what the router reports as serverInfo.version (AC-D70, design #678 B2, A7); "" = the old "M2.5".
	Version string
}

// ListTimeoutFromEnv reads ListTimeoutEnv. An empty value gives the default; an unreadable, non-positive or too large
// value gives the default (or the cap) AND a warning to print, so a mistyped setting is visible rather than silent.
func ListTimeoutFromEnv() (time.Duration, string) {
	raw := strings.TrimSpace(os.Getenv(ListTimeoutEnv))
	if raw == "" {
		return DefaultListTimeout, ""
	}
	d, err := time.ParseDuration(raw)
	switch {
	case err != nil || d <= 0:
		return DefaultListTimeout, fmt.Sprintf("%s=%q is not a positive duration; using %s", ListTimeoutEnv, raw, DefaultListTimeout)
	case d > MaxListTimeout:
		return MaxListTimeout, fmt.Sprintf("%s=%s is above the cap; using %s (an MCP client gives up on initialize after 30 s)", ListTimeoutEnv, d, MaxListTimeout)
	}
	return d, ""
}

// AuthorCallTimeoutFromEnv reads AuthorCallTimeoutEnv, with ListTimeoutFromEnv's rules: an empty value gives the
// default; an unreadable value, or one outside the bounds, gives the default (or the nearer bound) AND a warning to print.
func AuthorCallTimeoutFromEnv() (time.Duration, string) {
	return callTimeoutFromEnv(AuthorCallTimeoutEnv, DefaultAuthorCallTimeout, MinAuthorCallTimeout, MaxAuthorCallTimeout)
}

// RunnerCallTimeoutFromEnv reads RunnerCallTimeoutEnv, with AuthorCallTimeoutFromEnv's rules.
func RunnerCallTimeoutFromEnv() (time.Duration, string) {
	return callTimeoutFromEnv(RunnerCallTimeoutEnv, DefaultRunnerCallTimeout, MinRunnerCallTimeout, MaxRunnerCallTimeout)
}

func callTimeoutFromEnv(env string, def, lo, hi time.Duration) (time.Duration, string) {
	raw := strings.TrimSpace(os.Getenv(env))
	if raw == "" {
		return def, ""
	}
	d, err := time.ParseDuration(raw)
	switch {
	case err != nil || d <= 0:
		return def, fmt.Sprintf("%s=%q is not a positive duration; using %s", env, raw, def)
	case d < lo:
		return lo, fmt.Sprintf("%s=%s is below the minimum; using %s", env, d, lo)
	case d > hi:
		return hi, fmt.Sprintf("%s=%s is above the cap; using %s", env, d, hi)
	}
	return d, ""
}

// NewServer builds the router's MCP server: no static tools at all — every session gets its own set, built when it starts.
func NewServer(tbl *Table, opts Options) (*mcpserver.Server, error) {
	srv, err := mcpserver.NewServerWithAuth(Authenticator(tbl))
	if err != nil {
		return nil, err
	}
	s := newSessions(tbl, opts)
	srv.SessionTools = s.sessionTools
	srv.UnknownTool = s.unknownTool
	srv.Version = opts.Version
	return srv, nil
}

// sessions builds and serves the per-session sets.
type sessions struct {
	tbl  *Table
	opts Options

	mu    sync.Mutex
	cache map[string]cachedSet
	order []string // insertion order, for the bound
}

// cachedSet is one shared ToolSet and the runner snapshot its runner tools' handlers answer from.
type cachedSet struct {
	tools  *mcpserver.ToolSet
	runner *runnerSnapshot
}

func newSessions(tbl *Table, opts Options) *sessions {
	if opts.Transport == "" {
		opts.Transport = mcp.Streamable
	}
	if opts.ListTimeout <= 0 {
		opts.ListTimeout = DefaultListTimeout
	}
	if opts.CallTimeout <= 0 {
		opts.CallTimeout = DefaultAuthorCallTimeout
	}
	if opts.RunnerCallTimeout <= 0 {
		opts.RunnerCallTimeout = DefaultRunnerCallTimeout
	}
	return &sessions{tbl: tbl, opts: opts, cache: map[string]cachedSet{}}
}

func (s *sessions) logf(format string, args ...any) {
	if s.opts.Logf != nil {
		s.opts.Logf(format, args...)
	}
}

// sessionState is what a session's set remembers about its parts (B4): causes only — no URL, no token.
type sessionState struct {
	authorApplicable bool            // a test folder
	authorCause      AuthorCause     // AuthorUsable unless a state cause stopped the read
	authorListFailed string          // the A6 text when the read itself failed ("" = it did not)
	runnerBreach     bool            // decision 13: the control plane listed a runner_ tool
	runner           *runnerSnapshot // the runner plane as read at session start (runner.go); shared, never written
}

func (st *sessionState) authorUsable() bool {
	return st.authorApplicable && st.authorCause == AuthorUsable && st.authorListFailed == ""
}

// sessionTools is the SessionTools hook.
func (s *sessions) sessionTools(_ string, prin mcpserver.Principal) mcpserver.SessionSet {
	f, err := s.tbl.Resolve(prin.Subject)
	if err != nil {
		// The message authenticated a moment ago, so the folder was removed in between (a teardown).
		return mcpserver.SessionSet{Instructions: err.Error()}
	}
	st := &sessionState{authorApplicable: f.Hat.CanAccessScenarios()}
	ids := sortedIDs(f)

	// The NETWORK reads, in parallel (B4 "fetch in parallel"): the control plane's list for a test folder, and each
	// executor's. Nothing else runs in these goroutines — every decision about what was read is made below, in this
	// goroutine, where the server's recover covers it. A panic in a read is carried back here and raised again, so it
	// ends as the server's "could not build" set instead of taking the router process down.
	var (
		wg         sync.WaitGroup
		pmu        sync.Mutex
		panicked   any
		authorRead authorListRead
		execReads  = make([]executorRead, len(ids))
	)
	goRead := func(read func()) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					pmu.Lock()
					if panicked == nil {
						panicked = r
					}
					pmu.Unlock()
				}
			}()
			read()
		}()
	}
	if st.authorApplicable {
		goRead(func() { authorRead = s.readAuthor(f) })
	}
	for i, id := range ids {
		i, id, up := i, id, f.Upstreams[id]
		goRead(func() { execReads[i] = s.readExecutor(id, up) })
	}
	wg.Wait()
	if panicked != nil {
		panic(panicked)
	}

	var texts []string
	var authorTools []mcpserver.Tool
	authorDigest := ""
	if st.authorApplicable {
		authorTools, authorDigest, texts = s.authorPlane(f, st, authorRead)
	}
	var (
		runnerTools  []mcpserver.Tool
		snap         *runnerSnapshot
		runnerDigest string
		runnerRetry  bool
	)
	if st.runnerBreach {
		// Decision 13: the control plane listed a runner_ tool, so no runner tool at all is shown — neither the control
		// plane's nor the executors'. What the executors listed is not used.
		texts = append(texts, textRunnerBreach)
	} else {
		var runnerTexts []string
		snap, runnerTools, runnerTexts, runnerDigest, runnerRetry = s.runnerPlane(f, ids, execReads)
		texts = append(texts, runnerTexts...)
	}
	ts, shared := s.toolSetFor(st, authorTools, authorDigest, runnerTools, runnerDigest, snap)
	st.runner = shared
	set := mcpserver.SessionSet{Tools: ts, Instructions: strings.Join(texts, "\n"), State: st}
	if (st.authorApplicable && !st.authorUsable()) || runnerRetry {
		set.RetryAfter = retryFailedFirstUse
	}
	return set
}

// authorListRead is the network part of the author plane: the folder's record first (no call), then the list read.
type authorListRead struct {
	stateErr error
	res      mcp.ListResult
}

func (s *sessions) readAuthor(f *Folder) authorListRead {
	tgt, err := f.AuthorTarget()
	if err != nil {
		return authorListRead{stateErr: err}
	}
	return authorListRead{res: listTools(&mcp.Client{ServerURL: ResolveFromVantage(tgt.URL, s.opts.HostAlias),
		Transport: s.opts.Transport, Token: tgt.Token, Timeout: s.opts.ListTimeout})}
}

// authorPlane turns the control plane's list for a test folder into router tools. It records every cause in st and
// returns the session-start texts it produced.
func (s *sessions) authorPlane(f *Folder, st *sessionState, read authorListRead) (tools []mcpserver.Tool, digest string, texts []string) {
	if err := read.stateErr; err != nil {
		if ae := (*AuthorPlaneError)(nil); errors.As(err, &ae) {
			st.authorCause = ae.Cause
		}
		return nil, "", []string{err.Error()}
	}
	res := read.res
	if res.Failure != nil {
		st.authorListFailed = cpFailureText(res.Failure)
		s.logf("router: the control plane's tool list for folder %s could not be read: %s", f.Path, res.Failure.Error())
		return nil, "", []string{st.authorListFailed}
	}

	// Decision 13 FIRST, over every name the control plane listed — before any tool is dropped for its schema, so a
	// breach can never be hidden by the same tool also being malformed.
	var breach []string
	for _, lt := range res.Tools {
		if strings.HasPrefix(lt.Name, "runner_") {
			breach = append(breach, lt.Name)
		}
	}
	if len(breach) > 0 {
		st.runnerBreach = true
		s.logf("router: decision 13 — the control plane listed runner tool(s) %s for folder %s's author token; no runner tool is shown in that session", strings.Join(breach, ", "), f.Path)
	}

	h := sha256.New()
	for _, lt := range res.Tools {
		if strings.HasPrefix(lt.Name, "runner_") {
			continue
		}
		schema, why := decodeSchema(lt.InputSchema)
		if lt.Name == "" {
			why = "it has no name"
		}
		if why != "" {
			texts = append(texts, fmt.Sprintf(textNotPassedOnFmt, quoteName(lt.Name), why))
			s.logf("router: the control plane's tool %s for folder %s is not passed on: %s", quoteName(lt.Name), f.Path, why)
			continue
		}
		fmt.Fprintf(h, "%q\x00%q\x00", lt.Name, lt.Description)
		h.Write(lt.InputSchema)
		h.Write([]byte{0})
		tools = append(tools, s.authorTool(lt, schema))
	}
	return tools, hex.EncodeToString(h.Sum(nil)), texts
}

func quoteName(n string) string {
	if n == "" {
		return "a tool with no name"
	}
	return fmt.Sprintf("%q", n)
}

// decodeSchema decodes a listed input schema the way a strict MCP client checks it, keeping every number exactly as it
// was sent (UseNumber), so the schema listed through the router is the control plane's own. A schema Claude Code would
// reject makes it reject the WHOLE list (internal/control/buildertools.go:78-80 records that) — and through the router
// that list carries the runner tools too — so such a tool is not passed on, and the session is told.
func decodeSchema(raw json.RawMessage) (map[string]any, string) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, "it has no input schema"
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, "its input schema is not JSON"
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, "its input schema is not a JSON object"
	}
	if t, has := m["type"]; has && t != "object" {
		return nil, `its input schema's "type" is not "object"`
	}
	if p, has := m["properties"]; has {
		if _, ok := p.(map[string]any); !ok {
			return nil, `its input schema's "properties" is not an object`
		}
	}
	if r, has := m["required"]; has {
		list, ok := r.([]any)
		if !ok {
			return nil, `its input schema's "required" is not a list`
		}
		for _, e := range list {
			if _, ok := e.(string); !ok {
				return nil, `its input schema's "required" holds something that is not a name`
			}
		}
	}
	return m, ""
}

// authorTool turns one listed control-plane tool into the router's tool: the same name, description and schema, in the
// author namespace (so the server's hat filter keeps it from a product folder), with the old double-underscore name as
// a hidden alias for one release (B8). Its handler captures the listed NAME only.
func (s *sessions) authorTool(lt mcp.ListedTool, schema map[string]any) mcpserver.Tool {
	t := mcpserver.Tool{Name: lt.Name, Namespace: mcpserver.NSAuthor, Description: lt.Description, InputSchema: schema,
		Handler: s.authorHandler(lt.Name)}
	if rest, ok := strings.CutPrefix(lt.Name, "author_"); ok && !strings.HasPrefix(rest, "_") {
		t.Aliases = []string{"author__" + rest}
	}
	return t
}

// toolSetFor returns the shared ToolSet for this session: the runner tools the folder's executors listed, then the author
// tools the control plane listed — or, on a decision-13 breach, the author tools alone. Sets are shared between sessions
// (a thousand sessions of one folder hold one copy of the definitions). That is safe because no tool in them holds a URL
// or a token: each call resolves its folder again. The key covers everything a set can answer from — the author list and
// the runner snapshot (instance ids, causes, versions, what each executor listed) — so two sessions share a set only when
// every answer it can give is the same. The snapshot returned is the one the set's handlers use.
func (s *sessions) toolSetFor(st *sessionState, authorTools []mcpserver.Tool, authorDigest string, runnerTools []mcpserver.Tool, runnerDigest string, snap *runnerSnapshot) (*mcpserver.ToolSet, *runnerSnapshot) {
	key := "author:" + authorDigest + "|runner:" + runnerDigest
	if st.runnerBreach {
		key = "author-only:" + authorDigest
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.cache[key]; ok {
		return c.tools, c.runner
	}
	all := append(append([]mcpserver.Tool{}, runnerTools...), authorTools...)
	ts, skipped := mcpserver.NewToolSet(all...)
	if len(skipped) > 0 {
		// Cannot happen under decision 13 (a runner_ name never comes from the control plane, an author name never from an
		// executor); if it ever does, the first entry — the runner tool — wins, and it is said loudly.
		sort.Strings(skipped)
		s.logf("router: tool names skipped because they collide with an earlier one: %s", strings.Join(skipped, ", "))
	}
	if len(s.order) >= maxCachedToolSets {
		delete(s.cache, s.order[0])
		s.order = s.order[1:]
	}
	s.cache[key] = cachedSet{tools: ts, runner: snap}
	s.order = append(s.order, key)
	return ts, snap
}

// cpFailureText is the A6 text for a list read that failed.
func cpFailureText(f *mcp.ListFailure) string {
	switch {
	case f.Kind == mcp.ListUnreachable || f.Kind == mcp.ListTimedOut:
		return textCPUnreachable
	case f.AuthRefused() && strings.Contains(f.RPCMessage, cpCouldNotVerify):
		return fmt.Sprintf(textCPErrorFmt, fmt.Sprintf("HTTP %d: it could not check the token just now", f.HTTPStatus))
	case f.AuthRefused():
		return textCPRefused
	case f.Kind == mcp.ListHTTPStatus:
		return fmt.Sprintf(textCPErrorFmt, fmt.Sprintf("HTTP %d", f.HTTPStatus))
	case f.Kind == mcp.ListRPCError:
		return fmt.Sprintf(textCPErrorFmt, fmt.Sprintf("JSON-RPC error %d", f.RPCCode))
	}
	return fmt.Sprintf(textCPErrorFmt, "an answer that is not a tool list")
}

// ── the author call path (B4 steps 1-4, 6 and 8 for author_…) ─────────────────────────────────────────────────────

// authorHandler forwards one call of the listed tool `listed` to the control plane.
func (s *sessions) authorHandler(listed string) mcpserver.Handler {
	return func(args json.RawMessage, prin mcpserver.Principal) mcpserver.Outcome {
		// 1. The folder, from the router token, on EVERY call — the address and the token come from it, live.
		f, err := s.tbl.Resolve(prin.Subject)
		if err != nil {
			return mcpserver.ToolErr(map[string]any{"error": err.Error()})
		}
		// 3. The hat — re-derived from the FOLDER, on top of the server's NSAuthor check. If the two ever disagree, the
		// folder wins and nothing is forwarded.
		if f.Hat != role.Test {
			return mcpserver.ToolErr(map[string]any{
				"error": fmt.Sprintf("router: %q is not available to a product-agent folder", listed)})
		}
		// 4. instance_id: the folder rule and the one-instance rule, before any network call.
		if o := checkAuthorArgs(f, args); o != nil {
			return *o
		}
		tgt, err := f.AuthorTarget()
		if err != nil {
			return mcpserver.ToolErr(map[string]any{"error": err.Error()})
		}
		// 6. Forward: the listed name (also when the agent used the old alias), the arguments as sent.
		var callArgs any
		if len(bytes.TrimSpace(args)) > 0 {
			callArgs = json.RawMessage(args)
		}
		res := (&mcp.Client{ServerURL: ResolveFromVantage(tgt.URL, s.opts.HostAlias), Transport: s.opts.Transport,
			Token: tgt.Token, Timeout: s.opts.CallTimeout}).Call(mcp.CallInput{Tool: listed, Args: callArgs})
		// 8. The answer, back unchanged.
		return s.passBack(res, "author", argString(args, "instance_id"), f.Path)
	}
}

// The text an agent gets for a forwarded call that brought back no usable answer (AC-D70.2): the router's own sentence
// with the target and the kind of failure — never the upstream's address and never its error page, which go to the
// router's log only.
const (
	textNoAnswerFmt = "router: the call got no usable answer from %s: %s. The router's log has the details."
	kindUnreachable = "it is not reachable"
	kindTimedOut    = "it did not answer in time"
	kindHTTPFmt     = "it answered with HTTP %d"
	kindBadAnswer   = "its answer could not be read"
	// maxLoggedFailure bounds the client's text in that log line.
	maxLoggedFailure = 1024
)

// noAnswerKind is the kind of failure of a call that brought back no JSON-RPC answer, or "" when an answer came. A
// timeout is checked first: a first contact that timed out is also marked Unreachable.
func noAnswerKind(res mcp.CallResult) string {
	switch {
	case res.TimedOut || res.Failure == mcp.ListTimedOut:
		return kindTimedOut
	case res.Unreachable || res.Failure == mcp.ListUnreachable:
		return kindUnreachable
	case res.Failure == mcp.ListHTTPStatus && res.HTTPStatus != 0:
		return fmt.Sprintf(kindHTTPFmt, res.HTTPStatus)
	case res.Failure != "" || res.TransportErr != "":
		return kindBadAnswer
	}
	return ""
}

// noAnswerTarget names what the call went to, for an agent: an executor by its instance; the control plane, with the
// instance the call named when it named one.
func noAnswerTarget(plane, instance string) string {
	switch {
	case plane == "runner":
		return "the executor of instance " + instance
	case instance == "":
		return "the control plane"
	}
	return "the control plane (instance " + instance + ")"
}

// passBack turns an upstream's answer into the router's (decision 14): a result exactly as sent; a JSON-RPC error with
// the same code, message and data — data only when the upstream sent some, so an error without data does not gain
// "data": null on the way. A call that brought back no JSON-RPC answer — the upstream not reachable, a timeout, an HTTP
// status, an answer that cannot be read — stays a router tool error with reached:false (B4 step 8; plane is "author" for
// the control plane, "runner" for an executor), and since AC-D70.2 its text is the router's own sentence naming the
// target and the kind of failure: never the client's text, which quotes the upstream's URL (Go's error) or the first
// line of its error page. That text goes to the router's log, with the folder (where) and the instance.
func (s *sessions) passBack(res mcp.CallResult, plane, instance, where string) mcpserver.Outcome {
	if kind := noAnswerKind(res); kind != "" {
		detail := res.FailureDetail
		if detail == "" {
			detail = res.TransportErr
		}
		who := "instance " + instance
		if instance == "" {
			who = "no instance named"
		}
		s.logf("router: a call to the %s plane for folder %s (%s) got no usable answer: %s", plane, where, who,
			mcp.CapForLog(detail, maxLoggedFailure))
		return mcpserver.ToolErr(map[string]any{
			"error": fmt.Sprintf(textNoAnswerFmt, noAnswerTarget(plane, instance), kind),
			"plane": plane, "reached": false, "instance": instance})
	}
	switch {
	case res.JSONRPCError != nil:
		e := &mcpserver.RPCError{Code: res.JSONRPCError.Code, Message: res.JSONRPCError.Message}
		if len(res.JSONRPCError.Data) > 0 {
			e.Data = res.JSONRPCError.Data
		}
		return mcpserver.Outcome{RPCErr: e}
	}
	var env struct {
		Result json.RawMessage `json:"result"`
	}
	if json.Unmarshal(res.Raw, &env) != nil || len(env.Result) == 0 || string(env.Result) == "null" {
		upstream := "the control plane"
		if plane == "runner" {
			upstream = "the executor"
		}
		return mcpserver.ToolErr(map[string]any{
			"error": "router: " + upstream + " answered with neither a result nor an error",
			"plane": plane, "reached": true, "instance": instance})
	}
	return mcpserver.Outcome{Result: env.Result}
}

// checkAuthorArgs applies B4 step 4 to an author call. "Absent" means the key is not in the arguments object: the call
// then passes through (decision 5) and the control plane answers as it would on the direct path. "Present" means the
// key, spelled exactly — and because the control plane decodes keys without regard to case (Go's encoding/json), a key
// that only FOLDS to instance_id, or a second instance_id, is refused here: either would carry an instance past the
// folder rule. Arguments that are not a JSON object pass through unchanged.
func checkAuthorArgs(f *Folder, args json.RawMessage) *mcpserver.Outcome {
	refuse := func(msg string) *mcpserver.Outcome {
		o := mcpserver.ToolErr(map[string]any{"error": msg})
		return &o
	}
	idRaw, present, isObject, refusal := exactInstanceKey(args)
	switch {
	case refusal != nil:
		return refusal
	case !isObject || !present:
		return nil
	}
	var id string
	if json.Unmarshal(idRaw, &id) != nil || strings.TrimSpace(id) == "" {
		return refuse("router: instance_id must be a non-empty string naming one instance onboarded into this folder")
	}
	if err := f.checkInstance(id); err != nil {
		return refuse(err.Error())
	}
	return nil
}

// topLevelKeys reads an arguments object's top-level keys in the order sent, duplicates included. isObject is false for
// no arguments, JSON null, or anything that is not an object.
func topLevelKeys(raw json.RawMessage) (keys []string, vals []json.RawMessage, isObject bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, nil, false
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	if _, err := dec.Token(); err != nil {
		return nil, nil, false
	}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, nil, false
		}
		k, _ := tok.(string)
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, nil, false
		}
		keys, vals = append(keys, k), append(vals, v)
	}
	return keys, vals, true
}

// ── names not in the session's set (UnknownTool) ─────────────────────────────────────────────────────────────────

// unknownTool is the UnknownTool hook (B4 step 2).
func (s *sessions) unknownTool(_ string, name string, args json.RawMessage, prin mcpserver.Principal, state any) (*mcpserver.Tool, *mcpserver.Outcome) {
	st, _ := state.(*sessionState)
	switch {
	case strings.HasPrefix(name, "runner_"): // runner_X and runner__X
		if st != nil && st.runnerBreach {
			o := mcpserver.ToolErr(map[string]any{"error": textRunnerBreach})
			return nil, &o
		}
		return s.unknownRunnerTool(name, args, prin, st)
	case strings.HasPrefix(name, "author_"): // author_X and author__X
		f, err := s.tbl.Resolve(prin.Subject)
		if err != nil {
			o := mcpserver.ToolErr(map[string]any{"error": err.Error()})
			return nil, &o
		}
		if !f.Hat.CanAccessScenarios() {
			// A product folder gets the answer it has always got for an author name: the server's own scope refusal,
			// JSON-RPC -32001, counted as "denied" (VR-R7). Returned as an author-namespace tool, so it is the server's
			// scope check — the same code that refused the static author tools before — that refuses it, byte for byte.
			// Its handler runs only if the server's hat and the folder's ever disagree, and then forwards nothing.
			return &mcpserver.Tool{Name: name, Namespace: mcpserver.NSAuthor,
				Handler: func(json.RawMessage, mcpserver.Principal) mcpserver.Outcome {
					return mcpserver.ToolErr(map[string]any{
						"error": fmt.Sprintf("router: %q is not available to a product-agent folder", name)})
				}}, nil
		}
		if st == nil || st.authorUsable() {
			return nil, nil // "unknown tool" — what the control plane itself answers for a name it does not list
		}
		if st.authorListFailed != "" {
			o := mcpserver.ToolErr(map[string]any{"error": st.authorListFailed})
			return nil, &o
		}
		// A state cause at session start: say what is true NOW. The record may have arrived since (onboarding step 8b).
		text := textBecameAvailable
		if _, err := f.AuthorTarget(); err != nil {
			text = err.Error()
		}
		o := mcpserver.ToolErr(map[string]any{"error": text})
		return nil, &o
	}
	return nil, nil
}
