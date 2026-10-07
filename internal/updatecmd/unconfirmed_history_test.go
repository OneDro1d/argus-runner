package updatecmd

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// unconfirmed_history_test.go — AC-D53: THE NOT-CONFIRMED MARKER ACROSS RUNS.
//
// The marker (manifest.go ReadManifest) outlives the run that wrote it whenever that run could not commit a known
// executor. Every test here is about what a LATER reader or a LATER run does with it: the plan's gates, the page's
// reason, `previous` at the next commit, and what `argus up` re-runs.

const (
	img31 = "ghcr.io/x/exec@sha256:v31"
	img32 = "ghcr.io/x/exec@sha256:v32"
	img33 = "ghcr.io/x/exec@sha256:v33"
	img34 = "ghcr.io/x/exec@sha256:v34"
)

func writeMarker(t *testing.T, state, id, body string) {
	t.Helper()
	if err := os.MkdirAll(state+"/installed", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unconfirmedPath(state, id), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// markerBody is a marker in apply.sh's key=value shape with the lines a test names — a marker SHAPE, not a history:
// mark_unconfirmed also writes from_previous_* (and from_one_of when its plan could not read the executor under an
// earlier marker). Histories are played through the real plan and mark instead (unconfirmed_rerun_test.go machine).
func markerBody(img, version, rollbackTo, fromVersion, fromImage string) string {
	b := "image=" + img + "\nversion=" + version + "\n"
	if rollbackTo != "" {
		b += "rollback_to=" + rollbackTo + "\n"
	}
	return b + "from_version=" + fromVersion + "\nfrom_image=" + fromImage + "\nat=2026-09-30T13:00:00Z\n"
}

// ⛔ WHERE THE EXECUTOR WAS BEFORE THE MOVE comes from the plan's own reading: the executor's own version when discover
// could read it; else the record's — but only when no earlier marker says that record is stale. Then it is unknown,
// and nothing is invented.
func TestRenderApply_AC_D53_TheMarkerRecordsWhereTheExecutorWas(t *testing.T) {
	for _, c := range []struct {
		name                 string
		obsVersion           string
		earlier              *Unconfirmed
		wantVersion, wantImg string
	}{
		{"the executor's own reading", "0.3.30", nil, "0.3.30", "sha256:old"},
		{"the record, when the executor could not be read", "", nil, "0.3.31", img31},
		{"unknown, when the record is under an earlier marker", "", &Unconfirmed{Image: img31, Version: "0.3.31"}, "", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			in := fixtureInput("compose")
			in.Observed.ExecutorVersion = c.obsVersion
			in.Installed = Manifest{InstanceID: "i1", Tier: "compose", Version: "0.3.31", Executor: img31}
			if c.earlier != nil {
				in.Installed.Version, in.Installed.Executor, in.Installed.Unconfirmed = "", "", c.earlier
			}
			if c.wantVersion == "0.3.30" {
				// the executor's reading wins even over a record that says otherwise
				in.Installed.Version = "0.3.29"
			}
			p, err := BuildPlan(in)
			if err != nil {
				t.Fatal(err)
			}
			a := RenderApply(p, in)
			for _, want := range []string{"FROM_VERSION=" + shq(c.wantVersion) + "\n", "FROM_IMAGE=" + shq(c.wantImg) + "\n"} {
				if !strings.Contains(a, want) {
					t.Errorf("apply.sh does not carry %q", strings.TrimSpace(want))
				}
			}
		})
	}
}

// ⛔ PREV-1: THE MARKER CARRIES THE `previous` TRUE WHERE THE PLAN FOUND THE EXECUTOR. With no earlier marker, the
// record's. Under an earlier marker the record is stale, so the plan resolves that marker with the executor where
// discover read it — exactly as a commit would now: landed forward → where it came from (pinned only), anything else →
// what that marker carried. Nothing read under an earlier marker: none.
func TestRenderApply_AC_D53_TheMarkerCarriesThePreviousTrueWhereTheExecutorWas(t *testing.T) {
	const tag = "ghcr.io/x/exec:slim"
	fp31 := &Previous{Version: "0.3.31", Image: img31, UpdatedAt: "2026-09-01T00:00:00Z"}
	fwd := &Unconfirmed{Image: img33, Version: "0.3.33", FromVersion: "0.3.32", FromImage: img32, FromPrevious: fp31, At: "2026-09-30T13:00:00Z"}
	for _, c := range []struct {
		name       string
		obsVersion string
		earlier    *Unconfirmed
		want       string // "<version> / <image> / <at>", or "" for none
	}{
		{"no earlier marker, the executor read", "0.3.32", nil, "0.3.31 / " + img31 + " / 2026-09-01T00:00:00Z"},
		{"no earlier marker, nothing read", "", nil, "0.3.31 / " + img31 + " / 2026-09-01T00:00:00Z"},
		{"an earlier forward move that landed", "0.3.33", fwd, "0.3.32 / " + img32 + " / 2026-09-30T13:00:00Z"},
		{"an earlier forward move that did not land", "0.3.32", fwd, "0.3.31 / " + img31 + " / 2026-09-01T00:00:00Z"},
		{"an earlier forward move that landed, from a tag", "0.3.33",
			&Unconfirmed{Image: img33, Version: "0.3.33", FromVersion: "0.3.32", FromImage: tag, FromPrevious: fp31}, ""},
		{"an earlier rollback that landed", "0.3.31",
			&Unconfirmed{Image: img31, Version: "0.3.31", RollbackTo: "0.3.31", FromVersion: "0.3.32", FromImage: img32, FromPrevious: fp31},
			"0.3.31 / " + img31 + " / 2026-09-01T00:00:00Z"},
		{"an earlier marker, nothing read", "", fwd, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			in := fixtureInput("compose")
			in.Version, in.ImageDigest = "0.3.34", img34
			in.Observed.ExecutorVersion = c.obsVersion
			in.Installed = Manifest{InstanceID: "i1", Tier: "compose", Version: "0.3.32", Executor: img32, Previous: fp31}
			if c.earlier != nil {
				in.Installed.Version, in.Installed.Executor, in.Installed.Unconfirmed = "", "", c.earlier
			}
			p, err := BuildPlan(in)
			if err != nil {
				t.Fatal(err)
			}
			a := RenderApply(p, in)
			v, img, at := "", "", ""
			if c.want != "" {
				parts := strings.Split(c.want, " / ")
				v, img, at = parts[0], parts[1], parts[2]
			}
			for _, want := range []string{"FROM_PREVIOUS_VERSION=" + shq(v) + "\n", "FROM_PREVIOUS_IMAGE=" + shq(img) + "\n",
				"FROM_PREVIOUS_AT=" + shq(at) + "\n"} {
				if !strings.Contains(a, want) {
					t.Errorf("apply.sh does not carry %q", strings.TrimSpace(want))
				}
			}
		})
	}
}

