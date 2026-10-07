package main

import (
	"flag"
	"fmt"
	"sort"
	"strings"
)

// unknownflags.go — a flag a command does not own must never vanish.
//
// FOUND 2026-09-24, verifying the T5.4 money-path guard against the real shop-dev kit:
//
//	argus validate-config --config <kit>/argus-config.yaml --scenarios-dir <kit>/scenarios
//	→ {"valid": true, "scenarios_found": 0, ...}
//
// The flag is `--scenarios`. `--scenarios-dir` is not a common flag, so the VR4-B3 partition
// (commonflags.go) routed it into subArgs — and validate-config never reads subArgs. The mistyped
// flag disappeared, the command fell back to its built-in demo path (which does not exist from the
// caller's directory), found zero scenarios, and reported VALID. A green that checked nothing: the
// most dangerous answer a validator can give, because nobody audits good news.
//
// The partition is right (see commonflags.go for why a re-parse is wrong). What was missing is the
// other half: a command that never reads subArgs has nobody downstream to refuse a flag in it, so
// dispatch refuses it. Plain positionals are left alone — only a token shaped like a flag is a flag
// someone meant.

// readsSubArgs reports whether a command consumes the tokens the common parser did not take. Every
// command that owns its argv does; so do the few that are handed subArgs (or `rest`) explicitly in
// dispatch. Everything else would silently drop them.
func readsSubArgs(cmd string) bool {
	if ownsArgv(cmd) {
		return true
	}
	switch cmd {
	case "skills", "keygen", "cloud-onboarding-state", "runner-id":
		return true
	}
	return false
}

// wantsHelp reports whether the leftover tokens ask for help. `-h`/`--help` is no common flag, so it
// too used to be dropped — `argus validate-config --help` ran validate-config. It answers with the
// usage now; refusing it as "unknown" would punish exactly the caller who is trying to find out.
func wantsHelp(cmd string, sub []string) bool {
	if readsSubArgs(cmd) {
		return false
	}
	for _, tok := range sub {
		if tok == "--" {
			return false
		}
		if isHelpToken(tok) {
			return true
		}
	}
	return false
}

// isHelpToken reports whether a single argv token is a help request. Shared by wantsHelp (the
// common-flagset commands) and every self-parsing subcommand dispatcher (router/certificate/
// hub/anchor/runner-id/mcpjson) so "argus <cmd> --help" means the same thing everywhere,
// PRE-AUTH and PRE-TOKEN, never requiring a credential to find out how a command is used.
func isHelpToken(tok string) bool {
	switch tok {
	case "-h", "-help", "--help", "help":
		return true
	}
	return false
}

// isKnownCommand reports whether cmd is one `argus --help` lists (usage_commands.go, kept honest by
// TestUsageListsEveryDispatchedCommand). Used to decide whether an unrecognised `argus <cmd> --help`
// still deserves a command-specific usage line — it must not, or a typo'd command would print as
// though it existed.
func isKnownCommand(cmd string) bool {
	for _, c := range usageCommands {
		if c == cmd {
			return true
		}
	}
	return false
}

// unknownFlagError returns a refusal for the first flag-shaped token in sub that cmd would drop,
// or "" when there is none. Tokens after a bare `--` are the operator's explicit pass-through and are
// never judged.
func unknownFlagError(cmd string, fs *flag.FlagSet, sub []string) string {
	if readsSubArgs(cmd) {
		return ""
	}
	for _, tok := range sub {
		if tok == "--" {
			return ""
		}
		name, _ := flagToken(tok)
		if name == "" {
			continue
		}
		msg := fmt.Sprintf("%s: unknown flag %q — %s does not take it, and it would have been IGNORED, "+
			"not applied: the command would have run with its default instead", cmd, tok, cmd)
		if s := similarFlags(fs, name); len(s) > 0 {
			msg += ". Did you mean " + strings.Join(s, " or ") + "?"
		}
		return msg
	}
	return ""
}

// similarFlags names the declared flags that look like `name`: one is a prefix of the other, or they
// share their first four characters. Enough to turn --scenarios-dir into --scenarios without a
// fuzzy-matching dependency.
func similarFlags(fs *flag.FlagSet, name string) []string {
	var out []string
	fs.VisitAll(func(f *flag.Flag) {
		n := f.Name
		if n == name {
			return
		}
		if strings.HasPrefix(name, n) || strings.HasPrefix(n, name) ||
			(len(n) >= 4 && len(name) >= 4 && n[:4] == name[:4]) {
			out = append(out, "--"+n)
		}
	})
	sort.Strings(out)
	return out
}
