package main

import (
	"flag"
	"os"
	"testing"
)

// ─────────────────────────────────────────────────────────────────────────────────────────────────
// SEC-4 (gate 3) — `router wire` MAY TAKE ITS EXECUTOR CREDENTIAL FROM THE ENVIRONMENT.
//
// onboard.sh passes `--executor-token` into argus_wire, which is a `docker run`. That puts the
// runner token on the HOST's argv — visible to every user via `ps`, and replayed by `docker inspect`
// for the container's lifetime — on every router-path onboard. `--token` has had an env default since
// CP-M3-III-206 for exactly this reason; this one was missed.
//
// ⚠ THE FLAG MUST STILL WIN WHEN GIVEN, and that is the whole compatibility argument. The kit ships
// INDEPENDENTLY of the image: a binary that ignored the flag would break every existing kit, and a
// kit that drops the flag breaks against every existing binary. Flag-wins means this binary is
// compatible with kits old and new, so it can ship on its own — and the KIT side stays gated until
// an image carrying this defaulting is published.
//
// This test pins the precedence in both directions, because getting it backwards is silent: the
// wrong token is simply used, and the failure surfaces later as an authorization error somewhere
// else entirely.
//
// V27-009 redesign (V29-02 §1.4.T1): the AUTHOR token (minted during onboarding) no longer passes
// through `router wire` at all — not as a flag, not as an environment variable. A folder carries a
// CloudRef to the machine's record (`--cloud-url` [+ `--cloud-user`]), and the token lands on that
// record in-process via `cloud-mint-token --router-state`. The `--cloud-token` /
// ARGUS_WIRE_CLOUD_TOKEN half this test used to pin is gone with it; wirecloudurlonly_test.go
// covers what replaced it.
// ─────────────────────────────────────────────────────────────────────────────────────────────────

// wireExecutorToken re-declares the flag exactly as cmdRouterWire does and reports what it resolves
// to. Asserting on the resolution rule rather than on the whole subcommand keeps this a test of the
// precedence and not of folder I/O.
func wireExecutorToken(t *testing.T, args []string) string {
	t.Helper()
	fs := flag.NewFlagSet("wire", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	e := fs.String("executor-token", os.Getenv("ARGUS_WIRE_EXECUTOR_TOKEN"), "")
	if err := fs.Parse(args); err != nil {
		t.Fatalf("parse: %v", err)
	}
	return *e
}

func TestRouterWire_TokensMayComeFromTheEnvironment(t *testing.T) {
	t.Setenv("ARGUS_WIRE_EXECUTOR_TOKEN", "exec-from-env")

	if e := wireExecutorToken(t, nil); e != "exec-from-env" {
		t.Errorf("executor token = %q, want the environment's value.\n"+
			"  Without this the kit has no way to hand it over except argv, where `ps` shows it to\n"+
			"  every user on the machine.", e)
	}
}

// 🚩 THE COMPATIBILITY HALF. An existing kit still passes the flag, and it must keep winning —
// otherwise publishing this binary would silently change which credential every current kit sends.
func TestRouterWire_AnExplicitFlagStillBeatsTheEnvironment(t *testing.T) {
	t.Setenv("ARGUS_WIRE_EXECUTOR_TOKEN", "exec-from-env")

	if e := wireExecutorToken(t, []string{"--executor-token", "exec-from-flag"}); e != "exec-from-flag" {
		t.Fatalf("env overrode an explicit flag: executor=%q.\n"+
			"  The kit ships independently of the image. A binary that ignored the flag would change\n"+
			"  what every EXISTING kit sends, which is a silent behaviour change to the field.", e)
	}
}

// Neither source present is not an error here — the caller decides. Pinned so a future "helpful"
// default cannot appear: inventing a credential is worse than having none.
func TestRouterWire_NoTokenAnywhereIsEmptyNotInvented(t *testing.T) {
	t.Setenv("ARGUS_WIRE_EXECUTOR_TOKEN", "")
	if e := wireExecutorToken(t, nil); e != "" {
		t.Errorf("with no token in flag or environment, got executor=%q, want empty", e)
	}
}
