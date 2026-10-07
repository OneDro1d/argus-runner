package updatecmd

import (
	"os/exec"
	"strings"
	"testing"
	"time"
)

// unconfirmed_rerun_test.go — AC-D53: A RUN AFTER A RUN, PLAYED THROUGH THE REAL PLAN AND THE REAL MARK.
//
// The round-3 review found `previous` wrong in histories no test played: every marker in the tests was written by
// hand, so what the PLAN puts into a marker — from the record, the earlier marker and what discover read — was never
// what a later commit read. Here each run is planned from what is on disk (ReadManifest's view, as `update plan`
// reads it, through the real BuildPlan and its gate), its marker is written by apply.sh's own mark_unconfirmed in
// bash from the rendered literals, and each commit is the real CommitManifest given the version A-8a's re-hash gives:
// the move's target when it landed confirmed, else a READING of the executor — this run's own when its plan read it,
// or a LATER run's that reads it and moves nothing. (An executor nobody read is committed "unknown": see
// TestManifest_AC_D53_AnUnreadRunCommitsTheExecutorUnknown.)

// machine is one instance's router state, as the runs leave it.
type machine struct {
	t     *testing.T
	state string
	tier  string
}

func newMachine(t *testing.T, tier string, record Manifest) *machine {
	t.Helper()
	m := &machine{t: t, state: t.TempDir(), tier: tier}
	if record.InstanceID != "" {
		if err := WriteManifest(m.state, record); err != nil {
			t.Fatal(err)
		}
	}
	return m
}

// plan is `update plan` for a block moving the executor to version/img (a rollback block when rollbackTo is set), with
// discover having read the executor on obs/obsImg ("" = it could not read it). It returns the plan's refusal, if any.
func (m *machine) plan(obs, obsImg, version, img, rollbackTo string) (Plan, PlanInput, error) {
	m.t.Helper()
	view, err := ReadManifest(m.state, "i1")
	if err != nil {
		m.t.Fatal(err)
	}
	in := fixtureInput(m.tier)
	in.RouterState = m.state
	in.Installed = view
	in.Version, in.ImageDigest, in.RollbackTo = version, img, rollbackTo
	in.Observed.ExecutorVersion = obs
	if m.tier == "compose" {
		in.Observed.Compose.Services[0].Image, in.Observed.Compose.Services[0].Digest = obsImg, obsImg
	} else {
		in.Observed.K8s.Objects[0].Image, in.Observed.K8s.Objects[0].Digest = obsImg, obsImg
	}
	if obsImg == "" {
		// nothing read: discover saw no executor image either
		if m.tier == "compose" {
			in.Observed.Compose.Services[0].Digest = ""
		} else {
			in.Observed.K8s.Objects[0].Digest = ""
		}
	}
	p, err := BuildPlan(in)
	return p, in, err
}

// move is a run whose A-3 moved the executor: planned as above, and marked by apply.sh's own mark_unconfirmed.
func (m *machine) move(obs, obsImg, version, img, rollbackTo string) {
	m.t.Helper()
	p, in, err := m.plan(obs, obsImg, version, img, rollbackTo)
	if err != nil {
		m.t.Fatalf("precondition: the plan to %s refused: %v", version, err)
	}
	apply := RenderApply(p, in)
	lit := strings.TrimSuffix(renderLiterals(p, in), scriptHelpers)
	const begin, end = "# ── AC-D53 verdict helpers ──", "# ── end of the verdict helpers ──"
	i, j := strings.Index(apply, begin), strings.Index(apply, end)
	if i < 0 || j < i {
		m.t.Fatalf("apply.sh carries no verdict helpers")
	}
	script := "set -euo pipefail\n" + lit + apply[i:j] + "\nmark_unconfirmed\n"
	if out, err := exec.Command("bash", "-c", script).CombinedOutput(); err != nil {
		m.t.Fatalf("mark_unconfirmed did not run: %v\n%s", err, out)
	}
}

// commit is A-8a: the executor read on version/img, or unknown (version "").
func (m *machine) commit(version, img, rollbackTo string) Manifest {
	m.t.Helper()
	got, err := CommitManifest(m.state, Manifest{InstanceID: "i1", Tier: m.tier, Version: version, Executor: img},
		CommitOpts{RollbackTo: rollbackTo, Outcome: "updated"})
	if err != nil {
		m.t.Fatal(err)
	}
	return got
}

// interrupted is a forward move from 0.3.32 to 0.3.33 that LANDED and could not be confirmed (rollback-unreachable):
// its marker stands. On compose its A-8a could not run (the record stays); on k3d it committed the executor unknown.
func interrupted(t *testing.T, tier string) *machine {
	t.Helper()
	m := newMachine(t, tier, Manifest{InstanceID: "i1", Tier: tier, Version: "0.3.32", Executor: img32,
		Previous: &Previous{Version: "0.3.31", Image: img31}})
	m.move("0.3.32", img32, "0.3.33", img33, "")
	if tier != "compose" {
		m.commit("", "", "")
	}
	return m
}

