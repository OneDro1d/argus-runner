package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/OneDro1d/argus-runner/internal/buildinfo"
	"github.com/OneDro1d/argus-runner/internal/onboard"
	"github.com/OneDro1d/argus-runner/internal/sessioninit"
)

// session_init_cmd.go — `argus tester init <app>` and `argus builder init <app>` (msgbus onboarding review
// items 18 and 19). Both are PRE-AUTH and local: they write files and, for the builder, ask the control
// plane one question with the builder's own token. The logic lives in internal/sessioninit.

// The help text names the built-in control plane. It is written as {{CP}} here and filled in from
// buildinfo, so one build-time setting changes the help too.
var testerUsage = strings.ReplaceAll(testerUsageText, "{{CP}}", buildinfo.DefaultControlPlane())

var builderUsage = strings.ReplaceAll(builderUsageText, "{{CP}}", buildinfo.DefaultControlPlane())

const testerUsageText = `usage: argus tester init <app> [--dir <dir>] [--control-plane <url>] [--mcp-json <path>]

  tester init <app>
        Sets up a TESTER session's credential plumbing for <app>, in one step, on a machine whose sessions
        do not source ~/.bashrc (a Coder terminal). It generates exactly the pattern documented in
        docs/runbooks/TESTER-SESSION-PROMPT.md phase 3, and nothing else:
          - ~/.config/argus/tester.env (dir mode 0700, file mode 0600): an EMPTY stanza
            ARGUS_TESTER_TOKEN_<APP>= (and ARGUS_CP_URL if absent). One file for every app on the machine.
            An existing line is never rewritten: a non-empty token is never touched.
          - <dir>/argus-headers-<app>.sh (mode 0700): the headersHelper. It prints the Authorization header
            the MCP client consumes, reading tester.env AT CALL TIME.
          - <dir>/argus-<app> (mode 0700): a wrapper so the CLI sees the token too (sets
            ARGUS_CP_AUTHOR_TOKEN and the older ARGUS_CP_TOKEN, then runs argus). Run it wherever a
            guide says argus.
          - an "argus" entry in .mcp.json (merged; every other byte of the file is kept) naming the
            helper and <control-plane>/mcp. It carries no token.
        It prints the ONE thing you must do: paste your token into ~/.config/argus/tester.env after
        ARGUS_TESTER_TOKEN_<APP>=. It never reads, prints or logs a token value: the file is only asked
        whether a variable is empty. Safe to re-run: unchanged files stay as they are.
        <APP> is <app> upper-cased, anything but a letter or digit turned into _ (msgbus -> MSGBUS).
        Then check each phase with: argus doctor --tester

    --dir <dir>            where the headersHelper and wrapper go (default ~/.config/argus/bin; put it
                           on PATH, or call the wrapper by its full path)
    --control-plane <url>  the control plane (default $ARGUS_CP_URL, else the env file's ARGUS_CP_URL,
                           else {{CP}})
    --mcp-json <path>      the .mcp.json to merge the entry into (default ./.mcp.json, this session's
                           working directory). The entry key is "argus": one .mcp.json names one app.
`

const builderUsageText = `usage: argus builder init <app> [--dir <notepad-dir>] [--control-plane <url>] [--mcp-json <path>]

  builder init <app>
        The same pattern for a BUILDER session (runner scope), in the builder's own notepad directory, with a
        verification step. Everything lives under <dir>/.argus-builder/ (mode 0700), which is added to
        <dir>/.gitignore:
          - builder.env (mode 0600): an EMPTY ARGUS_BUILDER_TOKEN_<APP>= stanza (never overwritten)
          - argus-headers-<app>-builder.sh: the headersHelper, reading builder.env at call time
          - argus-<app>-builder: a CLI wrapper that hands the CLI the token as the RUNNER hat only, unsets
            every variable that could carry an author credential and points the session-file fallback (a
            shared author login) at a file that does not exist
        It never overwrites a token and never prints one.
        Run it once to write the files; it prints the ONE thing you must do: paste your builder token into
        builder.env after ARGUS_BUILDER_TOKEN_<APP>=. Run it again: it then calls tools/list on the control
        plane WITH THAT TOKEN and
          - FAILS loudly (exit 3) unless every tool returned is runner__* (a builder must never see an
            author tool: a token that lists one has author scope and would expose the tester's checks);
          - REFUSES (exit 3) if the control plane answers with an OAuth/sign-in challenge (HTTP 401 +
            WWW-Authenticate) instead of tools: a builder must never go through a sign-in prompt, which
            would issue an author-scope login. Do not open the sign-in link; fix the token.
        .mcp.json is written ONLY after the verification passes (an empty or wrong token would send the MCP
        client into that sign-in prompt), and an "argus" entry left by an earlier run is removed if it fails.

    --dir <notepad-dir>    the builder's notepad directory (default: the current directory)
    --control-plane <url>  the control plane (default $ARGUS_CP_URL, else the env file's ARGUS_CP_URL,
                           else {{CP}})
    --mcp-json <path>      the .mcp.json to wire after verification (default <dir>/.mcp.json)
`

