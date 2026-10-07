// Package calm turns a FINOS CALM architecture (https://calm.finos.org, 1.x) into Argus check files.
//
// . The importer is a PURE function of the architecture file, the files its controls point
// at and the operator's flags: it reads, it decides, it returns file bodies. It sends nothing anywhere and
// never guesses — a tool name, an endpoint or a transport it was not told is a refusal or a line in
// UNMAPPED.md, never a default. Nothing in the architecture is silently dropped: every relationship, flow,
// control and node is either named by a generated check (its `## References`, and calm-import.json) or
// listed in UNMAPPED.md with the reason.
package calm

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// The kinds of check the importer writes (calm-import.json `kind`).
const (
	KindConnects         = "connects"
	KindMustNotConnect   = "must-not-connect"
	KindGuardrailRefused = "guardrail-refused"
	KindGuardrailAllowed = "guardrail-allowed"
)

const (
	transportStreamable = "streamable-http"
	transportSSE        = "http-sse"

	// defaultAllowedSymbol is the symbol the "allowed" guardrail check uses when --control-arg
	// mcp-guardrail.allowed is not given. The demo's own scripts book NVDA trades.
	defaultAllowedSymbol = "NVDA"

	// checkTimeout is every generated check's `## TIMEOUT`: how long ONE mcp call or request may wait. An
	// `unreachable` step has its own, shorter bound (5 s) inside the executor.
	checkTimeout = "15s"
)

// Bind says where a node of the architecture lives. A Transport makes the node an MCP server.
type Bind struct {
	URL       string
	Transport string // "" = a plain HTTP service; streamable-http | http-sse = an MCP server
}

// Options is everything Import needs.
type Options struct {
	ArchitecturePath string
	Binds            map[string]Bind   // node unique-id -> where it lives
	StandAs          string            // the node Argus stands as; "" = the only actor
	ControlArgs      map[string]string // "mcp-guardrail.tool" etc.
	// Validate, when set, is run over EVERY generated check; a check it refuses fails the import. The CLI
	// passes toolcore.ValidateAll — the validator author__validate_scenario uses.
	Validate func(string) (*scenario.Scenario, []scenario.Error)
}

// Check is one generated check file and the CALM ids it covers.
type Check struct {
	ID      string
	File    string
	Kind    string
	Covers  []string // calm:<arch>:relationship:<id> | calm:<arch>:control:<node>/<control> | calm:<arch>:node:<id>
	Sources []string // files the architecture pointed at that fed this check (requirement-url, config-url)
}

// Unmapped is one thing in the architecture that did NOT become a check, with why.
type Unmapped struct {
	Ref    string // calm:<arch>:<kind>:<id>
	Kind   string // relationship | flow | control | node | architecture
	Reason string
	Detail string
}

// Result is what Import produced: file name -> body, nothing written yet.
type Result struct {
	Files    map[string][]byte
	Checks   []Check
	Unmapped []Unmapped
}

// ── flags ─────────────────────────────────────────────────────────────────────────────────────────

var tokenLike = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// ParseBind parses `--bind <node-id>=<url>[,<transport>]`.
func ParseBind(s string) (string, Bind, error) {
	node, rest, ok := strings.Cut(s, "=")
	node, rest = strings.TrimSpace(node), strings.TrimSpace(rest)
	if !ok || node == "" || rest == "" {
		return "", Bind{}, fmt.Errorf("--bind %q: want <node-id>=<url>[,streamable-http|http-sse]", s)
	}
	b := Bind{URL: rest}
	if i := strings.LastIndex(rest, ","); i >= 0 {
		if suffix := rest[i+1:]; tokenLike.MatchString(suffix) {
			if suffix != transportStreamable && suffix != transportSSE {
				return "", Bind{}, fmt.Errorf("--bind %q: the transport %q is not one of %s, %s", s, suffix, transportStreamable, transportSSE)
			}
			b = Bind{URL: rest[:i], Transport: suffix}
		}
	}
	if b.URL == "" {
		return "", Bind{}, fmt.Errorf("--bind %q: the url is empty", s)
	}
	return node, b, nil
}

var credentialKey = regexp.MustCompile(`(?i)(token|secret|password|passwd|key|credential|auth)`)

func validateBind(node string, b Bind) error {
	u, err := url.Parse(b.URL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("--bind %q: %q must be an http:// or https:// URL with a host", node, b.URL)
	}
	if u.User != nil {
		return fmt.Errorf("--bind %q: the url carries a credential (user info); a check file must never hold one — the executor's own environment supplies credentials", node)
	}
	for k := range u.Query() {
		if credentialKey.MatchString(k) {
			return fmt.Errorf("--bind %q: the url's query carries a credential-shaped parameter %q; a check file must never hold one", node, k)
		}
	}
	switch b.Transport {
	case "", transportStreamable, transportSSE:
	default:
		return fmt.Errorf("--bind %q: the transport %q is not one of %s, %s", node, b.Transport, transportStreamable, transportSSE)
	}
	return nil
}

