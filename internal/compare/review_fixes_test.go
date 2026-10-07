package compare_test

import (
	"math/rand"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/compare"
)

// Review findings F1-F6 on PR #495 (ARGUS-CMP-2). One named test per finding.

func rfPath(t testing.TB, s string) compare.Path {
	t.Helper()
	p, err := compare.ParsePath(s)
	if err != nil {
		t.Fatalf("ParsePath(%q): %v", s, err)
	}
	return p
}

func rfRec(t testing.TB, body string, spec compare.Spec) compare.OutputRecord {
	t.Helper()
	spec.Body = true
	r, _ := compare.BuildRecord(compare.Response{Status: 200, Body: []byte(body)}, spec, "", 1)
	return r
}

func rfKV(k, v string) compare.RawKV { return compare.RawKV{Key: k, Value: v, Line: 7} }

func rfProblems(kvs ...compare.RawKV) string {
	_, probs := compare.BuildRules(kvs)
	var b strings.Builder
	for _, p := range probs {
		b.WriteString(p.Key + ": " + p.Msg + "\n")
	}
	return b.String()
}

// F1: invalid UTF-8 and lone surrogate escapes are TEXT, never collapsed onto U+FFFD.
func TestReviewF1_InvalidUTF8IsTextNotOneHash(t *testing.T) {
	var spec compare.Spec
	ff, fe := rfRec(t, "\"\xff\"", spec), rfRec(t, "\"\xfe\"", spec)
	if ff.Hash == fe.Hash {
		t.Errorf("\\xff and \\xfe hash equal")
	}
	real := rfRec(t, "\"x�y\"", spec)
	if inv := rfRec(t, "\"x\xffy\"", spec); inv.Hash == real.Hash {
		t.Errorf("an invalid byte equals a real U+FFFD")
	}
	if ls := rfRec(t, `"\ud800"`, spec); ls.Hash == rfRec(t, "\"�\"", spec).Hash || ls.BodyKind != compare.KindText {
		t.Errorf("lone surrogate escape: kind=%s equals U+FFFD=%v", ls.BodyKind, ls.Hash == rfRec(t, "\"�\"", spec).Hash)
	}
	if a, b := rfRec(t, `"\ud800"`, spec), rfRec(t, `"\ud801"`, spec); a.Hash == b.Hash {
		t.Errorf("two lone surrogates hash equal")
	}
	if ff.BodyKind != compare.KindText {
		t.Errorf("invalid UTF-8 body kind = %s, want text", ff.BodyKind)
	}
	// a surrogate PAIR still equals the literal character
	if rfRec(t, `"😀"`, spec).Hash != rfRec(t, "\"\U0001F600\"", spec).Hash {
		t.Errorf("a valid escaped pair no longer equals the literal character")
	}
	// a path rule on such a body is not_recorded
	mask := compare.Spec{Masks: []compare.Path{rfPath(t, "$.a")}}
	r := rfRec(t, "{\"a\":\"\xff\"}", mask)
	if r.State != compare.StateNotRecorded || r.Reason != compare.ReasonMaskNeedsJSON {
		t.Errorf("mask on invalid UTF-8: state=%s reason=%s", r.State, r.Reason)
	}
	r = rfRec(t, `{"a":"\ud800"}`, mask)
	if r.State != compare.StateNotRecorded {
		t.Errorf("mask on lone surrogate: state=%s", r.State)
	}
}

