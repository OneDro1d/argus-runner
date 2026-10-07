package updatecmd

import (
	"strings"
	"testing"
)

// unconfirmed_gate_test.go — AC-D53: THE DOWNGRADE GATE ACROSS RUNS, PLAYED THROUGH THE REAL PLAN AND THE REAL MARK
// (the machine of unconfirmed_rerun_test.go). The gate's truth: a target below ANY version the executor may be on is a
// downgrade and needs --rollback-to; a target at or above every such version is not refused.

// ⛔ PREV-4 (round-3 review): AN EXECUTOR NOBODY COULD READ, UNDER AN EARLIER MARKER, MAY BE ON ANY VERSION THAT MARKER
// NAMED. The plan wrote from_version "" and dropped them: a downgrade got through, and the refusal named a move that
// never happened ("from 0.3.32 to 0.3.34").
func TestPlan_AC_D53_AnUnreadExecutorMayBeOnEveryVersionTheEarlierMarkerNamed(t *testing.T) {
	t.Run("the refusal names every version", func(t *testing.T) {
		m := interrupted(t, "compose")      // 0.3.32 → 0.3.33, unconfirmed
		m.move("", "", "0.3.34", img34, "") // then 0.3.34 with the executor unread, unconfirmed too
		_, _, err := m.plan("", "", "0.3.33", img33, "")
		if err == nil {
			t.Fatal("the 0.3.33 block was planned while the executor may be on 0.3.34")
		}
		// the "so it is on" clause itself, not only the versions somewhere in the text (round-4 review L4-1: the target
		// and the "from one of" clause named them all, so a list that dropped them passed)
		for _, w := range []string{"DOWN", "so it is on one of 0.3.32, 0.3.33, 0.3.34,"} {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("the refusal does not say %q: %v", w, err)
			}
		}
		if strings.Contains(err.Error(), "from 0.3.32 to 0.3.34") {
			t.Errorf("the refusal names a move from 0.3.32 to 0.3.34, which never happened: %v", err)
		}
	})
	t.Run("a stale rollback planned with the executor unread does not open the gate", func(t *testing.T) {
		m := interrupted(t, "compose")                   // 0.3.32 → 0.3.33, unconfirmed
		m.move("", "", "0.3.31", img31, "0.3.31")        // the page's stale rollback block, executor unread, unconfirmed
		_, _, err := m.plan("", "", "0.3.32", img32, "") // an older forward block, the executor still unread
		if err == nil || !strings.Contains(err.Error(), "DOWN") {
			t.Errorf("a forward block for 0.3.32 was planned while the executor may be on 0.3.33: %v", err)
		}
		if err != nil && !strings.Contains(err.Error(), "so it is on one of 0.3.31, 0.3.32, 0.3.33,") {
			t.Errorf("the refusal does not name every version the executor may be on: %v", err)
		}
	})
	// design check hist M-3 (b): the literal "unknown" the re-hash writes is not a version the executor may be on
	t.Run("the carried versions are versions", func(t *testing.T) {
		// the record's version is carried only when the earlier move's origin is unknown: an instance onboarded by
		// `argus up` (no version), its first update planned with the executor unread, then a commit whose re-hash
		// could not read it either — the record now reads the literal "unknown"
		m := newMachine(t, "compose", Manifest{InstanceID: "i1", Tier: "compose", Executor: "ghcr.io/x/exec:slim"})
		m.move("", "", "0.3.33", img33, "")
		m.commit("unknown", "", "")
		m.move("", "", "0.3.34", img34, "")
		if marker := readFile(t, unconfirmedPath(m.state, "i1")); strings.Contains(marker, "unknown") {
			t.Errorf("the marker carries the literal \"unknown\" as a version:\n%s", marker)
		}
	})
}