// ⛔ PREV-1 (round-3 review, high): AFTER AN UNCONFIRMED MOVE THAT LANDED, THE RECORD IS STALE — AND "PREVIOUS STAYS"
// MUST NOT MEAN "THE RECORD'S PREVIOUS". The executor is on 0.3.33, which it reached from 0.3.32; the record still
// says 0.3.32 with previous 0.3.31. The NOTE's own advice — re-run the update — recorded previous 0.3.31, and the page
// offered a rollback two releases back.
func TestManifest_AC_D53_ARunAfterAnUnconfirmedMoveThatLandedKeepsItsOrigin(t *testing.T) {
	for _, tier := range []string{"compose", "k3d"} {
		for _, c := range []struct {
			name string
			run  func(m *machine) Manifest
			want string
		}{
			{"the same update again", func(m *machine) Manifest {
				m.move("0.3.33", img33, "0.3.33", img33, "")
				return m.commit("0.3.33", img33, "")
			}, "0.3.32 / " + img32},
			{"an update to 0.3.34 that did not land", func(m *machine) Manifest {
				m.move("0.3.33", img33, "0.3.34", img34, "")
				return m.commit("0.3.33", img33, "")
			}, "0.3.32 / " + img32},
			{"an update to 0.3.34 that landed", func(m *machine) Manifest {
				m.move("0.3.33", img33, "0.3.34", img34, "")
				return m.commit("0.3.34", img34, "")
			}, "0.3.33 / " + img33},
			{"the page's stale rollback block to 0.3.31, which did not land", func(m *machine) Manifest {
				m.move("0.3.33", img33, "0.3.31", img31, "0.3.31")
				return m.commit("0.3.33", img33, "0.3.31")
			}, "0.3.32 / " + img32},
			// design check bash L-1: the NOTE's own advice, re-running the same update, with discover unable to read the
			// executor. Both histories give the same answer: if the earlier move landed, this run moved nothing and
			// the earlier move's origin stands; if it did not, this run moved 0.3.32 → 0.3.33.
			{"the same update again, the executor unread", func(m *machine) Manifest {
				m.move("", "", "0.3.33", img33, "")
				return m.commit("0.3.33", img33, "")
			}, "0.3.32 / " + img32},
			// …and the move put back; a later run reads the executor on 0.3.32: the earlier move did not land either
			{"the same update again, the executor unread, put back, then read on 0.3.32", func(m *machine) Manifest {
				m.move("", "", "0.3.33", img33, "")
				return m.commit("0.3.32", img32, "")
			}, "0.3.31 / " + img31},
			{"a run that moved nothing", func(m *machine) Manifest {
				if _, _, err := m.plan("0.3.33", img33, "0.3.34", img34, ""); err != nil {
					t.Fatal(err)
				}
				return m.commit("0.3.33", img33, "")
			}, "0.3.32 / " + img32},
			// the later run could not read the executor: where it came from is unknown, and nothing is invented
			{"an update the plan could not read the executor for, landed", func(m *machine) Manifest {
				m.move("", "", "0.3.34", img34, "")
				return m.commit("0.3.34", img34, "")
			}, "<none>"},
			// (put back, the run itself commits "unknown"; a later run reads 0.3.33: one of the histories that stays open —
			// an unread run between an unconfirmed move that landed and the next reading loses `previous`)
			{"an update the plan could not read the executor for, put back, then read on 0.3.33", func(m *machine) Manifest {
				m.move("", "", "0.3.34", img34, "")
				return m.commit("0.3.33", img33, "")
			}, "<none>"},
		} {
			t.Run(tier+"/"+c.name, func(t *testing.T) {
				m := interrupted(t, tier)
				if p := prevString(c.run(m).Previous); p != c.want {
					t.Errorf("previous %s, want %s", p, c.want)
				}
			})
		}
	}
}

// ⛔ PREV-2 (round-3 review): ONLY A PINNED IMAGE BECOMES `previous` — the V32 adversary-gate rule (rehash.go), which
// the marker bypassed: every forward move commits with its own marker standing, so `previous` came from the marker's
// from_image, and on k3d that is the Deployment's image string — a TAG when the instance was onboarded from one. The
// rollback block can never be rendered for a tag, so the page showed no block and no reason.
func TestManifest_AC_D53_ATagNeverBecomesPrevious(t *testing.T) {
	for _, tier := range []string{"compose", "k3d"} {
		t.Run(tier, func(t *testing.T) {
			const tag = "ghcr.io/x/exec:slim"
			// the onboarding record: no version, the executor it was onboarded from
			m := newMachine(t, tier, Manifest{InstanceID: "i1", Tier: tier, Executor: tag})
			m.move("0.3.31", tag, "0.3.32", img32, "")
			got := m.commit("0.3.32", img32, "")
			if got.Previous != nil {
				t.Errorf("previous %s — a tag, which the rollback block can never be rendered for", prevString(got.Previous))
			}
			if r := RollbackReason(got); !strings.Contains(r, "no previous version is recorded") {
				t.Errorf("the reason no rollback is offered is %q", r)
			}
		})
	}
}