// ⛔ THE PAGE LEARNS WHY THE VERSION IS UNKNOWN — AND THE RECORD NEVER STORES THE MARKER. RollbackReason had a
// branch for the marker that could never run: both production callers read a manifest that had come through JSON
// (the page reads the control plane's stored copy; the report posts ReadManifest's view), and the field was
// json:"-". The page got "" — its contract for "no manifest has ever been recorded".
func TestManifest_AC_D53_TheReportCarriesTheNotConfirmedStateAndTheRecordNever(t *testing.T) {
	state := t.TempDir()
	recordBefore(t, state)
	writeMarker(t, state, "i1", markerBody(newImg, "0.3.32", "", "0.3.31", oldImg))

	view, err := ReadManifest(state, "i1")
	if err != nil {
		t.Fatal(err)
	}
	// the report (cmd/argus update.go cmdUpdateReport) posts json.Marshal of this view; the page parses it back
	// (control/instanceview.go installedManifestOf) — the same round trip, here
	body, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	var page Manifest
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatal(err)
	}
	if page.Unconfirmed == nil || page.Unconfirmed.Image != newImg {
		t.Fatalf("the reported manifest does not carry the not-confirmed state: %s", body)
	}
	if strings.Contains(string(body), "record_version") || strings.Contains(string(body), "0.3.31\",\"executor") {
		t.Errorf("the report carries the stale record's version under the marker: %s", body)
	}
	r := RollbackReason(page)
	if r == "" || !strings.Contains(r, "unknown") {
		t.Errorf("the page's reason for an instance whose executor move is not confirmed is %q — want why the version is unknown", r)
	}

	// and the record: a manifest written with the view's field set must not store it
	fresh := t.TempDir()
	if err := WriteManifest(fresh, view); err != nil {
		t.Fatal(err)
	}
	if b := readFile(t, manifestPath(fresh, "i1")); strings.Contains(b, "unconfirmed") {
		t.Errorf("the record stores the marker — it is the machine's own fact about a move in progress:\n%s", b)
	}
}

