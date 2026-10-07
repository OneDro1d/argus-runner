package main

// help_every_command_test.go — F-CLI-HELP-1: `argus <cmd> --help` used to print the GLOBAL usage
// list for every command dispatched through the common flagset (render-k8s, cloud-list-workspaces,
// and every sibling like them), and exit 2 with an extra spurious JSON error line for every
// self-parsing subcommand (update, up, replay, skills, cloud-onboarding-state, preflight), and get
// swallowed as an unrecognised subcommand for every subcommand dispatcher (router, certificate,
// hub, anchor, runner-id, mcpjson) or as a literal positional (init). This iterates EVERY command
// name `argus --help` lists (usageCommands, kept honest against the real dispatch by
// TestUsageListsEveryDispatchedCommand) and drives the real `dispatch()` with no token env set at
// all, so a regression in the token gate ordering fails this test too.

import (
	"strconv"
	"strings"
	"testing"
)

// runHelp drives dispatch(...) with a clean environment (no token vars at all, so a command that
// still demands one before answering --help would fail this) and returns (exit code, combined
// stdout+stderr) — combined because different command shapes answer on different streams (bucket-2
// self-parsing commands print Go's own flag usage to stderr; the common-flagset path in dispatch
// prints to stderr too; a couple print to stdout), and the PROMISE only cares that the answer is
// visible somewhere, not which stream it landed on.
func runHelp(t *testing.T, cmd, flag string) (int, string) {
	t.Helper()
	for _, v := range []string{
		"ARGUS_TOKEN", "ARGUS_RUNNER_TOKEN", "ARGUS_EXECUTOR_SECRET", "ARGUS_AUTHOR_TOKEN",
		"ARGUS_CP_AUTHOR_TOKEN", "ARGUS_CP_TOKEN", "ARGUS_HUB_TOKEN",
	} {
		t.Setenv(v, "")
	}
	var rc int
	var out, errOut string
	errOut = captureStderr(t, func() {
		out = captureStdout(t, func() {
			rc = dispatch([]string{cmd, flag})
		})
	})
	return rc, out + errOut
}

// TestEveryUsageCommand_HelpExitsCleanAndIsCommandSpecific is the PROMISE's own acceptance test:
// every command name `argus --help` lists gets `--help` (and `-h`) answered at exit 0, with no
// token, and the answer names the command itself (never just the global list — a command whose
// output is nothing BUT the full command roster is exactly the F-CLI-HELP-1 defect this guards).
func TestEveryUsageCommand_HelpExitsCleanAndIsCommandSpecific(t *testing.T) {
	if len(usageCommands) < 20 {
		t.Fatalf("usageCommands looks too small (%d) — the fixture list is broken, not the assertion", len(usageCommands))
	}
	for _, cmd := range usageCommands {
		cmd := cmd
		for _, flagTok := range []string{"--help", "-h"} {
			flagTok := flagTok
			t.Run(cmd+"_"+strings.TrimLeft(flagTok, "-"), func(t *testing.T) {
				rc, out := runHelp(t, cmd, flagTok)
				if rc != exitOK {
					t.Fatalf("argus %s %s: exit %d, want %d (%s). output:\n%s", cmd, flagTok, rc, exitOK, exitCodeName(rc), out)
				}
				if strings.TrimSpace(out) == "" {
					t.Fatalf("argus %s %s: printed nothing", cmd, flagTok)
				}
				if !strings.Contains(strings.ToLower(out), cmd) {
					t.Errorf("argus %s %s: output does not name the command itself:\n%s", cmd, flagTok, out)
				}
			})
		}
	}
}

// TestUnknownCommand_HelpFallsBackToGlobalUsage guards the other side of isKnownCommand: a typo'd
// command must not be dressed up with a per-command usage line as though it existed — it still gets
// the global list, at exit 0 (--help never refuses).
func TestUnknownCommand_HelpFallsBackToGlobalUsage(t *testing.T) {
	rc, out := runHelp(t, "definitely-not-a-real-argus-command", "--help")
	if rc != exitOK {
		t.Fatalf("exit %d, want %d", rc, exitOK)
	}
	if !strings.Contains(out, `"tool": "argus"`) {
		t.Errorf("an unknown command's --help should fall back to the global usage JSON, got:\n%s", out)
	}
}

func exitCodeName(rc int) string {
	switch rc {
	case exitOK:
		return "exitOK"
	case exitErr:
		return "exitErr"
	case exitUsage:
		return "exitUsage"
	case exitDenied:
		return "exitDenied"
	case exitFailed:
		return "exitFailed"
	default:
		return "rc=" + strconv.Itoa(rc)
	}
}