var controlArgKeys = map[string]bool{
	"mcp-guardrail.tool":    true,
	"mcp-guardrail.arg":     true,
	"mcp-guardrail.allowed": true,
	"mcp-guardrail.refusal": true,
	// review: an argument template (instead of .arg) and the text a text-refusal carries.
	"mcp-guardrail.args":             true,
	"mcp-guardrail.refused-contains": true,
}

// symbolPlaceholder is the literal the mcp-guardrail.args template carries where the symbol goes.
const symbolPlaceholder = "${symbol}"

// parseArgsTemplate reads an mcp-guardrail.args template: a JSON object that carries ${symbol} at least once.
func parseArgsTemplate(s string) (map[string]any, error) {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("--control-arg mcp-guardrail.args is not valid JSON (%v)", err)
	}
	if dec.More() {
		return nil, fmt.Errorf("--control-arg mcp-guardrail.args has text after the JSON object")
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("--control-arg mcp-guardrail.args must be a JSON object")
	}
	if !strings.Contains(s, symbolPlaceholder) {
		return nil, fmt.Errorf("--control-arg mcp-guardrail.args must contain %s at least once (where the symbol goes)", symbolPlaceholder)
	}
	return obj, nil
}

// substituteSymbol returns a deep copy of v with ${symbol} replaced in every string (keys included). The
// replacement happens on the DECODED strings, so the encoder escapes the symbol: it can never break the JSON.
func substituteSymbol(v any, sym string) any {
	switch x := v.(type) {
	case string:
		return strings.ReplaceAll(x, symbolPlaceholder, sym)
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = substituteSymbol(e, sym)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[strings.ReplaceAll(k, symbolPlaceholder, sym)] = substituteSymbol(e, sym)
		}
		return out
	}
	return v
}

// ── the CALM document (only what the importer reads) ──────────────────────────────────────────────

type calmDoc struct {
	UniqueID      string                     `json:"unique-id"`
	Name          string                     `json:"name"`
	Nodes         []calmNode                 `json:"nodes"`
	Relationships []calmRelationship         `json:"relationships"`
	Flows         []calmFlow                 `json:"flows"`
	Controls      map[string]json.RawMessage `json:"controls"`
}

type calmNode struct {
	UniqueID string                     `json:"unique-id"`
	NodeType string                     `json:"node-type"`
	Name     string                     `json:"name"`
	Controls map[string]json.RawMessage `json:"controls"`
	Details  struct {
		Detailed string `json:"detailed-architecture"`
	} `json:"details"`
}

type calmRelationship struct {
	UniqueID string                     `json:"unique-id"`
	Type     map[string]json.RawMessage `json:"relationship-type"`
	Controls map[string]json.RawMessage `json:"controls"`
}

type calmFlow struct {
	UniqueID    string           `json:"unique-id"`
	Name        string           `json:"name"`
	Transitions []calmTransition `json:"transitions"`
}

type calmTransition struct {
	RelationshipID string `json:"relationship-unique-id"`
	Sequence       int    `json:"sequence-number"`
	Description    string `json:"description"`
}

type calmConnects struct {
	Source      struct{ Node string } `json:"source"`
	Destination struct{ Node string } `json:"destination"`
}

type calmControl struct {
	Description  string `json:"description"`
	Requirements []struct {
		RequirementURL string `json:"requirement-url"`
		ConfigURL      string `json:"config-url"`
	} `json:"requirements"`
}

// ── Import ────────────────────────────────────────────────────────────────────────────────────────

type importer struct {
	o      Options
	doc    calmDoc
	dir    string
	file   string
	nodes  map[string]calmNode
	order  []string // node ids, architecture order
	stand  string
	res    *Result
	seen   map[string]bool // check ids
	hit    map[string]bool // nodes a check exercises
	conn   map[string]bool // nodes the actor connects to directly
	posRel string          // the first connects relationship: the positive step of every negative check
	pos    *chainStep
	posTo  string
}

// Import reads the architecture and the files its controls name, and returns the generated files.
func Import(o Options) (*Result, error) {
	raw, err := os.ReadFile(o.ArchitecturePath)
	if err != nil {
		return nil, fmt.Errorf("read the architecture: %w", err)
	}
	im := &importer{o: o, dir: filepath.Dir(o.ArchitecturePath), file: filepath.Base(o.ArchitecturePath),
		nodes: map[string]calmNode{}, seen: map[string]bool{}, hit: map[string]bool{}, conn: map[string]bool{},
		res: &Result{Files: map[string][]byte{}}}
	if err := json.Unmarshal(raw, &im.doc); err != nil {
		return nil, fmt.Errorf("%s is not a CALM architecture document: %w", im.file, err)
	}
	if strings.TrimSpace(im.doc.UniqueID) == "" {
		return nil, fmt.Errorf("%s has no top-level unique-id: every CALM id in a check's References is namespaced by it", im.file)
	}
	for _, n := range im.doc.Nodes {
		if n.UniqueID == "" {
			return nil, fmt.Errorf("%s has a node with no unique-id", im.file)
		}
		if _, dup := im.nodes[n.UniqueID]; dup {
			return nil, fmt.Errorf("%s declares the node %q twice", im.file, n.UniqueID)
		}
		im.nodes[n.UniqueID] = n
		im.order = append(im.order, n.UniqueID)
	}
	if err := im.checkFlags(); err != nil {
		return nil, err
	}
	if err := im.pickStandAs(); err != nil {
		return nil, err
	}
	if err := im.relationships(); err != nil {
		return nil, err
	}
	if err := im.mustNotConnect(); err != nil {
		return nil, err
	}
	if err := im.nodeControls(); err != nil {
		return nil, err
	}
	im.flowsAndRest()
	im.unexercisedNodes()
	if err := im.emitReports(); err != nil {
		return nil, err
	}
	return im.res, nil
}