func prevString(p *Previous) string {
	if p == nil {
		return "<none>"
	}
	return p.Version + " / " + p.Image
}

// ⛔ `previous` AFTER A MOVE NOBODY COULD CONFIRM — ONE RULE, ON EVERY TIER. The marker standing at the commit names
// the move that was not confirmed: from where, to where, and the `previous` true where it started. The executor on
// the move's target means it landed, and `previous` is where it came from; anywhere else means it did not land, and
// `previous` is what it was there. The rule reads the marker, never the record under it (on compose that record is
// stale — the interrupted attempt could not write its own), and never a flag a retried commit would apply twice.
//
// Every marker here is the PLAN's (unconfirmed_rerun_test.go machine): hand-written markers carried what the test
// author believed the plan writes, and the round-3 review found histories where it wrote something else.
func TestManifest_AC_D53_PreviousAfterAnUnconfirmedMove(t *testing.T) {
	onRec33 := Manifest{InstanceID: "i1", Version: "0.3.33", Executor: img33, Previous: &Previous{Version: "0.3.32", Image: img32}}
	onRec32 := Manifest{InstanceID: "i1", Version: "0.3.32", Executor: img32, Previous: &Previous{Version: "0.3.31", Image: img31}}
	onRec31 := Manifest{InstanceID: "i1", Version: "0.3.31", Executor: img31}
	type move struct{ obs, obsImg, to, toImg, rollbackTo string }
	for _, h := range []struct {
		name   string
		before Manifest // the record before the interrupted attempt
		first  move     // the interrupted attempt's move, which nobody could confirm
		later  *move    // the later run's own move (its marker written over the first), or nil: it stopped before A-3
		// where the executor is at the later run's commit
		endsOn, endsImg string
		want            string // "<version> / <image>" of the previous that commit must record, or "<none>"
	}{
		{
			// K-1: on 0.3.33, a rollback to 0.3.32 landed but could not be confirmed; then a forward update to 0.3.34,
			// which read the executor on 0.3.32 before its own move
			name: "an unconfirmed rollback that landed, then a forward update", before: onRec33,
			first: move{"0.3.33", img33, "0.3.32", img32, "0.3.32"},
			later: &move{"0.3.32", img32, "0.3.34", img34, ""}, endsOn: "0.3.34", endsImg: img34,
			want: "0.3.32 / " + img32,
		},
		{
			name: "an unconfirmed forward update that landed, then another", before: onRec31,
			first: move{"0.3.31", img31, "0.3.32", img32, ""},
			later: &move{"0.3.32", img32, "0.3.33", img33, ""}, endsOn: "0.3.33", endsImg: img33,
			want: "0.3.32 / " + img32,
		},
		{
			// …and the later run could not read the executor, with the record under an earlier marker: "from" is unknown
			// (TestRenderApply_AC_D53_TheMarkerRecordsWhereTheExecutorWas), and nothing is invented
			name: "an unconfirmed forward update that landed, then another that could not read the executor", before: onRec31,
			first: move{"0.3.31", img31, "0.3.32", img32, ""},
			later: &move{"", "", "0.3.33", img33, ""}, endsOn: "0.3.33", endsImg: img33,
			want: "<none>",
		},
		{
			// R8-1: on 0.3.32 (previous 0.3.31), a forward update to 0.3.33 did NOT land (the undo put 0.3.32 back and
			// could not confirm it); the re-run of the same update fails again and is undone cleanly — its own marker
			// stands at its commit, which records 0.3.32. Nothing ever moved: previous stays 0.3.31.
			name: "an unconfirmed forward update that did not land, then a re-run that rolled back", before: onRec32,
			first: move{"0.3.32", img32, "0.3.33", img33, ""},
			later: &move{"0.3.32", img32, "0.3.33", img33, ""}, endsOn: "0.3.32", endsImg: img32,
			want: "0.3.31 / " + img31,
		},
		{
			// …and a later run that never moved the executor at all (it stopped before A-3): the earlier marker stands
			name: "an unconfirmed forward update that did not land, then a run that moved nothing", before: onRec32,
			first:  move{"0.3.32", img32, "0.3.33", img33, ""},
			endsOn: "0.3.32", endsImg: img32,
			want: "0.3.31 / " + img31,
		},
		{
			// an unconfirmed ROLLBACK that landed, then a run that moved nothing: a rollback leaves previous alone (P-9)
			name: "an unconfirmed rollback that landed, then a run that moved nothing", before: onRec33,
			first:  move{"0.3.33", img33, "0.3.32", img32, "0.3.32"},
			endsOn: "0.3.32", endsImg: img32,
			want: "0.3.32 / " + img32,
		},
	} {
		// compose: the interrupted attempt's A-8a could not run (the record from before stays under the marker);
		// k3d: it ran, and recorded the executor unknown
		for _, tier := range []string{"compose", "k3d"} {
			t.Run(tier+"/"+h.name, func(t *testing.T) {
				before := h.before
				before.Tier = tier
				m := newMachine(t, tier, before)
				m.move(h.first.obs, h.first.obsImg, h.first.to, h.first.toImg, h.first.rollbackTo)
				if tier != "compose" {
					m.commit("", "", h.first.rollbackTo)
				}
				if h.later != nil {
					m.move(h.later.obs, h.later.obsImg, h.later.to, h.later.toImg, h.later.rollbackTo)
				}
				if p := prevString(m.commit(h.endsOn, h.endsImg, "").Previous); p != h.want {
					t.Errorf("previous %s, want %s", p, h.want)
				}
			})
		}
	}
}

