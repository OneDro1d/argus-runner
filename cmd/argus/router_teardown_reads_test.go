package main

import (
	"os"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/role"
	"github.com/OneDro1d/argus-runner/internal/router"
)

// VR9-H2 / SA §1.4.10 — THE TWO FACTS TEARDOWN MUST READ *BEFORE* IT DESTROYS ANYTHING.
//
// ── WHY THESE ARE SUBCOMMANDS AND NOT SOMETHING THE KIT CAN ALREADY ASK ───────────────────────────
//
//  1. THE TOKEN. The router CLI was serve|wire|unwire|register|status|folders, and `router status`
//     REDACTS: it emits "cloud_plane": true — a boolean, never a value. So teardown had no way to obtain
//     the credential that authorises deleting its own registration, which is why all three delete calls
//     were gated on a SESSION_TOKEN the ordinary interactive teardown never sets. The delete was
//     structurally unreachable on the normal path.
//
//  2. IS THIS THE LAST INSTANCE. The existing reclaim decides with `router folders`, which prints
//     BARE PATHS, one per line — no instance list. The only quantity derivable from it is
//     `all_folders − folders_for_this_instance`, and that is WRONG: a folder is dropped only when its
//     upstream map empties, so a folder routing this instance AND another survives. The subtraction
//     under-counts the remainder and would delete the registration of a machine still routing two
//     other instances.
//
// ── UNDER THE V27-009 REDESIGN ───────────────────────────────────────────────────────────────────
//
// The author token (minted during onboarding) lives on the machine's record for (control plane, user),
// which every unwire KEEPS. `router author-token` therefore names the control plane (a machine may hold
// records for several) and, on a machine with several accounts, the user. What removes the record is
// `router unrecord`, so the read must precede THAT; reading before the unwire keeps it before everything.
func TestRouterAuthorToken(t *testing.T) {
	const cp = "https://cp.example/mcp"
	// The flag's env default must not leak in from the machine running this suite.
	t.Setenv("ARGUS_CP_URL", "")

	withRecords := func(t *testing.T, recs ...router.CloudRecord) string {
		t.Helper()
		dir := t.TempDir()
		st := router.State{Port: 9765}
		for _, r := range recs {
			st.PutRecord(r)
		}
		if err := router.SaveState(dir, st); err != nil {
			t.Fatal(err)
		}
		return dir
	}

	// ⛔ BARE ON STDOUT. It is consumed by `auth_curl`, which pipes the header through `curl -K -` to
	// keep it off argv (SEC-4). Any decoration — a JSON wrapper, a label, a trailing banner — would be
	// sent to the control plane as part of the credential.
	t.Run("it prints the record's token bare, and nothing else", func(t *testing.T) {
		dir := withRecords(t, router.CloudRecord{URL: cp, User: "user_a", Token: "odts_secret"})
		out := captureEmitRaw(t, func() {
			if rc := cmdRouterAuthorToken([]string{"--state", dir, "--control-plane", cp}); rc != 0 {
				t.Fatalf("exit %d, want 0 — teardown cannot delete its own registration without this", rc)
			}
		})
		got := string(out)
		if strings.TrimSpace(got) != "odts_secret" {
			t.Fatalf("stdout = %q, want exactly the token. It is piped into an Authorization header; "+
				"anything else travels to the control plane as part of the credential.", got)
		}
		if strings.Contains(got, "{") || strings.Contains(got, "\"") {
			t.Errorf("stdout carries JSON decoration: %q", got)
		}
	})

	// The kit knows the control plane by its BASE URL; the record is keyed by the /mcp form. Both name it.
	t.Run("the base URL names the same record as its /mcp form", func(t *testing.T) {
		dir := withRecords(t, router.CloudRecord{URL: cp, User: "user_a", Token: "odts_secret"})
		out := captureEmitRaw(t, func() {
			if rc := cmdRouterAuthorToken([]string{"--state", dir, "--control-plane", "https://cp.example"}); rc != 0 {
				t.Fatalf("exit %d, want 0", rc)
			}
		})
		if strings.TrimSpace(string(out)) != "odts_secret" {
			t.Fatalf("stdout = %q, want the record's token", out)
		}
	})

	// A machine may hold records for several control planes, so "the token" is not a question.
	t.Run("--control-plane is required: usage, and nothing on stdout", func(t *testing.T) {
		dir := withRecords(t, router.CloudRecord{URL: cp, User: "user_a", Token: "odts_secret"})
		var rc int
		out := captureEmitRaw(t, func() { rc = cmdRouterAuthorToken([]string{"--state", dir}) })
		if rc != exitUsage {
			t.Errorf("exit %d without --control-plane, want %d (usage)", rc, exitUsage)
		}
		if strings.TrimSpace(string(out)) != "" {
			t.Fatalf("stdout = %q on the usage path — a shell capturing this would send it as a bearer token", out)
		}
	})

	// ⛔ NOTHING ON STDOUT WHEN THERE IS NONE. A caller doing `TOK="$(... author-token)"` must get an
	// empty string, not an error message it would then send as a bearer token.
	t.Run("a record with no token yet: exit 1 and nothing on stdout", func(t *testing.T) {
		dir := withRecords(t, router.CloudRecord{URL: cp, User: "user_a"}) // registered (8a), never minted (8b)
		var rc int
		out := captureEmitRaw(t, func() { rc = cmdRouterAuthorToken([]string{"--state", dir, "--control-plane", cp}) })
		if rc != exitErr {
			t.Errorf("exit %d with no token, want %d — a caller cannot distinguish success from emptiness", rc, exitErr)
		}
		if strings.TrimSpace(string(out)) != "" {
			t.Fatalf("stdout = %q on the no-token path. A shell capturing this would send the "+
				"diagnostic to the control plane as a bearer token.", out)
		}
	})

	// An absent state dir is the ORPHAN case, and it is not an error worth a stack trace: teardown
	// simply has nothing to read and falls through to the reap.
	t.Run("an absent state dir is quiet and non-zero", func(t *testing.T) {
		var rc int
		out := captureEmitRaw(t, func() {
			rc = cmdRouterAuthorToken([]string{"--state", t.TempDir() + "/nope", "--control-plane", cp})
		})
		if rc == 0 {
			t.Error("exit 0 for a state dir that does not exist")
		}
		if strings.TrimSpace(string(out)) != "" {
			t.Errorf("stdout = %q, want nothing", out)
		}
	})

	// The record is the system of record. A machine torn down to zero folders still has it, and that is
	// the case this whole command exists to serve — and a folder's ref carries nothing to read anyway.
	t.Run("a machine torn down to zero folders still holds it", func(t *testing.T) {
		dir := t.TempDir()
		st := router.State{Port: 9765}
		st.PutRecord(router.CloudRecord{URL: cp, User: "user_a", Token: "odts_machine"})
		var err error
		st, _, _, err = router.UpsertFolder(st, router.FolderSpec{Path: t.TempDir(), Hat: role.Test, InstanceID: "orders",
			Executor: router.Upstream{URL: "http://exec", Token: "rt"}, Cloud: &router.CloudRef{URL: cp, User: "user_a"}})
		if err != nil {
			t.Fatal(err)
		}
		st, _, err = router.RemoveInstance(st, st.Folders[0].Path, "orders")
		if err != nil || len(st.Folders) != 0 {
			t.Fatalf("fixture: %v, folders=%d", err, len(st.Folders))
		}
		if err := router.SaveState(dir, st); err != nil {
			t.Fatal(err)
		}
		out := captureEmitRaw(t, func() {
			if rc := cmdRouterAuthorToken([]string{"--state", dir, "--control-plane", cp}); rc != 0 {
				t.Fatalf("exit %d", rc)
			}
		})
		if strings.TrimSpace(string(out)) != "odts_machine" {
			t.Fatalf("stdout = %q, want the record's token", out)
		}
	})

	// Two accounts on one machine: without --user the command cannot know whose token teardown wants,
	// and must print NOTHING rather than guess; with --user it prints that record's token.
	t.Run("two records for one control plane: --user picks, nameless is refused quietly", func(t *testing.T) {
		dir := withRecords(t,
			router.CloudRecord{URL: cp, User: "user_a", Token: "odts_a"},
			router.CloudRecord{URL: cp, User: "user_b", Token: "odts_b"})
		var rc int
		out := captureEmitRaw(t, func() { rc = cmdRouterAuthorToken([]string{"--state", dir, "--control-plane", cp}) })
		if rc == 0 || strings.TrimSpace(string(out)) != "" {
			t.Fatalf("nameless read with two records: exit %d stdout %q; want non-zero and nothing — printing "+
				"either token would authorise the delete with the wrong account's credential", rc, out)
		}
		out = captureEmitRaw(t, func() { rc = cmdRouterAuthorToken([]string{"--state", dir, "--control-plane", cp, "--user", "user_b"}) })
		if rc != 0 || strings.TrimSpace(string(out)) != "odts_b" {
			t.Fatalf("--user user_b: exit %d stdout %q, want 0 and user_b's token", rc, out)
		}
	})

	// VR-P7 still stands on the read path: a PRODUCT folder carrying a ref is corrupt state, refused before
	// anything is read — and refused QUIETLY, for the same reason as every other failure here.
	t.Run("a product folder carrying a ref is refused, with nothing on stdout", func(t *testing.T) {
		dir := t.TempDir()
		st := router.State{Port: 9765, Folders: []router.Folder{
			{Path: "/p", Hat: role.Product, Token: "odtr_p", Cloud: &router.CloudRef{URL: cp, User: "user_a"}},
		}}
		st.PutRecord(router.CloudRecord{URL: cp, User: "user_a", Token: "odts_secret"})
		if err := router.SaveState(dir, st); err != nil {
			t.Fatal(err)
		}
		var rc int
		out := captureEmitRaw(t, func() { rc = cmdRouterAuthorToken([]string{"--state", dir, "--control-plane", cp}) })
		if rc == 0 {
			t.Error("exit 0 on a state a product folder has a cloud ref in")
		}
		if strings.TrimSpace(string(out)) != "" {
			t.Fatalf("stdout = %q on the corrupt-state path, want nothing", out)
		}
	})
}