func (im *importer) ref(kind, id string) string {
	return "calm:" + im.doc.UniqueID + ":" + kind + ":" + id
}

func (im *importer) checkFlags() error {
	for _, node := range sortedKeys(im.o.Binds) {
		if _, ok := im.nodes[node]; !ok {
			return fmt.Errorf("--bind names %q, which is not a node of %s (its nodes: %s)", node, im.file, strings.Join(im.order, ", "))
		}
		if err := validateBind(node, im.o.Binds[node]); err != nil {
			return err
		}
	}
	for k := range im.o.ControlArgs {
		if !controlArgKeys[k] {
			return fmt.Errorf("--control-arg %q is not a key this importer reads (known: %s)", k, strings.Join(sortedKeys(controlArgKeys), ", "))
		}
	}
	r := im.o.ControlArgs["mcp-guardrail.refusal"]
	if r != "" && r != "tool" && r != "protocol" && r != "text" {
		return fmt.Errorf("--control-arg mcp-guardrail.refusal=%q: want tool (result.isError == true), protocol (a JSON-RPC error) or text (the refusal is words in an ordinary answer)", r)
	}
	rc := im.o.ControlArgs["mcp-guardrail.refused-contains"]
	switch {
	case r == "text" && strings.TrimSpace(rc) == "":
		return fmt.Errorf("--control-arg mcp-guardrail.refusal=text needs --control-arg mcp-guardrail.refused-contains=<the text a refusal carries>")
	case r != "text" && rc != "":
		return fmt.Errorf("--control-arg mcp-guardrail.refused-contains is only read with --control-arg mcp-guardrail.refusal=text")
	case strings.ContainsAny(rc, "\r\n"):
		return fmt.Errorf("--control-arg mcp-guardrail.refused-contains must be one line of text")
	}
	if a, as := im.o.ControlArgs["mcp-guardrail.arg"], im.o.ControlArgs["mcp-guardrail.args"]; a != "" && as != "" {
		return fmt.Errorf("--control-arg mcp-guardrail.arg and --control-arg mcp-guardrail.args are alternatives: pass exactly one, not both")
	} else if as != "" {
		if _, err := parseArgsTemplate(as); err != nil {
			return err
		}
	}
	return nil
}

func (im *importer) pickStandAs() error {
	if s := im.o.StandAs; s != "" {
		if _, ok := im.nodes[s]; !ok {
			return fmt.Errorf("--stand-as names %q, which is not a node of %s (its nodes: %s)", s, im.file, strings.Join(im.order, ", "))
		}
		im.stand = s
		return nil
	}
	var actors []string
	for _, id := range im.order {
		if im.nodes[id].NodeType == "actor" {
			actors = append(actors, id)
		}
	}
	switch len(actors) {
	case 1:
		im.stand = actors[0]
		return nil
	case 0:
		return fmt.Errorf("%s has no actor node: name the node Argus stands as with --stand-as <node-id> (its nodes: %s)", im.file, strings.Join(im.order, ", "))
	}
	return fmt.Errorf("%s has several actor nodes (%s): name the one Argus stands as with --stand-as <node-id>", im.file, strings.Join(actors, ", "))
}

// ── relationships ─────────────────────────────────────────────────────────────────────────────────

func (im *importer) relationships() error {
	for i, rel := range im.doc.Relationships {
		if rel.UniqueID == "" {
			return fmt.Errorf("%s: relationship #%d has no unique-id", im.file, i+1)
		}
		ref := im.ref("relationship", rel.UniqueID)
		var kinds []string
		for k := range rel.Type {
			kinds = append(kinds, k)
		}
		sort.Strings(kinds)
		if len(kinds) != 1 {
			im.unmap(ref, "relationship", fmt.Sprintf("relationship-type names %d types (%s); this importer reads exactly one", len(kinds), strings.Join(kinds, ", ")), "")
			im.relationshipControls(rel)
			continue
		}
		switch kinds[0] {
		case "connects":
			if err := im.connects(rel, ref, rel.Type["connects"]); err != nil {
				return err
			}
		case "deployed-in", "composed-of":
			im.unmap(ref, "relationship", kinds[0]+": a structural relationship — containment is not observable from outside the cluster, so Argus has no probe for it", "")
		case "interacts":
			im.unmap(ref, "relationship", "interacts: an actor's interaction with nodes carries no endpoint Argus can call", "")
		case "options":
			im.unmap(ref, "relationship", "options: a choice between sub-architectures, nothing to probe", "")
		default:
			im.unmap(ref, "relationship", fmt.Sprintf("relationship type %q is not mapped by this importer", kinds[0]), "")
		}
		im.relationshipControls(rel)
	}
	return nil
}