// ⛔ BASH-1: A COMMIT RUN TWICE RECORDS THE SAME. runner() repeats a verb whose container exit was 125, and docker
// can return 125 after the verb itself ran; the second commit then reads the record the first one wrote. A rule
// that re-applied "the record is stale" to it recorded the version just installed as `previous`.
func TestManifest_AC_D53_ARetriedCommitRecordsTheSame(t *testing.T) {
	for _, c := range []struct {
		name   string
		marker string
	}{
		{"after an earlier unconfirmed move", markerBody(img34, "0.3.34", "", "0.3.33", img33)},
		{"with no marker", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			state := t.TempDir()
			if err := WriteManifest(state, Manifest{InstanceID: "i1", Tier: "compose", Version: "0.3.33", Executor: img33,
				Previous: &Previous{Version: "0.3.32", Image: img32}}); err != nil {
				t.Fatal(err)
			}
			if c.marker != "" {
				writeMarker(t, state, "i1", c.marker)
			}
			next := Manifest{InstanceID: "i1", Tier: "compose", Version: "0.3.34", Executor: img34}
			first, err := CommitManifest(state, next, CommitOpts{Outcome: "updated"})
			if err != nil {
				t.Fatal(err)
			}
			second, err := CommitManifest(state, next, CommitOpts{Outcome: "updated"})
			if err != nil {
				t.Fatal(err)
			}
			if prevString(first.Previous) != "0.3.33 / "+img33 || prevString(second.Previous) != prevString(first.Previous) {
				t.Errorf("previous %s after the first commit, %s after the same commit again — want 0.3.33 both times",
					prevString(first.Previous), prevString(second.Previous))
			}
		})
	}
}

