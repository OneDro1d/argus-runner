package argus

// runid_test.go — R7 (M3.1 ADR-10, CP-M3-116), REVISED 2026-07-22 (owner remark: "Run Id is too
// long ... we used another run ID – much shorter").
//
// History. The original NewRunID was a bare-second timestamp (YYYYMMDDThhmmss, 15 chars). Two runs
// in the same second for one instance silently MERGED in the cloud ledger via
// ON CONFLICT (instance_id, run_id), so R7 appended 6 random hex — correct, but it made every id 21
// chars of half-noise and it rides inside every correlation id
// (tr-20260722T120153514960-CRED-004-59a479b1), the Grafana Run-ID variable and the Runs page.
//
// The contract now: MILLISECOND precision instead of the random suffix — YYYYMMDDThhmmssSSS, 18
// chars, entirely readable as a timestamp. Uniqueness is preserved BY CONSTRUCTION rather than by
// chance: the W1 fence (instance_run_lock, instance_id PK) serialises runs per instance, so two
// runs on one instance cannot overlap, and no run completes inside a single millisecond. That is a
// stronger guarantee than a 1-in-16.7M random draw, and it is deterministic.
//
// Unchanged: time-sortable (lexicographic == chronological), DASH-FREE and filename-/regex-safe —
// the run_id is the first '-'-delimited segment of tr-<run_id>-<scenario_id>-<8hex>, and scenario
// ids themselves contain dashes, so a dash inside run_id would break every correlation parse.

import (
	"regexp"
	"strings"
	"testing"
	"time"
)

var runIDShape = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}[0-9]{3}$`)

// Two runs on ONE instance can never overlap (the W1 fence serialises them) and no run finishes
// inside a millisecond, so millisecond precision is sufficient in practice. What must still hold is
// that the id CHANGES as the clock advances — a frozen or truncated stamp would resurrect the
// same-second merge.
func TestNewRunID_DistinctAcrossMilliseconds(t *testing.T) {
	seen := map[string]struct{}{}
	for i := 0; i < 5; i++ {
		id := NewRunID()
		if _, dup := seen[id]; dup {
			t.Fatalf("run_id %s repeated across a millisecond boundary — the stamp is not advancing", id)
		}
		seen[id] = struct{}{}
		time.Sleep(2 * time.Millisecond)
	}
}

// The owner-visible point of the change: the id is SHORTER than the 21-char R7 form and carries no
// random noise — it reads end-to-end as a timestamp.
func TestNewRunID_IsShorterAndFullyNumeric(t *testing.T) {
	id := NewRunID()
	if len(id) != 18 {
		t.Fatalf("run_id %q is %d chars; want 18 (YYYYMMDDThhmmssSSS) — the R7 form was 21", id, len(id))
	}
	if strings.ContainsAny(id, "abcdef") {
		t.Errorf("run_id %q still carries hex noise; it should read as a plain timestamp", id)
	}
}

// Lexicographic order MUST equal chronological order — the Runs page, the ledger and the Grafana
// Run-ID variable all sort these as strings.
func TestNewRunID_LexicographicallySortable(t *testing.T) {
	a := NewRunID()
	time.Sleep(2 * time.Millisecond)
	b := NewRunID()
	if !(a < b) {
		t.Fatalf("run ids must sort chronologically as strings: %q !< %q", a, b)
	}
}

func TestNewRunID_ShapeSortableDashFree(t *testing.T) {
	id := NewRunID()
	if !runIDShape.MatchString(id) {
		t.Fatalf("run_id %q does not match ^\\d{8}T\\d{6}[0-9a-f]{6}$ (timestamp prefix + 6-hex suffix)", id)
	}
	if strings.ContainsAny(id, "-:/ ") {
		t.Fatalf("run_id %q contains a separator — it must stay a clean correlation-id segment", id)
	}
}

func TestNewScenarioCorrelationID_RunIDStaysFirstSegment(t *testing.T) {
	runID := NewRunID()
	cid := newScenarioCorrelationID(runID, "DB-004")
	want := "tr-" + runID + "-DB-004-"
	if !strings.HasPrefix(cid, want) {
		t.Fatalf("correlation id %q; want prefix %q (run_id must survive as the first segment)", cid, want)
	}
}

// The millisecond stamp alone is only unique because the W1 fence (instance_run_lock) serialises
// runs per instance — a guarantee that lives in ANOTHER component. Relying on it means a future
// caller that mints two ids in one millisecond silently resurrects the same-second merge bug that
// R7 was created to fix. Make the guarantee LOCAL and unconditional: ids are strictly increasing
// within a process, whatever the clock does. Cheap, deterministic, and it keeps the id 18 chars.
func TestNewRunID_StrictlyIncreasingWithinAMillisecond(t *testing.T) {
	const n = 500 // far more than one millisecond's worth
	prev := ""
	seen := make(map[string]struct{}, n)
	for i := 0; i < n; i++ {
		id := NewRunID()
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate run_id %q at mint %d — the same-instant merge is back", id, i)
		}
		if prev != "" && !(prev < id) {
			t.Fatalf("run ids must strictly increase: %q !< %q", prev, id)
		}
		if len(id) != 18 {
			t.Fatalf("the monotonic guard must not change the length: %q is %d chars", id, len(id))
		}
		seen[id] = struct{}{}
		prev = id
	}
}