// cmdTester dispatches `tester init`.
func cmdTester(args []string) int {
	if len(args) == 0 {
		fmt.Print(testerUsage)
		return exitUsage
	}
	if isHelpToken(args[0]) {
		fmt.Print(testerUsage)
		return exitOK
	}
	if args[0] != "init" {
		return emitErr(exitUsage, "tester: unknown subcommand %q (init)\n%s", args[0], testerUsage)
	}
	return cmdSessionInit(sessioninit.Tester, args[1:], testerUsage)
}

// cmdBuilder dispatches `builder init`.
func cmdBuilder(args []string) int {
	if len(args) == 0 {
		fmt.Print(builderUsage)
		return exitUsage
	}
	if isHelpToken(args[0]) {
		fmt.Print(builderUsage)
		return exitOK
	}
	if args[0] != "init" {
		return emitErr(exitUsage, "builder: unknown subcommand %q (init)\n%s", args[0], builderUsage)
	}
	return cmdSessionInit(sessioninit.Builder, args[1:], builderUsage)
}

func cmdSessionInit(kind sessioninit.Kind, args []string, usage string) int {
	name := string(kind) + " init"
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dir := fs.String("dir", "", "")
	cpFlag := fs.String("control-plane", "", "")
	mcp := fs.String("mcp-json", "", "")
	// flags may come before or after <app>
	var positional []string
	rest := args
	for {
		if err := fs.Parse(rest); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				fmt.Print(usage)
				return exitOK
			}
			return emitErr(exitUsage, "%s: %v\n%s", name, err, usage)
		}
		if fs.NArg() == 0 {
			break
		}
		positional = append(positional, fs.Arg(0))
		rest = fs.Args()[1:]
	}
	for _, a := range positional {
		if isHelpToken(a) {
			fmt.Print(usage)
			return exitOK
		}
	}
	if len(positional) != 1 {
		return emitErr(exitUsage, "%s: want exactly one <app> (got %d)\n%s", name, len(positional), usage)
	}
	app := positional[0]
	if _, _, err := sessioninit.AppNames(app); err != nil {
		return emitErr(exitUsage, "%s: %v", name, err)
	}
	cp := *cpFlag
	if cp == "" {
		cp = os.Getenv("ARGUS_CP_URL")
	}

	opts := sessioninit.Options{Kind: kind, App: app, ControlPlane: cp}
	switch kind {
	case sessioninit.Tester:
		home, err := os.UserHomeDir()
		if err != nil {
			return emitErr(exitErr, "%s: no home directory: %v", name, err)
		}
		opts.Home = home
		opts.Dir = *dir
		if opts.Dir == "" {
			opts.Dir = filepath.Join(home, ".config", "argus", "bin")
		}
		opts.MCPJSON = *mcp
		if opts.MCPJSON == "" {
			opts.MCPJSON = ".mcp.json"
		}
		opts.MCPJSON = mustAbs(opts.MCPJSON)
		opts.Dir = mustAbs(opts.Dir)
	default:
		opts.Dir = *dir
		if opts.Dir == "" {
			opts.Dir = "."
		}
		opts.Dir = mustAbs(opts.Dir)
	}
	res, err := sessioninit.Init(opts)
	if err != nil {
		return emitErr(exitErr, "%s: %v", name, err)
	}
	fmt.Print(res.Message())

	if kind == sessioninit.Tester {
		if res.TokenSet {
			fmt.Printf("\nThe token for %s is already set in %s: nothing for you to do.\n", res.App, res.EnvFile)
		} else {
			fmt.Printf("\nONE THING FOR YOU TO DO: paste your token into %s after %s=\n"+
				"  (open the file in your editor, not through this chat; the file is mode 0600).\n", res.EnvFile, res.TokenVar)
		}
		fmt.Printf("Then restart the session so the MCP server picks up %s, approve the \"argus\" server, and run:\n"+
			"  argus doctor --tester\nUse %s wherever a guide says `argus`.\n", res.MCPJSON, res.Wrapper)
		return exitOK
	}
	return finishBuilderInit(res, mustAbs(orDefault(*mcp, filepath.Join(opts.Dir, ".mcp.json"))))
}