// F2: an agreement outside (0,100] is 100 everywhere it is judged.
func TestReviewF2_ZeroOrNegativeAgreementIsOneHundred(t *testing.T) {
	for _, pct := range []float64{0, -5, 101, 1e9} {
		j := compare.JudgeClaim([]string{"failed", "failed", "failed"}, pct)
		if j.State == compare.StateIdentical || j.ClaimSupported {
			t.Errorf("JudgeClaim(3 failed, %v): state=%s supported=%v", pct, j.State, j.ClaimSupported)
		}
		if compare.ClaimSupported(2, 5, pct) {
			t.Errorf("ClaimSupported(2,5,%v) is true", pct)
		}
		if compare.MinRuns(pct) != 1 {
			t.Errorf("MinRuns(%v) = %d", pct, compare.MinRuns(pct))
		}
	}
	for _, ref := range []compare.Reference{compare.RefFixed, compare.RefProperty} {
		zero := &compare.Rules{Reference: ref}
		o := compare.Result(rfIn(zero, rfRun("c1", "cand", "failed"), rfRun("c2", "cand", "failed")))
		if c := rfCell(o, "cand"); c.State == compare.StateIdentical {
			t.Errorf("%s with Agreement 0: two failed runs are identical", ref)
		}
		neg := &compare.Rules{Reference: ref, Agreement: -3}
		o = compare.Result(rfIn(neg, rfRun("c1", "cand", "failed")))
		if c := rfCell(o, "cand"); c.State == compare.StateIdentical {
			t.Errorf("%s with Agreement -3: a failed run is identical", ref)
		}
	}
	// measured: two runs that disagree with the reference
	zero := &compare.Rules{Reference: compare.RefMeasured, Repeats: 2,
		Output: compare.OutputSel{Status: true, Body: true}}
	o := compare.Result(rfIn(zero,
		rfRunOut("r1", "ref", "A"), rfRunOut("r2", "ref", "A"),
		rfRunOut("c1", "cand", "B"), rfRunOut("c2", "cand", "B")))
	if c := rfCell(o, "cand"); c.State != compare.StateDiffers {
		t.Errorf("measured with Agreement 0: state=%s", c.State)
	}
}

// F3: Tolerance at or under an Unordered path is refused by name.
func TestReviewF3_ToleranceUnderUnorderedIsRefused(t *testing.T) {
	cases := []struct{ un, tol string }{
		{"$.items", "$.items[*].p abs 0.1"},
		{"$.items", "$.items abs 1"},
		{"$.items", "$.items[0].p abs 1"},
		{"$", "$.a abs 1"},
		{"$.a", "$.a.b.c abs 1"},
		{"$.a[*].b", "$.a[*].b[*] abs 1"},
	}
	for _, c := range cases {
		got := rfProblems(rfKV("Reference", "measured"), rfKV("Unordered", c.un), rfKV("Tolerance", c.tol))
		if !strings.Contains(got, "Tolerance") || !strings.Contains(got, "Unordered") || !strings.Contains(got, strings.Fields(c.tol)[0]) {
			t.Errorf("Unordered %q + Tolerance %q not refused by name: %q", c.un, c.tol, got)
		}
	}
	// the Problem carries the Tolerance line
	_, probs := compare.BuildRules([]compare.RawKV{rfKV("Reference", "measured"), rfKV("Unordered", "$.items"), rfKV("Tolerance", "$.items[*].p abs 0.1")})
	if len(probs) != 1 || probs[0].Line != 7 {
		t.Errorf("want one problem on line 7, got %+v", probs)
	}
	// unrelated paths stay legal, including a sibling and a parent of the unordered path
	for _, c := range []struct{ un, tol string }{
		{"$.items", "$.price abs 1"},
		{"$.items[*].tags", "$.items[*].p abs 1"},
		{"$.a.b", "$.a abs 1"},
	} {
		if got := rfProblems(rfKV("Reference", "measured"), rfKV("Unordered", c.un), rfKV("Tolerance", c.tol)); got != "" {
			t.Errorf("Unordered %q + Tolerance %q refused: %q", c.un, c.tol, got)
		}
	}
}