// VR9-H2 — "IS THIS THE LAST INSTANCE ON THIS MACHINE?"
//
// The predicate, from SA §1.4.10:
//
//	this is the last instance ⟺ NO FOLDER LISTS ANY INSTANCE OTHER THAN THE ONE BEING TORN DOWN.
//
// ⛔ It must be computed BEFORE the unwire. The existing reclaim derives its decision AFTER, from the
// emptied table — so taken literally, VR9-H2's "delete the registration" order would fire on EVERY
// instance teardown, including a machine with three instances tearing down one, which then keeps
// routing with no registration at all.
func TestRouterLastInstance(t *testing.T) {
	folder := func(path string, insts ...string) router.Folder {
		f := router.Folder{Path: path, Hat: role.Test, Upstreams: map[string]router.Upstream{}}
		for _, id := range insts {
			f.Upstreams[id] = router.Upstream{}
		}
		return f
	}
	write := func(t *testing.T, folders ...router.Folder) string {
		t.Helper()
		dir := t.TempDir()
		if err := router.SaveState(dir, router.State{Port: 9765, Folders: folders}); err != nil {
			t.Fatal(err)
		}
		return dir
	}

	t.Run("the only instance: yes", func(t *testing.T) {
		dir := write(t, folder("/a", "orders"), folder("/b", "orders"))
		if rc := cmdRouterLastInstance([]string{"--state", dir, "--instance", "orders"}); rc != 0 {
			t.Fatalf("exit %d, want 0 — nothing else routes here, so the registration should go", rc)
		}
	})

	// ⛔ THE CASE THE `router folders` SUBTRACTION GETS WRONG. One folder routes BOTH instances, so
	// removing this one leaves the folder in place and the machine still routing.
	t.Run("a folder shared with another instance: no", func(t *testing.T) {
		dir := write(t, folder("/shared", "orders", "memstore"))
		if rc := cmdRouterLastInstance([]string{"--state", dir, "--instance", "orders"}); rc == 0 {
			t.Fatal("reported LAST while a folder still routes 'memstore'. Deleting the registration " +
				"here leaves a machine that routes for a live instance with nothing registered — the " +
				"exact failure SA §1.4.10 rules out `router folders` for.")
		}
	})

	t.Run("a separate folder for another instance: no", func(t *testing.T) {
		dir := write(t, folder("/a", "orders"), folder("/b", "memstore"))
		if rc := cmdRouterLastInstance([]string{"--state", dir, "--instance", "orders"}); rc == 0 {
			t.Fatal("reported LAST while /b routes 'memstore'")
		}
	})

	// A machine already emptied — the orphan-ish case — is trivially last.
	t.Run("no folders at all: yes", func(t *testing.T) {
		dir := write(t)
		if rc := cmdRouterLastInstance([]string{"--state", dir, "--instance", "orders"}); rc != 0 {
			t.Fatalf("exit %d, want 0 for an empty table", rc)
		}
	})

	// A folder with no upstreams at all cannot be evidence that another instance exists.
	t.Run("a folder routing nothing does not block: yes", func(t *testing.T) {
		dir := write(t, folder("/wired-not-routed"), folder("/a", "orders"))
		if rc := cmdRouterLastInstance([]string{"--state", dir, "--instance", "orders"}); rc != 0 {
			t.Fatalf("exit %d, want 0 — a folder with an empty upstream map names no other instance", rc)
		}
	})

	// ⛔ SAFE WHEN IT CANNOT TELL. An unreadable state must NOT report "last": the consequence of a
	// wrong yes is deleting a live machine's registration, and of a wrong no is leaving a row the reap
	// clears within a day.
	t.Run("an unreadable state does not report LAST", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(dir+"/state.json", []byte("{not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		if rc := cmdRouterLastInstance([]string{"--state", dir, "--instance", "orders"}); rc == 0 {
			t.Fatal("reported LAST from a state it could not read. A wrong YES deletes the " +
				"registration of a machine that may still be routing; a wrong NO costs a row the reap " +
				"clears anyway. The asymmetry decides the default.")
		}
	})

	t.Run("--instance is required", func(t *testing.T) {
		dir := write(t, folder("/a", "orders"))
		if rc := cmdRouterLastInstance([]string{"--state", dir}); rc == 0 {
			t.Fatal("exit 0 with no --instance; the predicate is meaningless without one")
		}
	})
}