func orDefault(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

func mustAbs(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}

// finishBuilderInit is the builder's verification: with no token yet, say what to paste; with one, call
// tools/list with it and refuse unless it returns only runner__* tools. .mcp.json is wired only on success.
func finishBuilderInit(res *sessioninit.Result, mcpPath string) int {
	// unwire pulls back an "argus" entry an earlier run wrote, on EVERY path that does not end in a verified
	// token: the usage text promises it, and a stale entry keeps pointing a session at a token that is gone,
	// unreadable or unverified. It says so when it removed one.
	unwire := func() {
		if removed, _ := sessioninit.UnwireMCP(mcpPath); removed {
			fmt.Printf("The \"argus\" entry in %s was removed, so no session keeps pointing at this token.\n", mcpPath)
		}
	}
	if !res.TokenSet {
		// Exit 0, unlike the refusals below: this is the expected first half of a two-step flow (init, paste,
		// init again), not a failed verification. The stale-entry removal still applies.
		unwire()
		fmt.Printf("\nONE THING FOR YOU TO DO: paste your builder token into %s after %s=\n"+
			"  (open the file in your editor, not through this chat; the file is mode 0600).\n"+
			"Then run `argus builder init %s` again: it verifies the token and only then wires %s.\n"+
			"Nothing has been wired into .mcp.json yet, on purpose: an empty token would send the MCP client\n"+
			"into a sign-in prompt, and a builder must never see one.\n", res.EnvFile, res.TokenVar, res.App, mcpPath)
		return exitOK
	}
	token, err := sessioninit.ReadToken(res.EnvFile, res.TokenVar)
	if err != nil || token == "" {
		unwire()
		if err != nil {
			return emitErr(exitErr, "builder init: could not read %s from %s: %v", res.TokenVar, res.EnvFile, err)
		}
		return emitErr(exitErr, "builder init: could not read %s from %s", res.TokenVar, res.EnvFile)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tools, err := onboard.NewCloudClient(res.Control).ToolsList(ctx, token)
	fmt.Printf("\nverification: tools/list on %s/mcp with the token from %s (%s)\n", res.Control, res.EnvFile, res.TokenVar)

	refuse := func(why, fix string) int {
		removed, _ := sessioninit.UnwireMCP(mcpPath)
		fmt.Printf("\nREFUSED. %s\n%s\n", why, fix)
		if removed {
			fmt.Printf("The \"argus\" entry in %s was removed, so no session keeps pointing at this token.\n", mcpPath)
		} else {
			fmt.Printf("%s was NOT written.\n", mcpPath)
		}
		return exitDenied
	}
	var sc *onboard.SignInChallengeError
	switch {
	case errors.As(err, &sc):
		return refuse(fmt.Sprintf("The control plane answered HTTP %d with a sign-in (OAuth) challenge instead of tools. "+
			"A builder must never go through a sign-in prompt: it would issue an author-scope login and expose the tester's checks.", sc.Status),
			"Do NOT open a sign-in link. The token is missing, malformed, expired or revoked: mint a new runner-scope (builder) token, paste it, and run this again.")
	case err != nil:
		// exitErr, not exitDenied: the control plane did not REFUSE this token, the check could not be made
		// (unreachable, 5xx, timeout), which is a failure of the attempt and worth retrying. The entry is
		// still pulled back: an unverified token must not stay wired.
		fmt.Printf("\nFAILED to verify: %s\n", refusalText(err))
		if removed, _ := sessioninit.UnwireMCP(mcpPath); removed {
			fmt.Printf("The \"argus\" entry in %s was removed, so no session keeps pointing at an unverified token.\n", mcpPath)
		} else {
			fmt.Printf("%s was not written.\n", mcpPath)
		}
		return exitErr
	}
	if jerr := sessioninit.JudgeBuilderTools(tools); jerr != nil {
		return refuse("tools/list FAILED the builder check: "+jerr.Error()+".", "Fix the token, then run this again.")
	}
	changed, werr := sessioninit.WireMCP(mcpPath, res.Helper, res.Control)
	if werr != nil {
		return emitErr(exitErr, "builder init: wiring %s: %v", mcpPath, werr)
	}
	fmt.Printf("PASS: tools/list returned %d tools, all runner__* (no author tool visible).\n", len(tools))
	if changed {
		fmt.Printf("wrote  %s (entry %q)\n", mcpPath, sessioninit.MCPKey)
	} else {
		fmt.Printf("%s already has the entry.\n", mcpPath)
	}
	fmt.Printf("Restart the builder session so the MCP server picks it up; use %s wherever a guide says `argus`.\n", res.Wrapper)
	return exitOK
}
