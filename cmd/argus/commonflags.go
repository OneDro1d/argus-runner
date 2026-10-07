package main

import (
	"flag"
	"strings"
)

// commonflags.go — VR4-B3: the common parser takes only what it owns.  (V18-003)
//
// ── WHY A PARTITION AND NOT A RE-PARSE ────────────────────────────────────────────────────────────
//
// `dispatch` used to hand the WHOLE argument vector to the COMMON flagset, which errors on any flag it
// has not heard of. A subcommand's own flag was therefore rejected before the subcommand ever ran:
// `cloud-onboarding-state` defines `--state` and could never be given it, so `onboarding_state` was
// never stamped (V18-002) and V17's half-finished-onboard marker could never fire. One parser defect,
// three findings.
//
// The obvious fixes are both wrong, and the reason is measurable. Go's `flag.Parse` STOPS at the first
// non-flag argument:
//
//	router serve --state /state        -> err=nil, leftover=[serve --state /state]   ESCAPES
//	cloud-onboarding-state --state x   -> flag provided but not defined: -state      DIES
//
// `router serve` works today ONLY because `serve` is a positional that halts the parse and forwards
// everything after it untouched. So parsing the whole vector centrally — or binding every subcommand's
// flags onto the common set — satisfies the requirement and breaks the one subcommand that works.
//
// Partitioning instead is the smallest change that cannot do that: the common flagset is handed
// exactly the tokens it declared, and every other token reaches the subcommand in its original order.

// splitCommonFlags divides a subcommand's argument vector into the tokens the COMMON flagset owns and
// everything else. Order is preserved within each half; nothing is dropped.
func splitCommonFlags(fs *flag.FlagSet, args []string) (common, rest []string) {
	for i := 0; i < len(args); i++ {
		tok := args[i]

		// Everything after `--` belongs to the subcommand, even if it names a common flag. This is the
		// operator's explicit escape hatch and it outranks ownership.
		if tok == "--" {
			return common, append(rest, args[i:]...)
		}

		name, carriesValue := flagToken(tok)
		f := fs.Lookup(name)
		if name == "" || f == nil {
			rest = append(rest, tok) // a positional, or a flag this parser does not own
			continue
		}

		common = append(common, tok)
		// A value-taking flag in the `--name value` form owns the NEXT token too. A bool never does —
		// Go only accepts `--flag=false`, so consuming the next token would steal a positional.
		if !carriesValue && !isBoolFlag(f) && i+1 < len(args) {
			i++
			common = append(common, args[i])
		}
	}
	return common, rest
}

// flagToken reports the flag name inside a token and whether the token already carries its value
// (`--name=value`). An empty name means the token is not a flag: a positional, a bare `-`, or `--`.
func flagToken(tok string) (name string, carriesValue bool) {
	if len(tok) < 2 || tok[0] != '-' {
		return "", false
	}
	s := strings.TrimPrefix(tok[1:], "-")
	if s == "" {
		return "", false
	}
	if eq := strings.IndexByte(s, '='); eq >= 0 {
		return s[:eq], true
	}
	return s, false
}

// isBoolFlag asks the flag's Value whether it is boolean, which is how the stdlib itself decides
// whether `-x` consumes the token after it. Asking beats maintaining a second list that can drift.
func isBoolFlag(f *flag.Flag) bool {
	b, ok := f.Value.(interface{ IsBoolFlag() bool })
	return ok && b.IsBoolFlag()
}