// ⛔ round-4 review G-3: THE RECORD'S VERSION STILL BOUNDS THE GATE WHILE A MARKER STANDS. What discover read was what
// `argus version` reports — the ARGUS_VERSION override when one was set, until round 6 asked the binary for its own —
// so a "from" that was read could be lower than where the executor really is. Round 4 dropped the record's version
// whenever "from" was read, and a downgrade got through. The markers here carry such a reading as it was written.
func TestPlan_AC_D53_TheRecordStillBoundsTheGate(t *testing.T) {
	t.Run("a read origin lower than the record", func(t *testing.T) {
		// on 0.3.33 (previous 0.3.32); the rollback block to 0.3.32, planned while the executor reported an override of
		// 0.3.31, ends unconfirmed — the executor may still be on 0.3.33
		m := newMachine(t, "compose", Manifest{InstanceID: "i1", Tier: "compose", Version: "0.3.33", Executor: img33,
			Previous: &Previous{Version: "0.3.32", Image: img32}})
		m.move("0.3.31", img33, "0.3.32", img32, "0.3.32")
		_, _, err := m.plan("", "", "0.3.32", img32, "") // `argus up --image <0.3.32's image>`: the FORWARD block
		if err == nil || !strings.Contains(err.Error(), "DOWN") || !strings.Contains(err.Error(), "0.3.33") {
			t.Errorf("a forward block for 0.3.32 was planned while the executor may be on 0.3.33: %v", err)
		}
	})
	// ⚠ THE PRICE, KEPT ON PURPOSE (the operator's rule: a refusal armed wrongly is preferred to one cleared wrongly):
	// the same rollback run twice, each unconfirmed, leaves the executor on 0.3.32 only — and the forward block to 0.3.32
	// is still refused, naming the path that works: re-running the rollback block.
	t.Run("the price: the same rollback twice, then the forward block to its version", func(t *testing.T) {
		m := newMachine(t, "compose", Manifest{InstanceID: "i1", Tier: "compose", Version: "0.3.33", Executor: img33,
			Previous: &Previous{Version: "0.3.32", Image: img32}})
		m.move("0.3.33", img33, "0.3.32", img32, "0.3.32")
		m.move("0.3.32", img32, "0.3.32", img32, "0.3.32")
		_, _, err := m.plan("", "", "0.3.32", img32, "")
		if err == nil || !strings.Contains(err.Error(), "Re-run that rollback to 0.3.32") {
			t.Errorf("the refusal does not name the path that works: %v", err)
		}
		if _, _, err := m.plan("", "", "0.3.32", img32, "0.3.32"); err != nil {
			t.Errorf("re-running the rollback block was refused: %v", err)
		}
	})
}

// ⛔ round-4 review G-1: DISCOVER'S READING DOES NOT HOLD THE GATE. It was the ARGUS_VERSION override when one was set
// (discover asks the binary for its own since round 6: TestDiscoverScript_ReadsTheBinarysOwnVersionNeverTheOverride),
// and round 4 counted it in the gate, so an override above the target refused every correct forward update. A reading
// above the target is handed to the plan here as it was then; the gate still does not count it.
func TestPlan_AC_D53_AnOverriddenReadingDoesNotHoldTheGate(t *testing.T) {
	for _, tier := range []string{"compose", "k3d"} {
		t.Run(tier, func(t *testing.T) {
			m := newMachine(t, tier, Manifest{InstanceID: "i1", Tier: tier, Version: "0.3.33", Executor: img33,
				Previous: &Previous{Version: "0.3.32", Image: img32}})
			if _, _, err := m.plan("0.9.9-pinned", img33, "0.3.34", img34, ""); err != nil {
				t.Errorf("a forward update to 0.3.34 was refused because the executor reports a pinned 0.9.9: %v", err)
			}
		})
	}
}

