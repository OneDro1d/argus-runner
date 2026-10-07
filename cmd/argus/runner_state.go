package main

import (
	"context"
	"os"
	"time"

	"github.com/OneDro1d/argus-runner/internal/runner"
)

// cmdRunnerState prints whether a run is in flight on this instance — VR9-C1's safety check, exposed
// so the onboarding kit can reach it.
//
// ── THE THREE HOPS ────────────────────────────────────────────────────────────────────────────────
//
//	update.sh --exec--> the executor --mints its own JWT--> GET /fed/state --JSON--> back
//	          (B: transport)          (A: credential)       (the endpoint)
//
// The PO framed (A) and (B) as mutually exclusive channels; they are complementary. (B) is how the kit
// REACHES this process — the same `docker exec` / `kubectl exec` `executor_version()` already uses.
// (A) is how this process authenticates to the control plane: its own Ed25519 machine identity, read
// from the environment it is already running in.
//
// ⛔ NOTHING NEW IS PASSED IN, and that is the security argument. The update command the product
// renders carries no credential, and each obvious way to give it one is forbidden — a `--token VALUE`
// is SEC-4 (argv is world-readable via `ps` and replayed by `docker inspect`), an
// `export ARGUS_CP_AUTHOR_TOKEN=…` (formerly ARGUS_CP_TOKEN) printed beside it is a template the
// operator must repair (VR5-U3), and
// softening the guard is refused because it protects results computed and never reported. Here the key
// never moves: it is already inside this container.
//
// ⚠ IT EXISTS AS A GO SUBCOMMAND BECAUSE update.sh IS POSIX SHELL. Minting an Ed25519 JWT in bash is
// not a thing.
//
// ── THE OUTPUT CONTRACT, AND WHY EVERY AMBIGUITY IS A NON-ZERO EXIT ───────────────────────────────
//
// An empty `running_run_id` means NO RUN IN FLIGHT, and that is the answer that PERMITS an update. So
// a failure must never be able to look like one: on any error this exits non-zero and prints NO
// running_run_id at all, so a kit parsing stdout cannot read a broken check as permission to proceed.
//
// ⛔ AND THE FAILURE CODE IS NEVER exitUsage. SA §0.10 splits three cases, and the split rests on this:
//
//	a JSON answer                  -> the kit USES it (refuse iff a run is in flight)
//	exitUsage / unknown command    -> the image PREDATES this subcommand -> the kit PROCEEDS
//	exec fails, or exitErr here    -> the kit REFUSES
//
// Collapsing the middle case into the last would make the entire existing estate need `--force` to
// reach the very build that fixes V27-001 — the false emergency update.sh's own comment already warns
// about, having been bitten by it once with `argus version`.
func cmdRunnerState(args []string) int {
	cp := os.Getenv("ARGUS_CP_URL")
	if cp == "" {
		return emitErr(exitErr, "runner-state: no control plane is configured for this executor "+
			"(ARGUS_CP_URL is empty), so whether a run is in flight cannot be established")
	}
	// The same defaults `argus run` uses, so this asks as the executor that actually runs here.
	inst := os.Getenv("ARGUS_INSTANCE_ID")
	if inst == "" {
		return emitErr(exitErr, "runner-state: ARGUS_INSTANCE_ID is empty, so there is no instance to ask about")
	}
	keyPath := envOr("ARGUS_IDENTITY_PATH", "results/"+inst+"/identity.key")
	priv, err := runner.LoadKey(keyPath)
	if err != nil {
		return emitErr(exitErr, "runner-state: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	running, err := runner.NewClient(cp, inst, priv).State(ctx)
	if err != nil {
		// ⛔ NO running_run_id ON THIS PATH. The caller must not be able to mistake a failed check for
		// a clear fence.
		return emitErr(exitErr, "runner-state: %v", err)
	}
	// Always present, even when empty: "no run" and "no answer" are different facts and only the first
	// permits an update.
	emit(map[string]any{"running_run_id": running})
	return exitOK
}