// F4: strict number spellings.
func TestReviewF4_StrictNumberSpellings(t *testing.T) {
	m := rfKV("Reference", "measured")
	for _, v := range []string{"1e2%", "0x1p6%", "+50%", "1_0%", "50 %", "5.%", ".5%", "1.1234567%", "NaN%", "Inf%", "-5%", "0%", "101%", "100.5%", "50", "%", "٣%"} {
		got := rfProblems(m, rfKV("Agreement", v))
		if !strings.Contains(got, "Agreement") || !strings.Contains(got, v) {
			t.Errorf("Agreement %q not refused naming key and value: %q", v, got)
		}
	}
	if got := rfProblems(m, rfKV("Agreement", "")); !strings.Contains(got, "Agreement") {
		t.Errorf("empty Agreement accepted: %q", got)
	}
	for _, v := range []string{"100%", "99.5%", "1%", "50%", "7%", "12.123456%"} {
		if got := rfProblems(m, rfKV("Agreement", v)); got != "" {
			t.Errorf("Agreement %q refused: %q", v, got)
		}
	}
	for _, v := range []string{"05", "007", "00", "0", "21", "+3", "1e1", " 5x", "1.5", "٣", "-1", "abc", ""} {
		got := rfProblems(m, rfKV("Repeats", v))
		if !strings.Contains(got, "Repeats") {
			t.Errorf("Repeats %q accepted: %q", v, got)
		}
	}
	for _, v := range []string{"1", "9", "10", "20"} {
		if got := rfProblems(m, rfKV("Repeats", v)); got != "" {
			t.Errorf("Repeats %q refused: %q", v, got)
		}
	}
	for _, v := range []string{"1e0", "+1", "1_0", "0x10", "-1", ".5", "5.", "1e-1", "NaN", "Inf", "0", "00.5", "01"} {
		got := rfProblems(m, rfKV("Tolerance", "$.a abs "+v))
		if !strings.Contains(got, "Tolerance") {
			t.Errorf("Tolerance abs %q accepted: %q", v, got)
		}
	}
	for _, v := range []string{"1", "0.5", "10", "0.001", "12.5"} {
		if got := rfProblems(m, rfKV("Tolerance", "$.a rel "+v)); got != "" {
			t.Errorf("Tolerance rel %q refused: %q", v, got)
		}
	}
	for _, v := range []string{"p95 1e1%", "p95 +5%", "p95 0x5%", "p95 1_0%", "p95 05%", "error_rate 1e0pp", "error_rate +1pp", "error_rate .5pp"} {
		got := rfProblems(m, rfKV("Not Worse Than", v))
		if !strings.Contains(got, "Not Worse Than") {
			t.Errorf("Not Worse Than %q accepted: %q", v, got)
		}
	}
	for _, v := range []string{"p95 20%", "p50 5.5%", "error_rate 0.5pp", "p99 100%"} {
		if got := rfProblems(m, rfKV("Not Worse Than", v)); got != "" {
			t.Errorf("Not Worse Than %q refused: %q", v, got)
		}
	}
}

