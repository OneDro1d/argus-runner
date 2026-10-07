package main

import (
	"flag"
	"strings"
	"testing"
)

// VR5-D1 (V19-010) — THE REGRESSION THIS PROJECT SHIPPED IN V18.
//
// `router register --control-plane <url> --token <tok>` printed a usage error on 0.3.21 while working
// on 0.3.20. Both flags are declared by the COMMON flagset (main.go:131 and :111), so the V18 argv
// partition routed them to the common parser and handed `cmdRouter` only what was left. The
// subcommand's own --control-plane/--token (router_wire.go:376-377) were never set, and its guard at
// :382 fired. Measured on 7/7 onboards, all three tiers, both flag orders.
//
// WHY THE V18 GUARD DID NOT CATCH IT — the part that matters more than the fix:
//
//   1. The SA named the hazard (C3) and named `router serve` as the victim. The build wrote a guard
//      for exactly that command. It passes, and it is not wrong: `serve` survives because `--state` is
//      NOT a common flag. The guard protected the named EXAMPLE rather than the CLASS.
//   2. It asserted on splitCommonFlags returning `rest` intact — the PARTITION FUNCTION — never that a
//      dispatched subcommand RECEIVES its flag values. A test at the function boundary cannot see a
//      value lost at the dispatch boundary.
//
// So these tests are ENUMERATED over every self-parsing subcommand and over every common flag name,
// not sampled. Sampling is what shipped the defect.

// ownArgvCommands is the enumeration under test. A command belongs here when dispatch hands it the raw
// vector and it never reads commonFlags — i.e. it parses its own arguments.
var ownArgvCommands = []string{"init", "router", "mcpjson", "update", "certificate", "up"}

// Every self-parsing subcommand receives EVERY common flag name intact. This is the class-level
// assertion the V18 guard should have been.
func TestSubcommandArgv_SelfParsingCommandsKeepEveryCommonFlag(t *testing.T) {
	fs := commonSet()

	// Enumerate the common flagset itself, so a flag added later is covered without editing this test.
	var commonNames []string
	fs.VisitAll(func(f *flag.Flag) { commonNames = append(commonNames, f.Name) })
	if len(commonNames) == 0 {
		t.Fatal("the common flagset declares nothing — this test would assert nothing")
	}

	for _, cmd := range ownArgvCommands {
		for _, name := range commonNames {
			t.Run(cmd+"/--"+name, func(t *testing.T) {
				// A representative vector: a positional subcommand, then the flag with a value.
				in := []string{"someverb", "--" + name, "VALUE"}
				_, sub := subcommandArgv(cmd, commonSet(), in)

				if !contains(sub, "--"+name) {
					t.Fatalf("`argus %s someverb --%s VALUE` — the subcommand never receives --%s.\n"+
						"  It was routed to the COMMON parser because the common flagset declares that\n"+
						"  name too. %s parses its own vector and reads none of the common flags, so\n"+
						"  taking one from it can only break it. This is V19-010 exactly.",
						cmd, name, name, cmd)
				}
				if !contains(sub, "VALUE") {
					t.Fatalf("`argus %s someverb --%s VALUE` — the flag arrived without its VALUE.\n"+
						"  A flag delivered without its argument is not delivered.", cmd, name)
				}
				if sub[0] != "someverb" {
					t.Errorf("the subcommand verb was lost or reordered: got %q", sub)
				}
			})
		}
	}
}

// The exact command that shipped broken, in both flag orders, end to end through the partition.
func TestSubcommandArgv_RouterRegisterGetsControlPlaneAndToken(t *testing.T) {
	orders := [][]string{
		{"register", "--control-plane", "https://cp", "--token", "odts_x"},
		{"register", "--token", "odts_x", "--control-plane", "https://cp"},
	}
	for _, in := range orders {
		_, sub := subcommandArgv("router", commonSet(), in)
		joined := strings.Join(sub, " ")
		for _, want := range []string{"--control-plane", "https://cp", "--token", "odts_x", "register"} {
			if !strings.Contains(joined, want) {
				t.Errorf("router register lost %q.\n  got: %q\n  Measured on 0.3.21: this produced\n"+
					"  `usage: argus router register --control-plane <url> --token <tok>` on every\n"+
					"  onboard, while 0.3.20 reached the POST.", want, joined)
			}
		}
	}
}

// `router serve` must keep working — it is what the V18 guard protected, and the fix must not trade
// one victim for another. VR4-B3's original subject.
func TestSubcommandArgv_RouterServeStillIntact(t *testing.T) {
	in := []string{"serve", "--state", "/state", "--port", "9765"}
	_, sub := subcommandArgv("router", commonSet(), in)
	if strings.Join(sub, " ") != strings.Join(in, " ") {
		t.Fatalf("router serve lost arguments: got %q, want %q", sub, in)
	}
}

// A command that DOES read commonFlags must still be partitioned, or VR4-B3 regresses: this is the
// command whose own --state was unusable before V18, and the reason the partition exists at all.
func TestSubcommandArgv_CommonReadingCommandsAreStillPartitioned(t *testing.T) {
	in := []string{"--instance-id", "prod-1", "--state", "complete"}
	common, sub := subcommandArgv("cloud-onboarding-state", commonSet(), in)

	if !contains(common, "--instance-id") {
		t.Error("--instance-id is a COMMON flag and must reach the common parser for a command that reads it")
	}
	if !contains(sub, "--state") || !contains(sub, "complete") {
		t.Errorf("--state is the SUBCOMMAND's own flag and must reach it: got %q.\n"+
			"  This is VR4-B3 (V18-003) — the defect the partition was built to fix.", sub)
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