func (im *importer) relationshipControls(rel calmRelationship) {
	for _, name := range sortedKeys(rel.Controls) {
		im.unmap(im.ref("control", rel.UniqueID+"/"+name), "control",
			"a control on a relationship: no mapper (v1 maps only mcp-guardrail, and only on a node)", im.controlDetail(rel.Controls[name]))
	}
}

func (im *importer) connects(rel calmRelationship, ref string, raw json.RawMessage) error {
	var c calmConnects
	if err := json.Unmarshal(raw, &c); err != nil || c.Source.Node == "" || c.Destination.Node == "" {
		im.unmap(ref, "relationship", "connects: source.node / destination.node could not be read", "")
		return nil
	}
	src, dst := c.Source.Node, c.Destination.Node
	if src != im.stand {
		im.unmap(ref, "relationship", fmt.Sprintf("connects %s -> %s: Argus stands as %q and cannot originate a call from %q — it is proved only through a declared entry point; needs a probe", src, dst, im.stand, src), "")
		return nil
	}
	if dst == im.stand {
		im.unmap(ref, "relationship", "connects: the destination is the node Argus stands as", "")
		return nil
	}
	if _, ok := im.nodes[dst]; !ok {
		im.unmap(ref, "relationship", fmt.Sprintf("connects: the destination %q is not a node of the architecture", dst), "")
		return nil
	}
	b, bound := im.o.Binds[dst]
	if !bound {
		return fmt.Errorf("relationship %q connects %q to %q, which has no --bind: pass --bind %s=<url>[,streamable-http|http-sse] (an MCP server needs the transport)", rel.UniqueID, src, dst, dst)
	}
	probe := positiveStep("probe", b)
	title := fmt.Sprintf("CALM connects: %s -> %s (%s)", src, dst, rel.UniqueID)
	id := checkID("CALM-CONNECTS", rel.UniqueID)
	body := renderCheck(checkSpec{
		ID: id, Title: title, Layer: "HTTP Ingestion",
		Steps:    []chainStep{probe},
		Runnable: []string{claimLine(probe.Name, positiveClaim(b))},
		NonRunnable: []string{
			fmt.Sprintf("Argus, standing as %q, can reach %q at the bound address — the relationship %q holds from where the executor runs.", src, dst, rel.UniqueID),
			positiveNote(b),
		},
		Covers: []string{ref, im.ref("node", dst)}, Source: im.file,
	})
	if err := im.addCheck(Check{ID: id, Kind: KindConnects, Covers: []string{ref, im.ref("node", dst)}}, body); err != nil {
		return err
	}
	im.hit[dst], im.conn[dst] = true, true
	if im.pos == nil {
		p := positiveStep("alive", b)
		im.pos, im.posRel, im.posTo = &p, rel.UniqueID, dst
	}
	return nil
}

// ── must-not-connect ──────────────────────────────────────────────────────────────────────────────

func (im *importer) mustNotConnect() error {
	for _, id := range im.order {
		if id == im.stand || im.conn[id] {
			continue
		}
		b, bound := im.o.Binds[id]
		if !bound {
			continue // reported by unexercisedNodes: not bound
		}
		if im.pos == nil {
			continue // reported by unexercisedNodes: no positive step
		}
		target := im.ref("node", id)
		posRef := im.ref("relationship", im.posRel)
		blocked := chainStep{Type: "http", Name: "blocked", Method: "GET", URL: b.URL}
		cid := checkID("CALM-NOCONNECT", im.stand, id)
		body := renderCheck(checkSpec{
			ID: cid, Title: fmt.Sprintf("CALM must not connect: %s -> %s", im.stand, id), Layer: "Permissions",
			Steps: []chainStep{*im.pos, blocked},
			Runnable: []string{
				claimLine("alive", positiveClaim(im.o.Binds[im.posTo])),
				claimLine("blocked", "unreachable"),
			},
			NonRunnable: []string{
				fmt.Sprintf("The architecture declares no relationship from %q to %q, so a request to %q's bound address must get NO HTTP response (connect timeout, refused, reset or no route). Any HTTP status, even 403, means the network let it through.", im.stand, id, id),
				fmt.Sprintf("The first step (%q, over the relationship %q) proves the system is up; if it does not pass, the unreachable step is NOT MEASURED, never green.", im.posTo, im.posRel),
			},
			Covers: []string{posRef, target}, Source: im.file,
		})
		if err := im.addCheck(Check{ID: cid, Kind: KindMustNotConnect, Covers: []string{posRef, target}}, body); err != nil {
			return err
		}
		im.hit[id] = true
	}
	return nil
}

// ── controls ──────────────────────────────────────────────────────────────────────────────────────