// ⛔ AN UNKNOWN EXECUTOR IS NO EVIDENCE OF A MOVE — ALSO WITH NO MARKER. A commit of an executor the re-hash recorded
// unknown keeps `previous`; with no marker standing the next branch took the record's version as "where it came from"
// and recorded the version the instance is on as its own previous (planted fault fixround3 M2 stayed green).
func TestManifest_AC_D53_AnUnknownCommitKeepsPreviousWithNoMarker(t *testing.T) {
	state := t.TempDir()
	if err := WriteManifest(state, Manifest{InstanceID: "i1", Tier: "compose", Version: "0.3.32", Executor: img32,
		Previous: &Previous{Version: "0.3.31", Image: img31}}); err != nil {
		t.Fatal(err)
	}
	got, err := CommitManifest(state, Manifest{InstanceID: "i1", Tier: "compose"},
		CommitOpts{Outcome: "rollback-unreachable", FailedStep: "A-3"})
	if err != nil {
		t.Fatal(err)
	}
	if p := prevString(got.Previous); p != "0.3.31 / "+img31 {
		t.Errorf("an unknown executor committed with no marker recorded previous %s, want 0.3.31 (unchanged)", p)
	}
}

// ⛔ A RECORD THAT READS "unknown" IS NOT WHERE THE INSTANCE CAME FROM. With no marker, a known commit over a record
// the re-hash wrote as the literal "unknown" recorded `previous` "unknown" — a version nothing can order or roll back
// to. Nothing moved that anyone saw: `previous` stays.
func TestManifest_AC_D53_AnUnknownRecordNeverBecomesPrevious(t *testing.T) {
	state := t.TempDir()
	if err := WriteManifest(state, Manifest{InstanceID: "i1", Tier: "compose", Version: "unknown",
		Previous: &Previous{Version: "0.3.31", Image: img31}}); err != nil {
		t.Fatal(err)
	}
	got, err := CommitManifest(state, Manifest{InstanceID: "i1", Tier: "compose", Version: "0.3.33", Executor: img33},
		CommitOpts{Outcome: "rolled-back", FailedStep: "A-1"})
	if err != nil {
		t.Fatal(err)
	}
	if p := prevString(got.Previous); p != "0.3.31 / "+img31 {
		t.Errorf("previous %s after a record that read unknown, want 0.3.31 (unchanged)", p)
	}
}

