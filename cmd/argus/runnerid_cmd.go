package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/OneDro1d/argus-runner/internal/envname"
	"github.com/OneDro1d/argus-runner/internal/mcp"
)

// cmdRunnerID is `argus runner-id mint | list | revoke` (AC-17): dispatched PRE-AUTH, the same way
// package-check/replay/anchor and every cloud-* verb are — it carries its OWN author session token
// (--token or ARGUS_CP_AUTHOR_TOKEN, formerly ARGUS_CP_TOKEN), never
// ARGUS_RUNNER_TOKEN/ARGUS_EXECUTOR_SECRET (the in-env dark-factory hat gate this verb has nothing to
// do with). It reaches the control plane over the exact MCP surface
// an agent would (author_mint_runner_id / author_list_runner_ids / author_revoke_runner_id) — one
// implementation of the mint/list/revoke rule, a second caller, never a parallel REST endpoint.
const runnerIDUsage = "usage: argus runner-id mint|list|revoke --control-plane <url> --token <author session token> --instance-id <id> [--label <l>] [--runner-id <id>]"

func cmdRunnerID(subArgs []string, cf *commonFlags) int {
	if len(subArgs) == 0 {
		return emitErr(exitUsage, "runner-id: mint | list | revoke required")
	}
	sub := subArgs[0]
	// F-CLI-HELP-1: `sub` doubles as the flagset name AND as the tool selector further down, so
	// `argus runner-id --help` used to be judged against the SAME required-flags check as a real
	// mint/list/revoke call and refused for a missing token before its own subcommand was even
	// checked. Caught above that entirely, PRE-AUTH, no token needed to see the usage.
	if isHelpToken(sub) {
		fmt.Println(runnerIDUsage)
		return exitOK
	}
	fs := flag.NewFlagSet("runner-id "+sub, flag.ContinueOnError)
	instanceID := fs.String("instance-id", "", "the instance this runner id is bound to")
	label := fs.String("label", "", "mint: a display label for the minted runner id")
	runnerID := fs.String("runner-id", "", "revoke: the runner id to revoke")
	cpURL := fs.String("control-plane", os.Getenv("ARGUS_CP_URL"), "the control-plane base URL")
	// ⛔ NO DEFAULT HERE (#44, matching up.go / certificate_cmd.go): a default of
	// os.Getenv("ARGUS_CP_AUTHOR_TOKEN"/"ARGUS_CP_TOKEN") would put the token in --help's rendered
	// usage text. Read AFTER Parse instead, only when the flag itself was left empty.
	token := fs.String("token", "", "an author session token (else ARGUS_CP_AUTHOR_TOKEN, formerly ARGUS_CP_TOKEN)")
	if err := fs.Parse(subArgs[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	rawFlagToken := *token
	if *token == "" {
		*token = envname.Lookup(envname.CPAuthorToken, envname.CPAuthorTokenDeprecated)
	}
	if *cpURL == "" || *token == "" {
		return emitErr(exitUsage, "runner-id %s: --control-plane and --token (author session) are required", sub)
	}
	if *instanceID == "" {
		return emitErr(exitUsage, "runner-id %s: --instance-id is required", sub)
	}
	// F-CRED-1: this always presents an EXPLICIT credential (--token or the env pair) — it never
	// falls back to the session file (the refusal above catches that case) — so the banner only
	// ever names --token or the env var, never "session file".
	credentialBanner(*cpURL, rawFlagToken)

	var tool string
	args := map[string]any{"instance_id": *instanceID}
	switch sub {
	case "mint":
		tool = "author_mint_runner_id"
		if *label != "" {
			args["label"] = *label
		}
	case "list":
		tool = "author_list_runner_ids"
	case "revoke":
		if *runnerID == "" {
			return emitErr(exitUsage, "runner-id revoke: --runner-id is required")
		}
		tool = "author_revoke_runner_id"
		args["runner_id"] = *runnerID
	default:
		return emitErr(exitUsage, "unknown runner-id subcommand %q (want mint | list | revoke)", sub)
	}

	cl := &mcp.Client{ServerURL: strings.TrimRight(*cpURL, "/") + "/mcp", Transport: mcp.Streamable, Token: *token, Timeout: 30 * time.Second}
	return emitMCPToolResult(cl.Call(mcp.CallInput{Tool: tool, Args: args}))
}

// emitMCPToolResult renders an mcp.CallResult the way this CLI renders every other answer: the
// tool's own JSON payload on success, a structured refusal distinguishing transport failure from a
// tool-plane refusal — never a raw Go error string.
func emitMCPToolResult(res mcp.CallResult) int {
	if res.Unreachable {
		return emitErr(exitErr, "runner-id: control plane unreachable")
	}
	if res.TransportErr != "" {
		return emitErr(exitErr, "runner-id: %s", res.TransportErr)
	}
	if res.JSONRPCError != nil {
		return emitErr(exitErr, "runner-id: %s", res.JSONRPCError.Message)
	}
	if len(res.Content) == 0 {
		return emitErr(exitErr, "runner-id: empty response")
	}
	var payload any
	if err := json.Unmarshal([]byte(res.Content[0].Text), &payload); err != nil {
		return emitErr(exitErr, "runner-id: malformed response: %v", err)
	}
	emit(payload)
	if res.IsError {
		return exitErr
	}
	return exitOK
}