// ⛔ round-4 review G-6: THE REFUSAL'S LIST IS THE VERSIONS IT NAMES. Markers as the file may hold them (the sentence,
// not a history): a "to" nobody recorded is a place the executor may be; an origin that is not an orderable version is
// named as it is; one version written with and without its "v" is one version.
func TestPlan_AC_D53_TheRefusalListsTheVersionsItNames(t *testing.T) {
	view := func(t *testing.T, marker string) Manifest {
		t.Helper()
		state := t.TempDir()
		if err := WriteManifest(state, Manifest{InstanceID: "i1", Tier: "compose"}); err != nil {
			t.Fatal(err)
		}
		writeMarker(t, state, "i1", marker)
		m, err := ReadManifest(state, "i1")
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	for _, c := range []struct {
		name, marker, target, want, never string
	}{
		{"an unrecorded target", "image=" + img34 + "\nfrom_version=0.3.35\nfrom_image=" + img33 + "\n", "0.3.34",
			"so it is on one of 0.3.35, an unrecorded version,", ""},
		{"an origin that is not a version", markerBody(img34, "0.3.36", "", "dev-build", img33), "0.3.35",
			"so it is on one of 0.3.36, dev-build,", ""},
		{"one version with and without its v", markerBody(img34, "v0.3.36", "", "0.3.36", img33), "0.3.35",
			"so it is on v0.3.36,", "0.3.36, 0.3.36"},
	} {
		t.Run(c.name, func(t *testing.T) {
			in := fixtureInput("compose")
			in.Installed = view(t, c.marker)
			in.Version, in.ImageDigest = c.target, img32
			_, err := BuildPlan(in)
			if err == nil {
				t.Fatal("planned")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("the refusal does not say %q: %v", c.want, err)
			}
			if c.never != "" && strings.Contains(err.Error(), c.never) {
				t.Errorf("the refusal names one version twice (%q): %v", c.never, err)
			}
		})
	}
}

// ⛔ design check hist M-3 (a): "SO IT IS ON …" NAMES AN UNRECORDED ORIGIN TOO. An instance onboarded by `argus up`
// records no version; its first update, with the executor unread, came from a version nobody recorded — the executor
// may still be there.
func TestPlan_AC_D53_TheRefusalNamesAnUnrecordedOrigin(t *testing.T) {
	m := newMachine(t, "compose", Manifest{InstanceID: "i1", Tier: "compose", Executor: "ghcr.io/x/exec:slim"})
	m.move("", "", "0.3.33", img33, "")
	_, _, err := m.plan("", "", "0.3.32", img32, "")
	if err == nil {
		t.Fatal("the 0.3.32 block was planned while the executor may be on 0.3.33")
	}
	if !strings.Contains(err.Error(), "so it is on one of 0.3.33, an unrecorded version") {
		t.Errorf("the refusal does not say the executor may still be on the version nobody recorded: %v", err)
	}
}

// ⛔ round-5 review R5-MSG-1: EVERY VERSION THE REFUSAL NAMES, IT EXPLAINS. The gate counts the record's version too
// (TestPlan_AC_D53_TheRecordStillBoundsTheGate) — so when it is neither where the move came from nor where it was going,
// the sentence says where it comes from: the installed record. Otherwise it is named once.
func TestPlan_AC_D53_TheRefusalSaysWhereTheRecordsVersionComesFrom(t *testing.T) {
	record := func(tier string) Manifest {
		return Manifest{InstanceID: "i1", Tier: tier, Version: "0.3.33", Executor: img33,
			Previous: &Previous{Version: "0.3.32", Image: img32}}
	}
	for _, c := range []struct {
		name  string
		setup func(t *testing.T) *machine
		plan  [2]string // target version, image
		want  string
		never string
	}{
		{"a stale record below the move's origin", func(t *testing.T) *machine {
			m := interrupted(t, "compose") // landed on 0.3.33 unconfirmed; the record still says 0.3.32
			m.move("0.3.33", img33, "0.3.34", img34, "")
			return m
		}, [2]string{"0.3.33", img33},
			"moved its executor from 0.3.33 to 0.3.34 and could not confirm it, and its installed record still says 0.3.32, " +
				"so it is on one of 0.3.32, 0.3.33, 0.3.34", ""},
		{"the price: the same rollback twice, then the forward block", func(t *testing.T) *machine {
			m := newMachine(t, "compose", record("compose"))
			m.move("0.3.33", img33, "0.3.32", img32, "0.3.32")
			m.move("0.3.32", img32, "0.3.32", img32, "0.3.32")
			return m
		}, [2]string{"0.3.32", img32},
			"and could not confirm it, and its installed record still says 0.3.33, so it is on one of 0.3.32, 0.3.33", ""},
		{"the record is where the move came from: named once", func(t *testing.T) *machine {
			m := newMachine(t, "compose", record("compose"))
			m.move("0.3.33", img33, "0.3.34", img34, "")
			return m
		}, [2]string{"0.3.33", img33},
			"moved its executor from 0.3.33 to 0.3.34 and could not confirm it, so it is on one of 0.3.33, 0.3.34",
			"installed record"},
		{"k3d: the unknown commit left the record no version", func(t *testing.T) *machine {
			m := interrupted(t, "k3d")
			m.move("0.3.33", img33, "0.3.34", img34, "")
			m.commit("", "", "")
			return m
		}, [2]string{"0.3.33", img33},
			"moved its executor from 0.3.33 to 0.3.34 and could not confirm it, so it is on one of 0.3.33, 0.3.34",
			"installed record"},
		// round-6 design check S1/S6: on k3d the unknown commit blanks the record's version, and an unread re-run of the
		// same block carries the earlier marker's versions — the record's of that time among them
		{"k3d: an unread re-run carried an earlier record's version", func(t *testing.T) *machine {
			m := newMachine(t, "k3d", Manifest{InstanceID: "i1", Tier: "k3d", Version: "0.3.32", Executor: img32,
				Previous: &Previous{Version: "0.3.31", Image: img31}})
			m.move("0.3.32", img32, "0.3.33", img33, "")
			m.move("0.3.33", img33, "0.3.34", img34, "")
			m.move("", "", "0.3.34", img34, "")
			m.commit("", "", "")
			return m
		}, [2]string{"0.3.33", img33},
			// round-6 review R6-MSG-1: the version came from an earlier RECORD the unread re-run carried, not from where an
			// earlier run moved it — the words say only what is known: an earlier unconfirmed run named it
			"moved its executor from 0.3.33 to 0.3.34 and could not confirm it, and an earlier unconfirmed run also " +
				"named 0.3.32, so it is on one of 0.3.32, 0.3.33, 0.3.34", "may have left it"},
		{"k3d: the same rollback three times, the last unread", func(t *testing.T) *machine {
			m := newMachine(t, "k3d", record("k3d"))
			m.move("0.3.33", img33, "0.3.32", img32, "0.3.32")
			m.move("0.3.32", img32, "0.3.32", img32, "0.3.32")
			m.move("", "", "0.3.32", img32, "0.3.32")
			m.commit("", "", "0.3.32")
			return m
		}, [2]string{"0.3.32", img32},
			"moved its executor from 0.3.32 to 0.3.32 and could not confirm it, and an earlier unconfirmed run also " +
				"named 0.3.33, so it is on one of 0.3.32, 0.3.33", "may have left it"},
		// markers as the file may hold them: "from" is every version an earlier marker named, or nothing
		{"the record among the versions an earlier marker named: named once", func(t *testing.T) *machine {
			m := newMachine(t, "compose", record("compose"))
			writeMarker(t, m.state, "i1", "image="+img34+"\nversion=0.3.34\nfrom_one_of=0.3.32 0.3.33\n")
			return m
		}, [2]string{"0.3.33", img33},
			"moved its executor from one of 0.3.32, 0.3.33 to 0.3.34 and could not confirm it, so it is on one of", "installed record"},
		{"the record outside the versions an earlier marker named", func(t *testing.T) *machine {
			m := newMachine(t, "compose", record("compose"))
			writeMarker(t, m.state, "i1", "image="+img34+"\nversion=0.3.34\nfrom_one_of=0.3.31 0.3.32\n")
			return m
		}, [2]string{"0.3.33", img33},
			"could not confirm it, and its installed record still says 0.3.33, so it is on one of", ""},
		{"the record is where the move was going: named once", func(t *testing.T) *machine {
			m := newMachine(t, "compose", Manifest{InstanceID: "i1", Tier: "compose", Version: "0.3.34", Executor: img34})
			m.move("0.3.33", img33, "0.3.34", img34, "") // the executor read on 0.3.33, the record says 0.3.34
			return m
		}, [2]string{"0.3.33", img33},
			"moved its executor from 0.3.33 to 0.3.34 and could not confirm it, so it is on one of 0.3.33, 0.3.34",
			"installed record"},
		{"one version with and without its v: named once", func(t *testing.T) *machine {
			m := newMachine(t, "compose", record("compose"))
			writeMarker(t, m.state, "i1", "image="+img34+"\nversion=0.3.34\nfrom_version=v0.3.33\nfrom_image="+img33+"\n")
			return m
		}, [2]string{"0.3.33", img33},
			"moved its executor from v0.3.33 to 0.3.34 and could not confirm it, so it is on one of", "installed record"},
		{"no origin recorded: the record is where it came from, named once", func(t *testing.T) *machine {
			m := newMachine(t, "compose", record("compose"))
			writeMarker(t, m.state, "i1", "image="+img34+"\nversion=0.3.34\n")
			return m
		}, [2]string{"0.3.33", img33},
			"moved its executor from 0.3.33 to 0.3.34 and could not confirm it, so it is on one of 0.3.33, 0.3.34",
			"installed record"},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := c.setup(t)
			_, _, err := m.plan("", "", c.plan[0], c.plan[1], "")
			if err == nil {
				t.Fatalf("precondition: the plan to %s was not refused", c.plan[0])
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("the refusal does not say it:\n got: %v\nwant: …%s…", err, c.want)
			}
			if c.never != "" && strings.Contains(err.Error(), c.never) {
				t.Errorf("the refusal names %q where it adds nothing: %v", c.never, err)
			}
		})
	}
}