func (im *importer) controlDetail(raw json.RawMessage) string {
	var c calmControl
	if json.Unmarshal(raw, &c) != nil {
		return ""
	}
	var parts []string
	for _, r := range c.Requirements {
		if r.RequirementURL != "" {
			parts = append(parts, "requirement-url: "+r.RequirementURL)
		}
		if r.ConfigURL != "" {
			parts = append(parts, "config-url: "+r.ConfigURL)
		}
	}
	return strings.Join(parts, "; ")
}

func (im *importer) nodeControls() error {
	for _, id := range im.order {
		n := im.nodes[id]
		for _, name := range sortedKeys(n.Controls) {
			ref := im.ref("control", id+"/"+name)
			if name != "mcp-guardrail" {
				im.unmap(ref, "control", fmt.Sprintf("no mapper for the control %q (v1 maps only mcp-guardrail)", name), im.controlDetail(n.Controls[name]))
				continue
			}
			if err := im.guardrail(id, name, ref, n.Controls[name]); err != nil {
				return err
			}
		}
	}
	return nil
}

func (im *importer) guardrail(node, name, ref string, raw json.RawMessage) error {
	b, bound := im.o.Binds[node]
	if !bound {
		return fmt.Errorf("node %q carries the control %q but has no --bind: pass --bind %s=<url>,streamable-http|http-sse (the guardrail is checked by calling its MCP tool)", node, name, node)
	}
	detail := im.controlDetail(raw)
	if b.Transport == "" {
		im.unmap(ref, "control", fmt.Sprintf("the node %q is bound without a transport, so it is not an MCP binding and an mcp-guardrail cannot be called (add ,streamable-http or ,http-sse to its --bind)", node), detail)
		return nil
	}
	tool, arg, argsTmpl := im.o.ControlArgs["mcp-guardrail.tool"], im.o.ControlArgs["mcp-guardrail.arg"], im.o.ControlArgs["mcp-guardrail.args"]
	var missing []string
	if tool == "" {
		missing = append(missing, "--control-arg mcp-guardrail.tool=<tool name>")
	}
	if arg == "" && argsTmpl == "" {
		missing = append(missing, "--control-arg mcp-guardrail.arg=<argument key> (or --control-arg mcp-guardrail.args=<JSON object template containing ${symbol}>)")
	}
	if len(missing) > 0 {
		im.unmap(ref, "control", "needs "+strings.Join(missing, " and ")+" (the importer never guesses a tool); add the missing --control-arg flag(s) and import again", detail)
		return nil
	}
	var c calmControl
	if err := json.Unmarshal(raw, &c); err != nil {
		im.unmap(ref, "control", "the control could not be read: "+err.Error(), detail)
		return nil
	}
	var denied []string
	var sources []string
	problems := []string{}
	for _, r := range c.Requirements {
		if r.RequirementURL != "" {
			sources = append(sources, "requirement-url: "+r.RequirementURL)
		}
		if r.ConfigURL == "" {
			continue
		}
		sources = append(sources, "config-url: "+r.ConfigURL)
		syms, why := im.readDenied(r.ConfigURL)
		if why != "" {
			problems = append(problems, why)
			continue
		}
		denied = appendUnique(denied, syms...)
	}
	if len(problems) > 0 || len(denied) == 0 {
		if len(problems) == 0 {
			problems = append(problems, "no requirement carries a config-url, so there is no denied-symbols list to read")
		}
		im.unmap(ref, "control", strings.Join(problems, "; "), detail)
		return nil
	}
	allowed := im.o.ControlArgs["mcp-guardrail.allowed"]
	if allowed == "" {
		allowed = defaultAllowedSymbol
	}
	for _, d := range denied {
		if strings.EqualFold(d, allowed) {
			return fmt.Errorf("the allowed symbol %q is in the denied list of %q on %q: pass --control-arg mcp-guardrail.allowed=<a symbol the guardrail lets through>", allowed, name, node)
		}
	}
	refusalClaim := "result.isError == true"
	refusalWord := "a tool error (result.isError == true)"
	// allowedClaims are asserted on EVERY allowed step (the first step of each denied check and the allowed check).
	allowedClaims := []string{claimLine("allowed", "result.isError == false")}
	switch im.o.ControlArgs["mcp-guardrail.refusal"] {
	case "protocol":
		refusalClaim, refusalWord = "protocol error", "a JSON-RPC protocol error"
	case "text":
		// The server refuses inside an ordinary answer (isError == false). The claim language has no negative
		// body claim (specs/17-chain-scenarios.md: only `body contains|matching|equals|has`), so the allowed
		// step asserts the allowed symbol IS in its answer — an answer that carries the symbol is not a refusal.
		text := im.o.ControlArgs["mcp-guardrail.refused-contains"]
		refusalClaim = "body contains " + text
		refusalWord = fmt.Sprintf("an ordinary answer (result.isError == false) whose text contains %q", text)
		allowedClaims = append(allowedClaims, claimLine("allowed", "body contains "+allowed))
	}
	var argsObj map[string]any
	if argsTmpl != "" {
		var err error
		if argsObj, err = parseArgsTemplate(argsTmpl); err != nil {
			return err
		}
	}
	call := func(name, sym string) chainStep {
		args := map[string]any{arg: sym}
		if argsObj != nil {
			args = substituteSymbol(argsObj, sym).(map[string]any)
		}
		return chainStep{Type: "mcp", Name: name, Transport: b.Transport, ServerURL: b.URL, Tool: tool, Args: args}
	}
	covers := []string{ref, im.ref("node", node)}
	for _, d := range denied {
		cid := checkID("CALM-GUARD", node, name, "DENY", d)
		body := renderCheck(checkSpec{
			ID: cid, Title: fmt.Sprintf("CALM control %s/%s refuses %s", node, name, d), Layer: "Permissions",
			Steps:    []chainStep{call("allowed", allowed), call("denied", d)},
			Runnable: append(append([]string{}, allowedClaims...), claimLine("denied", refusalClaim)),
			NonRunnable: []string{
				fmt.Sprintf("The guardrail on %q must refuse %q (it is in the control's denied-symbols) with %s.", node, d, refusalWord),
				fmt.Sprintf("The first step calls the same tool with the allowed symbol %q and must succeed: it proves %q is a real tool, so a wrong --control-arg mcp-guardrail.tool cannot read as a refusal.", allowed, tool),
				"The tool name and argument key come from --control-arg; the importer never guesses a tool. If the tool needs more arguments than the one named, add them by hand.",
			},
			Covers: covers, Source: im.file,
		})
		if err := im.addCheck(Check{ID: cid, Kind: KindGuardrailRefused, Covers: covers, Sources: sources}, body); err != nil {
			return err
		}
	}
	cid := checkID("CALM-GUARD", node, name, "ALLOW")
	body := renderCheck(checkSpec{
		ID: cid, Title: fmt.Sprintf("CALM control %s/%s lets %s through", node, name, allowed), Layer: "Permissions",
		Steps:    []chainStep{call("allowed", allowed)},
		Runnable: append([]string{}, allowedClaims...),
		NonRunnable: []string{
			fmt.Sprintf("A symbol that is NOT in the control's denied-symbols (%q) must succeed: a guardrail that refuses everything is not the control the architecture declares.", allowed),
		},
		Covers: covers, Source: im.file,
	})
	if err := im.addCheck(Check{ID: cid, Kind: KindGuardrailAllowed, Covers: covers, Sources: sources}, body); err != nil {
		return err
	}
	im.hit[node] = true
	return nil
}

