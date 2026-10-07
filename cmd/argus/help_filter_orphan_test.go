package main

import (
	"flag"
	"io"
	"testing"
)

// TestEveryCommonFlag_IsOwnedBySomeCommand guards commandFlags from the side the table tests cannot
// see: the table is hand-traced, so a flag ADDED to commonFlags.bind and never added here would be
// read by its command and printed by NONE — the filter would hide it from every --help. A flag that
// genuinely belongs to no dispatched command belongs in knownUnlistedFlags, with the reason.
func TestEveryCommonFlag_IsOwnedBySomeCommand(t *testing.T) {
	knownUnlistedFlags := map[string]string{
		"cloud": "bound in commonFlags.bind but read by no code in cmd/argus (checked 2026-09-28) — a dead flag, so hiding it is right",
	}
	fs := flag.NewFlagSet("argus", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var c commonFlags
	c.bind(fs)
	owned := map[string]bool{}
	for _, flags := range commandFlags {
		for _, f := range flags {
			owned[f] = true
		}
	}
	fs.VisitAll(func(f *flag.Flag) {
		if owned[f.Name] {
			return
		}
		if why, ok := knownUnlistedFlags[f.Name]; ok {
			t.Logf("--%s is listed by no command on purpose: %s", f.Name, why)
			return
		}
		t.Errorf("--%s is bound in commonFlags but no command in commandFlags (help_filter.go) lists it, so every --help hides it — add it to the command(s) that read it", f.Name)
	})
}
