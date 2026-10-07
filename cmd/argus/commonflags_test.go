package main

import (
	"flag"
	"reflect"
	"testing"
)

// commonflags_test.go — VR4-B3: a subcommand's OWN flags must be reachable.  (V18-003)
//
// ── THE DEFECT ────────────────────────────────────────────────────────────────────────────────────
//
// `dispatch` parsed the WHOLE argument vector with the COMMON flagset before handing off, so a flag
// that belongs to a subcommand was rejected by a parser that had never heard of it. `cloud-onboarding-
// state` defines `--state` and could never be given it — which is why `instances.onboarding_state` was
// never once stamped (V18-002), which in turn made V17's half-finished-onboard marker unreachable. One
// parser defect, three findings.
//
// ── THE TRAP THIS FILE EXISTS TO GUARD (SA §0 C3) ─────────────────────────────────────────────────
//
// Go's flag.Parse STOPS at the first non-flag argument. Measured:
//
//	router serve --state /state        -> err=nil, leftover=[serve --state /state]   ESCAPES
//	cloud-onboarding-state --state x   -> flag provided but not defined: -state      DIES
//
// So a subcommand is broken if and only if its own flags appear BEFORE any positional. `router serve`
// works today purely by accident of ordering. **A fix that parses the whole vector centrally, or binds
// every subcommand flag onto the common set, satisfies the requirement and BREAKS `router`.** The
// router case below is not a nice-to-have; it is the reason this is a partition and not a re-parse.

func commonSet() *flag.FlagSet {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	(&commonFlags{}).bind(fs)
	return fs
}

func TestSplitCommonFlags(t *testing.T) {
	cases := []struct {
		name       string
		args       []string
		wantCommon []string
		wantRest   []string
	}{
		{
			// V18-003 / V18-002: the finding itself.
			name:       "a subcommand flag leading the vector reaches the subcommand",
			args:       []string{"--state", "complete"},
			wantCommon: nil,
			wantRest:   []string{"--state", "complete"},
		},
		{
			// SA §0 C3 — the regression guard. This works TODAY and must keep working.
			name:       "router serve keeps its arguments intact",
			args:       []string{"serve", "--state", "/state"},
			wantCommon: nil,
			wantRest:   []string{"serve", "--state", "/state"},
		},
		{
			name:       "common flags are taken, subcommand flags are left, order preserved",
			args:       []string{"--instance-id", "x", "--state", "complete"},
			wantCommon: []string{"--instance-id", "x"},
			wantRest:   []string{"--state", "complete"},
		},
		{
			name:       "a subcommand flag BEFORE a common flag still splits correctly",
			args:       []string{"--state", "complete", "--instance-id", "x"},
			wantCommon: []string{"--instance-id", "x"},
			wantRest:   []string{"--state", "complete"},
		},
		{
			// A bool flag takes no value, so it must not swallow the following token.
			name:       "a boolean common flag does not consume the next argument",
			args:       []string{"--summary", "serve"},
			wantCommon: []string{"--summary"},
			wantRest:   []string{"serve"},
		},
		{
			name:       "the equals form is recognised on both sides",
			args:       []string{"--instance-id=x", "--state=complete"},
			wantCommon: []string{"--instance-id=x"},
			wantRest:   []string{"--state=complete"},
		},
		{
			name:       "single-dash form is the same flag",
			args:       []string{"-instance-id", "x", "-state", "y"},
			wantCommon: []string{"-instance-id", "x"},
			wantRest:   []string{"-state", "y"},
		},
		{
			// Everything after `--` belongs to the subcommand, even if it names a common flag.
			name:       "the -- terminator hands the rest over untouched",
			args:       []string{"--instance-id", "x", "--", "--instance-id", "y"},
			wantCommon: []string{"--instance-id", "x"},
			wantRest:   []string{"--", "--instance-id", "y"},
		},
		{
			name:       "positionals and unknown flags interleave without reordering",
			args:       []string{"serve", "--state", "/s", "--instance-id", "x", "extra"},
			wantCommon: []string{"--instance-id", "x"},
			wantRest:   []string{"serve", "--state", "/s", "extra"},
		},
		{
			name:       "an empty vector splits into nothing",
			args:       nil,
			wantCommon: nil,
			wantRest:   nil,
		},
		{
			// A value-taking flag at the very end has no value to take. It must not panic or
			// steal past the end of the slice.
			name:       "a trailing value-taking flag with no value does not overrun",
			args:       []string{"--instance-id"},
			wantCommon: []string{"--instance-id"},
			wantRest:   nil,
		},
		{
			// A bare "-" is a conventional stdin placeholder, not a flag.
			name:       "a bare dash is a positional",
			args:       []string{"-"},
			wantCommon: nil,
			wantRest:   []string{"-"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotCommon, gotRest := splitCommonFlags(commonSet(), c.args)
			if !reflect.DeepEqual(gotCommon, c.wantCommon) {
				t.Errorf("common = %q, want %q", gotCommon, c.wantCommon)
			}
			if !reflect.DeepEqual(gotRest, c.wantRest) {
				t.Errorf("rest = %q, want %q", gotRest, c.wantRest)
			}
		})
	}
}

// The partition is only useful if what it hands the COMMON flagset actually parses. This closes the
// loop: split, then parse, and assert the common flag really landed in the struct.
func TestSplitCommonFlags_ParsesWhatItKeeps(t *testing.T) {
	cf := &commonFlags{}
	fs := flag.NewFlagSet("cloud-onboarding-state", flag.ContinueOnError)
	cf.bind(fs)

	common, rest := splitCommonFlags(fs, []string{"--state", "complete", "--instance-id", "prod-1"})
	if err := fs.Parse(common); err != nil {
		t.Fatalf("the common half must parse cleanly, got %v", err)
	}
	if cf.instance != "prod-1" {
		t.Errorf("instance = %q, want prod-1 — the common flag was not applied", cf.instance)
	}
	if !reflect.DeepEqual(rest, []string{"--state", "complete"}) {
		t.Errorf("rest = %q, want the subcommand's own flag", rest)
	}
}
