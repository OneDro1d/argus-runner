package main

import (
	"errors"
	"flag"
	"fmt"
	"strings"

	"github.com/OneDro1d/argus-runner/internal/calm"
	"github.com/OneDro1d/argus-runner/internal/toolcore"
)

// calmUsage is `argus calm --help`.
const calmUsage = `calm import <architecture.json> --bind <node-id>=<url>[,<transport>] ... --out <dir> [--stand-as <actor-id>] [--control-arg <key>=<value> ...]

Turns a FINOS CALM architecture (CALM 1.x JSON) into Argus check files, one per file, plus calm-import.json
(each generated check -> the CALM ids it covers) and UNMAPPED.md (every relationship, flow, control and node
that did NOT become a check, with the reason). Nothing is sent anywhere: this reads the architecture and the
control files it points at, and writes under --out. No token needed. It never overwrites a file.

  --bind <node-id>=<url>[,<transport>]   where a node lives; repeat per node. A transport (streamable-http |
                                         http-sse) makes the node an MCP server; none makes it a plain HTTP
                                         service. A url never carries a credential.
  --stand-as <node-id>                   the node Argus stands as (default: the architecture's only actor)
  --control-arg <key>=<value>            mcp-guardrail.tool=<tool name>   (REQUIRED to check an mcp-guardrail;
                                         the importer never guesses a tool)
                                         mcp-guardrail.arg=<argument key> (the symbol's argument) OR
                                         mcp-guardrail.args=<JSON object template> where the literal ${symbol}
                                         is replaced by the symbol; exactly one of the two is REQUIRED
                                         mcp-guardrail.allowed=<symbol>   (a symbol the guardrail lets through;
                                         default NVDA)
                                         mcp-guardrail.refusal=tool|protocol|text (how a refusal shows; default
                                         tool). text needs mcp-guardrail.refused-contains=<text the refusal
                                         carries> (a refusal that is words in an ordinary answer)
  --out <dir>                            where the files go (must not already hold them)

Every generated check is a chain scenario. A "must not connect" check ends in the claim
` + "`- step blocked: unreachable`" + ` and runs only after a positive step passed; it needs an executor that
carries that claim (see specs/17-chain-scenarios.md).`

// multiFlag collects a repeatable flag.
type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

// cmdCalm is `argus calm <verb>`. Only `import` exists. PRE-AUTH and local, like validate-scenario.
func cmdCalm(args []string) int {
	if len(args) == 0 {
		return emitErr(exitUsage, "calm: want a verb — only `import` exists.\n%s", calmUsage)
	}
	if isHelpToken(args[0]) {
		fmt.Println(calmUsage)
		return exitOK
	}
	if args[0] != "import" {
		return emitErr(exitUsage, "calm: unknown verb %q — only `import` exists.\n%s", args[0], calmUsage)
	}
	fs := flag.NewFlagSet("calm import", flag.ContinueOnError)
	var binds, ctlArgs multiFlag
	fs.Var(&binds, "bind", "<node-id>=<url>[,<transport>]")
	fs.Var(&ctlArgs, "control-arg", "<key>=<value>")
	standAs := fs.String("stand-as", "", "the node Argus stands as")
	out := fs.String("out", "", "output directory")
	fs.SetOutput(discardWriter{})
	flagArgs, positional := splitCalmArgs(args[1:])
	if err := fs.Parse(flagArgs); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Println(calmUsage)
			return exitOK
		}
		return emitErr(exitUsage, "calm import: %v", err)
	}
	for _, p := range args[1:] {
		if isHelpToken(p) {
			fmt.Println(calmUsage)
			return exitOK
		}
	}
	if len(positional) != 1 {
		return emitErr(exitUsage, "calm import: want exactly one architecture file (got %d) — calm import <architecture.json> --bind … --out <dir>", len(positional))
	}
	if *out == "" {
		return emitErr(exitUsage, "calm import: --out <dir> is required")
	}
	o := calm.Options{ArchitecturePath: positional[0], StandAs: *standAs,
		Binds: map[string]calm.Bind{}, ControlArgs: map[string]string{}, Validate: toolcore.ValidateAll}
	for _, b := range binds {
		node, bind, err := calm.ParseBind(b)
		if err != nil {
			return emitErr(exitUsage, "calm import: %v", err)
		}
		if _, dup := o.Binds[node]; dup {
			return emitErr(exitUsage, "calm import: --bind names %q twice", node)
		}
		o.Binds[node] = bind
	}
	for _, c := range ctlArgs {
		k, v, ok := strings.Cut(c, "=")
		if !ok || k == "" || v == "" {
			return emitErr(exitUsage, "calm import: --control-arg %q: want <key>=<value>", c)
		}
		o.ControlArgs[k] = v
	}
	res, err := calm.Import(o)
	if err != nil {
		return emitErr(exitErr, "calm import: architecture: %v", err)
	}
	if err := res.Write(*out); err != nil {
		return emitErr(exitErr, "calm import: %v", err)
	}
	var files []string
	for _, c := range res.Checks {
		files = append(files, c.File)
	}
	emit(map[string]any{"ok": true, "out": *out, "checks": len(res.Checks), "unmapped": len(res.Unmapped),
		"files": append(files, "calm-import.json", "UNMAPPED.md")})
	return exitOK
}

// splitCalmArgs separates flags (each followed by its value) from positionals, so the architecture file
// may come before or after the flags. Every flag of `calm import` takes a value.
func splitCalmArgs(args []string) (flagArgs, positional []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") && !strings.Contains(a, "=") && !isHelpToken(a) && i+1 < len(args) {
			flagArgs = append(flagArgs, a, args[i+1])
			i++
			continue
		}
		if strings.HasPrefix(a, "-") {
			flagArgs = append(flagArgs, a)
			continue
		}
		positional = append(positional, a)
	}
	return flagArgs, positional
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