// F5: the result hash does not depend on the order of checks, runs, members or output rows.
func TestReviewF5_ResultIsOrderIndependent(t *testing.T) {
	rs := &compare.Rules{Reference: compare.RefMeasured, Repeats: 1, Agreement: 100, Output: compare.OutputSel{Status: true, Body: true}}
	build := func() compare.Input {
		mk := func(id, mem, h1, h2, h3 string) compare.Run {
			r := compare.Run{ID: id, Member: mem, VersionKey: "v1", Status: "completed", SetHash: "S",
				Outcomes: map[string]string{"C1": "passed", "C2": "passed", "C3": "passed"}}
			r.Outputs = []compare.ScenarioOutput{rfRow("C1", h1), rfRow("C2", h2), rfRow("C3", h3)}
			return r
		}
		return compare.Input{SetHash: "S",
			Checks:  []compare.Check{{ID: "C1", Path: "a.md", Rules: rs}, {ID: "C2", Path: "b.md", Rules: rs}, {ID: "C3", Path: "c.md", Rules: rs}},
			Members: []compare.Member{{Name: "ref", Role: compare.RoleReference}, {Name: "cand", Role: compare.RoleCandidate}, {Name: "cand2", Role: compare.RoleCandidate}},
			Runs: []compare.Run{mk("r1", "ref", "A", "A", "A"), mk("r2", "ref", "A", "A", "A"), mk("c1", "cand", "A", "B", "A"),
				mk("c2", "cand", "A", "B", "A"), mk("d1", "cand2", "A", "A", "B")}}
	}
	base := compare.Result(build())
	wantHash, wantJSON := base.Hash(), string(base.JSON())
	rng := rand.New(rand.NewSource(7))
	for i := 0; i < 60; i++ {
		in := build()
		rng.Shuffle(len(in.Checks), func(a, b int) { in.Checks[a], in.Checks[b] = in.Checks[b], in.Checks[a] })
		rng.Shuffle(len(in.Runs), func(a, b int) { in.Runs[a], in.Runs[b] = in.Runs[b], in.Runs[a] })
		rng.Shuffle(len(in.Members), func(a, b int) { in.Members[a], in.Members[b] = in.Members[b], in.Members[a] })
		for k := range in.Runs {
			o := in.Runs[k].Outputs
			rng.Shuffle(len(o), func(a, b int) { o[a], o[b] = o[b], o[a] })
		}
		got := compare.Result(in)
		if got.Hash() != wantHash || string(got.JSON()) != wantJSON {
			t.Fatalf("permutation %d changed the hash: %s want %s", i, got.Hash(), wantHash)
		}
	}
}

// F6: documented behaviours, pinned.
func TestReviewF6_UnicodeIsNotNormalisedButEscapesAre(t *testing.T) {
	var spec compare.Spec
	nfc, nfd := "\"é\"", "\"é\""
	if rfRec(t, nfc, spec).Hash == rfRec(t, nfd, spec).Hash {
		t.Errorf("NFC and NFD spellings hash equal: Unicode is NOT normalised")
	}
	if rfRec(t, `"é"`, spec).Hash != rfRec(t, nfc, spec).Hash {
		t.Errorf("a literal character must equal its \\u escape")
	}
}

func TestReviewF6_FractionOrExponentIsFloat64IntegerBeyond2e53IsExact(t *testing.T) {
	var spec compare.Spec
	if rfRec(t, `9007199254740993.0`, spec).Hash != rfRec(t, `9007199254740992`, spec).Hash {
		t.Errorf("a number with a fraction is compared as float64")
	}
	if rfRec(t, `9007199254740993`, spec).Hash == rfRec(t, `9007199254740992`, spec).Hash {
		t.Errorf("an integer literal beyond 2^53 is kept as written")
	}
}

func rfRow(check, h string) compare.ScenarioOutput {
	return compare.ScenarioOutput{ScenarioID: check, OutputRecord: compare.OutputRecord{
		V: 1, State: compare.StateRecorded, Status: 200, Hash: h, Parts: compare.Parts{Body: h}, BodyKind: "json"}}
}

func rfRun(id, member, outcome string) compare.Run {
	return compare.Run{ID: id, Member: member, VersionKey: "v1", Status: "completed", SetHash: "S",
		Outcomes: map[string]string{"C1": outcome}}
}

func rfRunOut(id, member, h string) compare.Run {
	r := rfRun(id, member, "passed")
	r.Outputs = []compare.ScenarioOutput{rfRow("C1", h)}
	return r
}

func rfIn(rs *compare.Rules, runs ...compare.Run) compare.Input {
	return compare.Input{SetHash: "S",
		Checks:  []compare.Check{{ID: "C1", Path: "c1.md", Rules: rs}},
		Members: []compare.Member{{Name: "ref", Role: compare.RoleReference}, {Name: "cand", Role: compare.RoleCandidate}},
		Runs:    runs}
}

func rfCell(o compare.Output, member string) compare.Cell {
	for _, c := range o.Checks[0].Cells {
		if c.Member == member {
			return c
		}
	}
	return compare.Cell{}
}