// readDenied reads the `denied-symbols` array of a control's config file (relative to the architecture).
func (im *importer) readDenied(configURL string) ([]string, string) {
	if strings.Contains(configURL, "://") {
		return nil, fmt.Sprintf("config-url %q is a remote URL; the importer reads only files next to the architecture", configURL)
	}
	p := filepath.Join(im.dir, filepath.FromSlash(configURL))
	st, err := os.Stat(p)
	if err != nil {
		return nil, fmt.Sprintf("config-url %q could not be read (%s)", configURL, pathReason(err))
	}
	if st.Size() > 1<<20 {
		return nil, fmt.Sprintf("config-url %q is larger than 1 MiB", configURL)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Sprintf("config-url %q could not be read (%s)", configURL, pathReason(err))
	}
	var cfg struct {
		Denied json.RawMessage `json:"denied-symbols"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, fmt.Sprintf("config-url %q is not JSON: %v", configURL, err)
	}
	var syms []string
	if len(cfg.Denied) == 0 || json.Unmarshal(cfg.Denied, &syms) != nil || len(syms) == 0 {
		return nil, fmt.Sprintf("config-url %q has no denied-symbols list of strings", configURL)
	}
	for _, s := range syms {
		if strings.TrimSpace(s) == "" {
			return nil, fmt.Sprintf("config-url %q has an empty entry in denied-symbols", configURL)
		}
	}
	return syms, ""
}

// ── everything else ───────────────────────────────────────────────────────────────────────────────

func (im *importer) flowsAndRest() {
	for _, id := range im.order {
		if d := im.nodes[id].Details.Detailed; d != "" {
			im.unmap(im.ref("node", id+"/detailed-architecture"), "node",
				"the node points at a nested architecture; v1 does not follow it (import that file on its own)", "detailed-architecture: "+d)
		}
	}
	for _, f := range im.doc.Flows {
		id := f.UniqueID
		if id == "" {
			id = f.Name
		}
		var ts []string
		for _, t := range f.Transitions {
			line := fmt.Sprintf("%d. %s", t.Sequence, t.RelationshipID)
			if t.Description != "" {
				line += " — " + t.Description
			}
			ts = append(ts, line)
		}
		im.unmap(im.ref("flow", id), "flow", "a flow is a sequence of relationships; v1 turns none into a check (a flow needs a probe that drives the whole path)",
			"transitions: "+strings.Join(ts, "; "))
	}
	for _, name := range sortedKeys(im.doc.Controls) {
		im.unmap(im.ref("control", "architecture/"+name), "control", "an architecture-level control: no mapper (v1 maps only mcp-guardrail, and only on a node)", im.controlDetail(im.doc.Controls[name]))
	}
}

func (im *importer) unexercisedNodes() {
	for _, id := range im.order {
		if id == im.stand {
			im.unmap(im.ref("node", id), "node", "Argus stands as this node: every check starts from it, so it is the origin of the probes, never their target", "")
			continue
		}
		if im.hit[id] {
			continue
		}
		ref := im.ref("node", id)
		if _, bound := im.o.Binds[id]; !bound {
			im.unmap(ref, "node", fmt.Sprintf("not bound (no --bind %s=<url>): no check can reach it, so no must-not-connect check was made for it", id), "")
			continue
		}
		im.unmap(ref, "node", fmt.Sprintf("bound, but no must-not-connect check was made: the architecture gives Argus no positive step (no relationship from %q to a bound node), and an unreachable claim with nothing proving the system is up would be a false green", im.stand), "")
	}
}

func (im *importer) unmap(ref, kind, reason, detail string) {
	im.res.Unmapped = append(im.res.Unmapped, Unmapped{Ref: ref, Kind: kind, Reason: reason, Detail: detail})
}

// ── check files ───────────────────────────────────────────────────────────────────────────────────

type chainStep struct {
	Type      string `json:"type"`
	Name      string `json:"name"`
	Transport string `json:"transport,omitempty"`
	ServerURL string `json:"server_url,omitempty"`
	Tool      string `json:"tool,omitempty"`
	Args      any    `json:"args,omitempty"`
	Method    string `json:"method,omitempty"`
	URL       string `json:"url,omitempty"`
}

// positiveStep proves a bound node answers: an MCP node gets an unknown-tool call (a JSON-RPC protocol
// error is an MCP server speaking MCP — the same probe the shipped DOGFOOD-002 / PROTO-001 use); a plain
// HTTP node gets a GET that must answer 200.
func positiveStep(name string, b Bind) chainStep {
	if b.Transport != "" {
		return chainStep{Type: "mcp", Name: name, Transport: b.Transport, ServerURL: b.URL,
			Tool: "argus_calm_probe_unknown_tool", Args: map[string]any{}}
	}
	return chainStep{Type: "http", Name: name, Method: "GET", URL: b.URL}
}

func positiveClaim(b Bind) string {
	if b.Transport != "" {
		return "protocol error"
	}
	return "status=200"
}

func positiveNote(b Bind) string {
	if b.Transport != "" {
		return "The probe calls a tool that does not exist: an MCP server answers it with a JSON-RPC protocol error (any code), which proves it is there and speaks MCP without invoking anything."
	}
	return "The probe is one GET of the bound URL and must answer 200; bind a URL that does (a health path) if the root does not."
}

func claimLine(step, claim string) string { return "- step " + step + ": " + claim }

type checkSpec struct {
	ID, Title, Layer, Source string
	Steps                    []chainStep
	Runnable, NonRunnable    []string
	Covers                   []string
}

func renderCheck(c checkSpec) string {
	var trig bytes.Buffer
	enc := json.NewEncoder(&trig)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	_ = enc.Encode(map[string]any{"steps": c.Steps})
	var b strings.Builder
	fmt.Fprintf(&b, "# Scenario: %s\n\n## Metadata\n- **ID**: %s\n- **Layer**: %s\n- **Tags**: chain, calm\n\n", c.Title, c.ID, c.Layer)
	fmt.Fprintf(&b, "## TRIGGER\nPOST `chain`\n\n```json\n%s```\n\n", trig.String())
	b.WriteString("## EXPECT\n\n### Runnable\n")
	for _, l := range c.Runnable {
		b.WriteString(l + "\n")
	}
	b.WriteString("\n### Non-runnable\n")
	for _, l := range c.NonRunnable {
		b.WriteString("- " + l + "\n")
	}
	fmt.Fprintf(&b, "\n## TIMEOUT\n%s\n\n", checkTimeout)
	b.WriteString("## CLEANUP\nN/A — read-only: this check only reads (one probe or call per step); it creates nothing.\n\n")
	b.WriteString("## References\n")
	for _, r := range c.Covers {
		b.WriteString("- " + r + "\n")
	}
	fmt.Fprintf(&b, "- Generated by `argus calm import` from %s (design). Edit freely; re-importing never overwrites an existing file.\n", c.Source)
	return b.String()
}

var illegalID = regexp.MustCompile(`[^A-Za-z0-9_-]`)

// checkID joins parts into a scenario id (^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$). A part that had to be
// changed, or an id that is too long, gets a hash of the original so two different CALM ids can never
// collapse into one file.
func checkID(prefix string, parts ...string) string {
	orig := prefix + "-" + strings.Join(parts, "-")
	id := illegalID.ReplaceAllString(orig, "-")
	if id == orig && len(id) <= 64 {
		return id
	}
	sum := sha256.Sum256([]byte(orig))
	h := hex.EncodeToString(sum[:])[:8]
	if len(id) > 55 {
		id = id[:55]
	}
	return id + "-" + h
}

func (im *importer) addCheck(c Check, body string) error {
	if im.seen[c.ID] {
		return fmt.Errorf("two checks would be written as %s", c.ID)
	}
	im.seen[c.ID] = true
	c.File = c.ID + ".md"
	if im.o.Validate != nil {
		if _, errs := im.o.Validate(body); len(errs) > 0 {
			var msgs []string
			for _, e := range errs {
				msgs = append(msgs, e.Message)
			}
			return fmt.Errorf("the check %s does not pass the scenario validator: %s", c.ID, strings.Join(msgs, "; "))
		}
	}
	im.res.Files[c.File] = []byte(body)
	im.res.Checks = append(im.res.Checks, c)
	return nil
}

// ── the two reports ───────────────────────────────────────────────────────────────────────────────

func (im *importer) emitReports() error {
	type bindOut struct {
		Node      string `json:"node"`
		URL       string `json:"url"`
		Transport string `json:"transport,omitempty"`
	}
	type checkOut struct {
		ID      string   `json:"id"`
		File    string   `json:"file"`
		Kind    string   `json:"kind"`
		Covers  []string `json:"covers"`
		Sources []string `json:"sources,omitempty"`
	}
	type unmappedOut struct {
		Ref    string `json:"ref"`
		Kind   string `json:"kind"`
		Reason string `json:"reason"`
		Detail string `json:"detail,omitempty"`
	}
	out := struct {
		Tool         string            `json:"tool"`
		Design       string            `json:"design"`
		Architecture map[string]string `json:"architecture"`
		StandAs      string            `json:"stand-as"`
		Binds        []bindOut         `json:"binds"`
		ControlArgs  map[string]string `json:"control-args"`
		Checks       []checkOut        `json:"checks"`
		Unmapped     []unmappedOut     `json:"unmapped"`
	}{
		Tool: "argus calm import", Design: "",
		Architecture: map[string]string{"unique-id": im.doc.UniqueID, "file": im.file},
		StandAs:      im.stand, ControlArgs: map[string]string{},
		Binds: []bindOut{}, Checks: []checkOut{}, Unmapped: []unmappedOut{},
	}
	for _, n := range sortedKeys(im.o.Binds) {
		out.Binds = append(out.Binds, bindOut{n, im.o.Binds[n].URL, im.o.Binds[n].Transport})
	}
	for k, v := range im.o.ControlArgs {
		out.ControlArgs[k] = v
	}
	for _, c := range im.res.Checks {
		out.Checks = append(out.Checks, checkOut{c.ID, c.File, c.Kind, c.Covers, c.Sources})
	}
	for _, u := range im.res.Unmapped {
		out.Unmapped = append(out.Unmapped, unmappedOut{u.Ref, u.Kind, u.Reason, u.Detail})
	}
	var jb bytes.Buffer
	enc := json.NewEncoder(&jb)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		return err
	}
	im.res.Files["calm-import.json"] = jb.Bytes()
	im.res.Files["UNMAPPED.md"] = []byte(im.unmappedMarkdown())
	return nil
}

func (im *importer) unmappedMarkdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# UNMAPPED — what `argus calm import` did NOT turn into a check\n\n")
	fmt.Fprintf(&b, "Architecture: `%s` (%s). Argus stands as `%s`.\n\n", im.doc.UniqueID, im.file, im.stand)
	b.WriteString("Every relationship, flow, control and node of the architecture is either named in a generated check's\n`## References` (see `calm-import.json`) or listed here with the reason. Nothing is dropped silently.\n\n")
	sections := []struct{ kind, title string }{
		{"relationship", "Relationships"}, {"flow", "Flows"}, {"control", "Controls"}, {"node", "Nodes with no check"},
	}
	for _, s := range sections {
		fmt.Fprintf(&b, "## %s\n\n", s.title)
		n := 0
		for _, u := range im.res.Unmapped {
			if u.Kind != s.kind {
				continue
			}
			n++
			fmt.Fprintf(&b, "- `%s` — %s", u.Ref, u.Reason)
			if u.Detail != "" {
				fmt.Fprintf(&b, " (%s)", u.Detail)
			}
			b.WriteString("\n")
		}
		if n == 0 {
			b.WriteString("- none\n")
		}
		b.WriteString("\n")
	}
	return b.String()
}

// Write writes every file under dir. It refuses to overwrite: an author may have edited a generated check
// (CALM-3 adds a `## LOAD` by hand), and re-importing must never destroy that. Nothing is written unless
// every file can be.
func (r *Result) Write(dir string) error {
	names := sortedKeys(r.Files)
	for _, n := range names {
		if _, err := os.Stat(filepath.Join(dir, n)); err == nil {
			return fmt.Errorf("%s already exists in %s: the importer never overwrites a file (remove it, or import into an empty directory)", n, dir)
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, n := range names {
		f, err := os.OpenFile(filepath.Join(dir, n), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return err
		}
		if _, err := f.Write(r.Files[n]); err != nil {
			f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
	}
	return nil
}

// ── small helpers ─────────────────────────────────────────────────────────────────────────────────

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// pathReason is an I/O error without the path (the path is already in the message).
func pathReason(err error) string {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	return err.Error()
}

func appendUnique(dst []string, add ...string) []string {
	for _, a := range add {
		dup := false
		for _, d := range dst {
			if d == a {
				dup = true
			}
		}
		if !dup {
			dst = append(dst, a)
		}
	}
	return dst
}
