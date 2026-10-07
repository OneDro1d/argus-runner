package main

import (
	"errors"
	"flag"
	"fmt"

	"github.com/OneDro1d/argus-runner/internal/role"
	"github.com/OneDro1d/argus-runner/internal/skills"
)

// cmdSkills prints the skills onboarding installs for a hat, one per line.
//
// It existed so the list had exactly ONE definition (internal/skills) with two consumers:
// onboarding's installer and teardown's name-scoped purge. ⚠ IT HAD A SECOND CONSUMER — teardown's name-scoped purge (VR8-S1) — and that is gone
// (owner ruling 2026-08-21, the skill-lifecycle topic is deferred). This command currently has NO
// consumer in the kit: onboard.sh still names the skills literally. It is kept because the
// deferred analysis will need it, and internal/skills/drift_test.go keeps the list honest
// meanwhile. Before V26 the list lived only in bash,
// because teardown removed the whole `.claude/skills` directory and needed no list at all — which is
// precisely why it also removed skills the user wrote.
//
// PLAIN LINES, not JSON, for the same reason cmdRouterFolders emits plain lines: the consumer is a
// shell `for` loop, and a shell parsing JSON with sed is how a name containing a space silently
// becomes two names.
func cmdSkills(args []string) int {
	fs := flag.NewFlagSet("skills", flag.ContinueOnError)
	hat := fs.String("hat", "", "which hat's skills to list: product | test")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return emitErr(exitUsage, "flag error: %v", err)
	}
	r, err := role.Parse(*hat)
	if err != nil {
		return emitErr(exitUsage, "skills: --hat must be product or test (got %q)", *hat)
	}
	for _, name := range skills.Installed(r) {
		fmt.Println(name)
	}
	return exitOK
}