// ⛔ round-4 review PREV-R4-1: WITH NO MARKER, A RECORD THE EXECUTOR IS NOT ON IS STALE TOO. The executor was moved
// outside an update (a hand-run `kubectl set image` or `docker compose up`), so discover reads 0.3.33 over a record of
// 0.3.32. A run that failed before A-3 committed previous 0.3.32 — the commit's own rule for a record and a reading that
// differ — while a run that moved and was undone committed the record's 0.3.31, one release too far back. One machine
// state, one answer.
func TestManifest_AC_D53_ARecordTheExecutorLeftIsWhereItCameFrom(t *testing.T) {
	for _, tier := range []string{"compose", "k3d"} {
		for _, c := range []struct {
			name string
			run  func(m *machine) Manifest
		}{
			{"the run failed before A-3", func(m *machine) Manifest {
				if _, _, err := m.plan("0.3.33", img33, "0.3.34", img34, ""); err != nil {
					t.Fatal(err)
				}
				return m.commit("0.3.33", img33, "")
			}},
			{"the run moved the executor and was undone", func(m *machine) Manifest {
				m.move("0.3.33", img33, "0.3.34", img34, "")
				return m.commit("0.3.33", img33, "")
			}},
		} {
			t.Run(tier+"/"+c.name, func(t *testing.T) {
				m := newMachine(t, tier, Manifest{InstanceID: "i1", Tier: tier, Version: "0.3.32", Executor: img32,
					Previous: &Previous{Version: "0.3.31", Image: img31}})
				if p := prevString(c.run(m).Previous); p != "0.3.32 / "+img32 {
					t.Errorf("previous %s, want 0.3.32 — where the executor came from", p)
				}
			})
		}
	}
}

// ⛔ round-4 review L4-3: WHAT THE UNREAD RUN ITSELF LEAVES. A run whose plan could not read the executor, and whose
// move was undone, commits what the real A-8a commits: the re-hash's "unknown" (it moved nothing it can name, and read
// nothing). The marker stays, the record's `previous` stays — never offered, since the view reads the version unknown —
// and a LATER reading settles it (the histories above).
func TestManifest_AC_D53_AnUnreadRunCommitsTheExecutorUnknown(t *testing.T) {
	m := interrupted(t, "compose")
	m.move("", "", "0.3.34", img34, "")
	p, in, err := m.plan("", "", "0.3.34", img34, "") // the same plan, for the re-hash
	if err != nil {
		t.Fatal(err)
	}
	rh := Rehash(p, map[string]string{"A-1": "undone", "A-3": "undone"}, in.Observed, "rolled-back", "A-3")
	if rh.Version != "unknown" {
		t.Fatalf("precondition: the re-hash recorded %q for an executor it neither moved nor read", rh.Version)
	}
	m.commit(rh.Version, rh.Executor, "")
	if u, err := readUnconfirmed(m.state, "i1"); err != nil || u == nil {
		t.Fatalf("the marker was removed by a commit of an executor nobody read (%v)", err)
	}
	view, err := ReadManifest(m.state, "i1")
	if err != nil {
		t.Fatal(err)
	}
	if RollbackOffered(view) {
		t.Errorf("a rollback is offered for an executor nobody could read: %+v", view)
	}
}

// ⛔ A VERSION NOBODY READ IS UNKNOWN — ALSO ON A ROLLBACK RUN (design check, hist L-1). The re-hash recorded the
// rollback's TARGET as the version of an executor the run neither moved nor read (rehash.go previousVersionFrom); the
// commit took that for a reading, removed an earlier run's marker, and the gate opened below the version the executor
// may be on.
func TestManifest_AC_D53_ARollbackRunThatReadNothingRecordsNoVersion(t *testing.T) {
	m := interrupted(t, "compose")
	// the page's stale rollback block to 0.3.31, discover unable to read the executor — and A-1 failed, so A-3 never ran
	p, in, err := m.plan("", "", "0.3.31", img31, "0.3.31")
	if err != nil {
		t.Fatalf("precondition: the rollback block was refused: %v", err)
	}
	rh := Rehash(p, map[string]string{"A-1": "undone"}, in.Observed, "rolled-back", "A-1")
	if rh.Version == "0.3.31" {
		t.Errorf("the re-hash recorded the rollback's target 0.3.31 for an executor it neither moved nor read")
	}
	m.commit(rh.Version, rh.Executor, "0.3.31")
	if u, err := readUnconfirmed(m.state, "i1"); err != nil || u == nil {
		t.Fatalf("the earlier marker was removed by a commit of an executor nobody read (%v)", err)
	}
	if _, _, err := m.plan("", "", "0.3.32", img32, ""); err == nil || !strings.Contains(err.Error(), "DOWN") {
		t.Errorf("a forward block for 0.3.32 was planned while the executor may be on 0.3.33: %v", err)
	}
}

