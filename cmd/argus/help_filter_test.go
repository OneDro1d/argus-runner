package main

import (
	"strings"
	"testing"
)

// help_filter_test.go — msgbus tester follow-up item 1 (2026-09-28): "argus render-k8s --help
// prints Usage of render-k8s: but then lists EVERY command's flags (-addr for serve, -args for
// mcp-call, -scope for cloud-login, …). Filter to the flags that command reads." Item ID
// F-CLI-HELP-FILTER, help_filter.go.
//
// foreignSampleFlags are flags each owned by EXACTLY ONE command (verified against commandFlags
// below by TestForeignSampleFlags_AreSingleOwner, so the sample cannot silently rot into a false
// positive as commandFlags is edited). For every OTHER command, none of these may appear in its
// --help output.
var foreignSampleFlags = map[string]string{
	"addr":      "serve",
	"scope":     "cloud-login",
	"args":      "mcp-call",
	"revoke":    "cloud-enroll",
	"workspace": "cloud-switch-workspace",
	"from":      "propose-scenario",
	"replicas":  "render-k8s",
	"summary":   "cloud-list-workspaces",
	"path":      "write-scenario",
	"commit":    "mark-deployment",
}

// TestForeignSampleFlags_AreSingleOwner guards the guard: every flag in foreignSampleFlags must
// be owned by EXACTLY the command named, and by no other command in commandFlags — otherwise the
// cross-command assertions below would be checking a flag against a sibling that legitimately
// owns it too, and could never fail even when the filter breaks.
func TestForeignSampleFlags_AreSingleOwner(t *testing.T) {
	for flagName, owner := range foreignSampleFlags {
		for cmd, flags := range commandFlags {
			owns := false
			for _, f := range flags {
				if f == flagName {
					owns = true
					break
				}
			}
			if owns && cmd != owner {
				t.Fatalf("sample flag %q is claimed by both %q and %q — pick a different sample flag or fix commandFlags", flagName, owner, cmd)
			}
			if !owns && cmd == owner {
				t.Fatalf("sample flag %q is not actually in commandFlags[%q] — fix the sample or commandFlags", flagName, owner)
			}
		}
	}
}

// hasFlagToken reports whether out prints a declaration line for -name (bounded so "-token"
// never matches inside "-token-out"/"-token-dir").
func hasFlagToken(out, name string) bool {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimLeft(line, " ")
		if !strings.HasPrefix(line, "-"+name) {
			continue
		}
		rest := line[len("-"+name):]
		if rest == "" || rest[0] == ' ' || rest[0] == '\t' {
			return true
		}
	}
	return false
}

// TestPerCommandHelp_OwnsItsFlagsOnly is the PROMISE's own table test: for every command
// usageCommands lists, `argus <cmd> --help` contains none of foreignSampleFlags' flags EXCEPT
// the one (if any) that cmd itself owns, and DOES contain every flag commandFlags says cmd owns.
// This is exactly the tester's complaint (item 1): a command's --help must list only ITS OWN
// flags, not a sibling's.
func TestPerCommandHelp_OwnsItsFlagsOnly(t *testing.T) {
	if len(usageCommands) < 20 {
		t.Fatalf("usageCommands looks too small (%d)", len(usageCommands))
	}
	for _, cmd := range usageCommands {
		cmd := cmd
		t.Run(cmd, func(t *testing.T) {
			owned, coveredHere := commandFlags[cmd]
			rc, out := runHelp(t, cmd, "--help")
			if rc != exitOK {
				t.Fatalf("argus %s --help: exit %d, want %d\n%s", cmd, rc, exitOK, out)
			}
			// Foreign-flag check applies only to commands this fix actually filters — the
			// self-parsing commands (router, certificate, update, secrets, ...) print their own
			// scoped usage already and are not in commandFlags at all; skip them here (they are
			// covered by help_every_command_test.go's own-name assertion instead).
			if !coveredHere {
				return
			}
			ownSet := make(map[string]bool, len(owned))
			for _, f := range owned {
				ownSet[f] = true
			}
			for flagName, ownerCmd := range foreignSampleFlags {
				if ownerCmd == cmd {
					continue // this command legitimately owns it
				}
				if ownSet[flagName] {
					continue // commandFlags says cmd also legitimately reads it (e.g. shared flags)
				}
				if hasFlagToken(out, flagName) {
					t.Errorf("argus %s --help lists -%s, which belongs only to %s:\n%s", cmd, flagName, ownerCmd, out)
				}
			}
			for _, flagName := range owned {
				if !hasFlagToken(out, flagName) {
					t.Errorf("argus %s --help is missing its own -%s flag:\n%s", cmd, flagName, out)
				}
			}
		})
	}
}