// ⛔ RT-2 / K-5: WHILE THE MARKER STANDS, THE PLAN'S GATES STILL HOLD. The view reads the installed version unknown,
// and an unknown installed version is — by design — no evidence of being ahead of the target, so the downgrade gate
// and the rollback-image guard nested in it were skipped. The executor is on one of the versions the marker and the
// record name: a target below the HIGHEST of them could be a downgrade.
func TestPlan_AC_D53_TheGatesHoldWhileTheMarkerStands(t *testing.T) {
	view := func(t *testing.T, before Manifest, marker string) Manifest {
		t.Helper()
		state := t.TempDir()
		if err := WriteManifest(state, before); err != nil {
			t.Fatal(err)
		}
		writeMarker(t, state, "i1", marker)
		m, err := ReadManifest(state, "i1")
		if err != nil {
			t.Fatal(err)
		}
		if m.Version != "" {
			t.Fatalf("precondition: the view reads version %q with the marker standing", m.Version)
		}
		return m
	}
	fwd := Manifest{InstanceID: "i1", Tier: "compose", Version: "0.3.46", Executor: img33,
		Previous: &Previous{Version: "0.3.45", Image: img32}}
	fwdMarker := markerBody(img34, "0.3.47", "", "0.3.46", img33)

	refused := func(t *testing.T, in PlanInput, why string, words ...string) {
		t.Helper()
		_, err := BuildPlan(in)
		if err == nil {
			t.Fatalf("%s: planned", why)
		}
		for _, w := range append([]string{"DOWN"}, words...) {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("%s: the refusal does not say %q: %v", why, w, err)
			}
		}
	}
	t.Run("an undeclared downgrade is refused, naming both versions the executor can be on", func(t *testing.T) {
		in := fixtureInput("compose")
		in.Installed = view(t, fwd, fwdMarker)
		in.Version, in.ImageDigest = "0.3.45", img32
		refused(t, in, "0.3.46/0.3.47 down to 0.3.45", "0.3.46", "0.3.47")
	})
	// F1: each of the two versions must count — the HIGHER one, whichever it is
	t.Run("the move's target counts: a target equal to the record's version is still below it", func(t *testing.T) {
		in := fixtureInput("compose")
		in.Installed = view(t, fwd, fwdMarker)
		in.Version, in.ImageDigest = "0.3.46", img33
		refused(t, in, "0.3.46 while the move may have landed 0.3.47")
	})
	t.Run("the version before the move counts: after an unconfirmed rollback, the version rolled away from", func(t *testing.T) {
		in := fixtureInput("compose")
		in.Installed = view(t, fwd, markerBody(img32, "0.3.45", "0.3.45", "0.3.46", img33))
		in.Version, in.ImageDigest = "0.3.45", img32
		// R8-3: the words say it was a rollback, and name both versions
		refused(t, in, "0.3.45 while the rollback may not have landed", "0.3.46", "0.3.45", "rollback to 0.3.45")
	})
	// D: the RECORD's version counts too — here it alone holds the gate. A marker with no origin at all (the plan
	// writes one only when neither a reading, a record nor an earlier marker named it — or the file was edited), under
	// a record on 0.3.46: the forward block for 0.3.45 is a downgrade, and only the record says so. The history in
	// which the record bounds the gate for real — a read origin below it — is TestPlan_AC_D53_TheRecordStillBoundsTheGate.
	t.Run("the record's version counts: it alone is the highest", func(t *testing.T) {
		in := fixtureInput("compose")
		in.Installed = view(t, fwd, markerBody(img32, "0.3.45", "0.3.45", "", ""))
		in.Version, in.ImageDigest = "0.3.45", img32
		refused(t, in, "0.3.45 below the record's 0.3.46, the marker naming only 0.3.45", "0.3.46")
	})
	t.Run("…also when the record already reads unknown: the marker still says where the move came from", func(t *testing.T) {
		in := fixtureInput("compose")
		in.Installed = view(t, Manifest{InstanceID: "i1", Tier: "compose", Previous: &Previous{Version: "0.3.45", Image: img32}},
			markerBody(img32, "0.3.45", "0.3.45", "0.3.46", img33))
		in.Version, in.ImageDigest = "0.3.45", img32
		refused(t, in, "0.3.45 with the record unknown and the marker from 0.3.46", "0.3.46")
	})
	t.Run("a rollback towards an image that is not the recorded previous is refused", func(t *testing.T) {
		in := fixtureInput("compose")
		in.Installed = view(t, fwd, fwdMarker)
		in.Version, in.RollbackTo, in.ImageDigest = "0.3.44", "0.3.44", img31
		if _, err := BuildPlan(in); err == nil || !strings.Contains(err.Error(), "recorded previous image") {
			t.Errorf("a rollback to %s while the recorded previous image is %s was planned: %v", img31, img32, err)
		}
	})
	t.Run("the declared rollback to the recorded previous is permitted", func(t *testing.T) {
		in := fixtureInput("compose")
		in.Installed = view(t, fwd, fwdMarker)
		in.Version, in.RollbackTo, in.ImageDigest = "0.3.45", "0.3.45", img32
		if _, err := BuildPlan(in); err != nil {
			t.Errorf("the page's own rollback block was refused while the marker stands: %v", err)
		}
	})
	t.Run("a forward update is permitted", func(t *testing.T) {
		in := fixtureInput("compose")
		in.Installed = view(t, fwd, fwdMarker)
		in.Version, in.ImageDigest = "0.3.48", img34
		if _, err := BuildPlan(in); err != nil {
			t.Errorf("a forward update was refused while the marker stands: %v", err)
		}
	})
	// ⛔ K-5, WITHOUT ANY MARKER: a record that already reads unknown (the re-hash recorded it so) still has a
	// previous image, and a rollback block that names another one belongs to another instance or another update
	t.Run("the rollback-image guard does not depend on the installed version", func(t *testing.T) {
		in := fixtureInput("compose")
		in.Installed = Manifest{InstanceID: "i1", Tier: "compose", Previous: &Previous{Version: "0.3.45", Image: img32}}
		in.Version, in.RollbackTo, in.ImageDigest = "0.3.45", "0.3.45", img31
		if _, err := BuildPlan(in); err == nil || !strings.Contains(err.Error(), "recorded previous image") {
			t.Errorf("a rollback towards an image that is not the recorded previous was planned for an instance whose version reads unknown: %v", err)
		}
	})
}