// ⛔ ONE FACT, ONE TIME (design check, hist L-3). A landed move's `previous` is stamped with when that move BEGAN — the
// marker's at= — whichever run resolves it: the commit that sees it landed, or a later plan that reads the executor on
// its target. It was the commit's own time on one path and the marker's on the other.
func TestManifest_AC_D53_ThePreviousOfALandedMoveIsStampedWhenTheMoveBegan(t *testing.T) {
	// the commit runs well after the mark, so its own time cannot pass for the mark's
	commitLater := func(m *machine) Manifest {
		got, err := CommitManifest(m.state, Manifest{InstanceID: "i1", Tier: m.tier, Version: "0.3.33", Executor: img33},
			CommitOpts{Outcome: "updated", Now: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	for _, c := range []struct {
		name string
		run  func(m *machine) Manifest
	}{
		{"resolved by a later plan (the same update again)", func(m *machine) Manifest {
			m.move("0.3.33", img33, "0.3.33", img33, "")
			return commitLater(m)
		}},
		{"resolved by the commit (a run that moved nothing)", commitLater},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := interrupted(t, "compose")
			e, err := readUnconfirmed(m.state, "i1")
			if err != nil || e == nil || e.At == "" {
				t.Fatalf("precondition: the interrupted move's marker %+v (%v)", e, err)
			}
			got := c.run(m)
			if got.Previous == nil || got.Previous.UpdatedAt != e.At {
				t.Errorf("previous %+v, want it stamped %s — when the move from 0.3.32 began", got.Previous, e.At)
			}
		})
	}
}

// ⛔ THE UNDO'S RE-MARK KEEPS WHEN THE MOVE BEGAN. The undo writes the marker again before it moves the executor back;
// its at= was the undo's own time, so "when the move began" was when it was being undone.
func TestAC_D53_TheUndosReMarkKeepsTheFirstMarksTime(t *testing.T) {
	requireBash(t)
	helpers := renderedVerdictHelpers(t)
	state := t.TempDir()
	script := "set -euo pipefail\nSTAGE=" + shq(t.TempDir()) + "\nROUTER_STATE=" + shq(state) +
		"\nINSTANCE=i1\nIMAGE_DIGEST=" + img33 + "\nTARGET_VERSION=0.3.33\nROLLBACK_TO=''\nFROM_VERSION=0.3.32\nFROM_IMAGE=" + img32 +
		"\nFROM_PREVIOUS_VERSION=0.3.31\nFROM_PREVIOUS_IMAGE=" + img31 + "\nFROM_PREVIOUS_AT=''\nFROM_ONE_OF=''\n" + helpers +
		"\nmark_unconfirmed\ngrep '^at=' \"$ROUTER_STATE/installed/$INSTANCE.unconfirmed\"\nsleep 1.2\nmark_unconfirmed\n" +
		"grep '^at=' \"$ROUTER_STATE/installed/$INSTANCE.unconfirmed\"\n"
	out, err := exec.Command("bash", "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("the marker helpers did not run: %v\n%s", err, out)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != 2 || lines[0] != lines[1] {
		t.Errorf("the second mark changed when the move began:\n%s", out)
	}
}

// ⛔ PREV-3 (round-3 review): THE LITERAL "unknown" IS NOT A READING. The re-hash records it for an executor nobody
// could read (rehash.go previousVersionFrom); the commit took it for a known executor and removed the marker — and the
// downgrade gate, reading an unorderable version, opened.
func TestManifest_AC_D53_AnUnknownLiteralKeepsTheMarker(t *testing.T) {
	m := interrupted(t, "compose")
	got := m.commit("unknown", "", "")
	if prevString(got.Previous) != "0.3.31 / "+img31 {
		t.Errorf("previous %s after a commit of an executor nobody read, want the record's own (unchanged)", prevString(got.Previous))
	}
	if u, err := readUnconfirmed(m.state, "i1"); err != nil || u == nil {
		t.Fatalf("the marker was removed by a commit of an executor nobody read (%v)", err)
	}
	if _, _, err := m.plan("", "", "0.3.32", img32, ""); err == nil || !strings.Contains(err.Error(), "DOWN") {
		t.Errorf("a forward block for 0.3.32 was planned while the executor may be on 0.3.33: %v", err)
	}
}
