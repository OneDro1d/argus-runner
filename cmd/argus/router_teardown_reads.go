package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/OneDro1d/argus-runner/internal/router"
)

// VR9-H2 / SA §1.4.10 — the two facts teardown must read BEFORE it destroys anything.
//
// ⛔ BOTH READS HAPPEN BEFORE THE UNWIRE, not merely before the de-register. There are TWO deadlines:
//
//	the de-register  kills the credential control-plane side (a revoked token is dead immediately,
//	                 internal/control/store/tokens.go:213), and
//	the UNWIRE       destroys the state the credential lives in — which runs EARLIER STILL.
//
// V27-009 (0.3.29) moved the token onto the machine's RECORD, which unwire no longer touches — the ordering
// above is kept because the de-register still needs the credential, and because the record itself is
// removed (`router unrecord`) only AFTER the control plane answered 200 to the delete.

// cmdRouterAuthorToken prints this machine's author token, bare, so teardown can authorise the delete
// of its own control-plane registration.
//
// ── WHY THIS HAS TO EXIST ─────────────────────────────────────────────────────────────────────────
//
// All three of teardown's delete calls were gated on a SESSION_TOKEN with exactly two sources
// (ARGUS_CP_AUTHOR_TOKEN, formerly ARGUS_CP_TOKEN, and --token), and the ordinary interactive
// teardown sets NEITHER — so the delete
// was structurally unreachable on the normal path. The credential teardown needs was sitting in the
// router state it was about to erase, and nothing could read it out: ReadCloudAuthorToken had one
// non-test caller, and `router status` deliberately REDACTS, emitting `"cloud_plane": true` rather
// than a value.
//
// D5' withdrew the premise that blocked this: `DELETE /api/routers/{id}` goes through authenticateOwner
// (internal/control/web.go:240-259), which filters on author scope and a non-empty subject and NOT on
// token class — so an `odts_` author token (minted during onboarding) satisfies it.
//
// ⛔ THE OUTPUT CONTRACT IS PART OF THE SECURITY ARGUMENT. The token goes to `auth_curl`
// (onboarding/lib/auth-curl.sh:121), which pipes the header through `curl -K -` to keep it off argv
// (SEC-4). So stdout carries the token and NOTHING else — no JSON envelope, no label, no banner — and
// on the failure path stdout carries nothing at all, because a shell doing TOK="$(…)" would otherwise
// send a diagnostic to the control plane as a bearer token.
func cmdRouterAuthorToken(args []string) int {
	fs := flag.NewFlagSet("router author-token", flag.ContinueOnError)
	// Diagnostics go to stderr for the same reason the token goes to stdout alone.
	fs.SetOutput(os.Stderr)
	stateDir := fs.String("state", router.StateDir(), "where the router keeps its folder table")
	cp := fs.String("control-plane", os.Getenv("ARGUS_CP_URL"), "the control plane whose record to read (base URL or its /mcp URL)")
	user := fs.String("user", "", "the account subject the record belongs to (omit on a single-user machine)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *cp == "" {
		fmt.Fprintln(os.Stderr, "router author-token: --control-plane is required (a machine may hold records for several control planes)")
		return exitUsage
	}
	tok, err := router.ReadRecordToken(*stateDir, recordURL(*cp), *user)
	if err != nil {
		fmt.Fprintf(os.Stderr, "router author-token: %v\n", err)
		return exitErr
	}
	if tok == "" {
		// The ORPHAN case, and an ordinary one: a machine whose state is already gone has no token to
		// read. Teardown falls through to the control plane's reap.
		fmt.Fprintln(os.Stderr, "router author-token: no author token is held on this machine")
		return exitErr
	}
	// No newline decoration. ⛔ Never logged, never echoed, never placed on a command line.
	fmt.Fprint(os.Stdout, tok)
	return exitOK
}

// cmdRouterLastInstance answers "is the instance being torn down the last one this machine routes?" —
// exit 0 for yes, non-zero for no.
//
// The predicate, from SA §1.4.10:
//
//	this is the last instance ⟺ NO FOLDER LISTS ANY INSTANCE OTHER THAN THE ONE BEING TORN DOWN.
//
// ⛔ WHY THE EXISTING MACHINERY CANNOT ANSWER IT. teardown's reclaim is gated on RT_DECISION, derived
// from `router folders` AFTER the unwire — so taken literally, VR9-H2's "delete the registration"
// order would fire on EVERY instance teardown, including a machine with three instances tearing down
// one, which would then keep routing with no registration at all.
//
// ⛔ AND `router folders` CANNOT BE MADE TO ANSWER IT. It prints bare paths, one per line
// (router_wire.go:716-722) — no upstream count, no instance list. The only quantity derivable is
// `all_folders − folders_for_this_instance`, and that is wrong: a folder is dropped only when its
// upstream map empties (registry.go:284-286), so a folder routing this instance AND another survives
// the unwire. The subtraction under-counts the remainder and deletes a live machine's registration.
//
// ⚠ MECHANISM NOTE — a stated refinement of SA §1.4.10, not a change of source. The SA says to read
// `router status`, whose per-folder `"instances"` field carries exactly this. That is the right SOURCE
// and it is the one used here — this reads the same state file `router status` renders. What it avoids
// is making the KIT parse a nested JSON array in POSIX shell without jq, which is both fragile and
// untestable without executing teardown.sh end to end. The predicate is decided here, where it has
// tests, and the shell consumes an exit code.
func cmdRouterLastInstance(args []string) int {
	fs := flag.NewFlagSet("router last-instance", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	stateDir := fs.String("state", router.StateDir(), "where the router keeps its folder table")
	instance := fs.String("instance", "", "the instance id being torn down")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *instance == "" {
		fmt.Fprintln(os.Stderr, "usage: argus router last-instance --instance <id> [--state <dir>]")
		return exitUsage
	}
	st, err := router.LoadState(*stateDir)
	if err != nil {
		// ⛔ SAFE WHEN IT CANNOT TELL, and the asymmetry decides which way. A wrong YES deletes the
		// registration of a machine that may still be routing for a live instance; a wrong NO costs a
		// row the control plane's reap clears within a day.
		fmt.Fprintf(os.Stderr, "router last-instance: %v\n", err)
		return exitErr
	}
	for i := range st.Folders {
		for id := range st.Folders[i].Upstreams {
			if id != *instance {
				fmt.Fprintf(os.Stderr, "router last-instance: folder %q still routes %q\n",
					st.Folders[i].Path, id)
				return exitErr
			}
		}
	}
	return exitOK
}
