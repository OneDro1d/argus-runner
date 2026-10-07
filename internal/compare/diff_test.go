package compare

// diff_test.go -- ARGUS-CMP-9: compare.Diff, the structured difference of two canonical outputs.
//
// The rule that makes it trustworthy: Diff reports a difference EXACTLY when the two outputs' hashes differ on that
// part. TestDiff_ReportsADifferenceExactlyWhenTheHashesDiffer proves both directions on every pair of the golden
// vectors; TestDiff_GeneratedCasesAgreeWithTheHashes proves them on generated bodies.

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"
)

func canonOf(t *testing.T, rulesKV []string, status int, headers map[string][]string, body string) (*Canonical, OutputRecord) {
	t.Helper()
	b := buildOutput(t, mustRules(t, append([]string{"Reference", "measured"}, rulesKV...)...), status, headers, []byte(body))
	return mustVerified(t, b), b.rec
}

func partsOf(a, b OutputRecord) []string {
	var out []string
	if a.Parts.Status != b.Parts.Status {
		out = append(out, "status")
	}
	if a.Parts.Headers != b.Parts.Headers {
		out = append(out, "headers")
	}
	if a.Parts.Body != b.Parts.Body {
		out = append(out, "body")
	}
	return out
}

func TestDiff_OfAnOutputWithItselfIsEmptyForEveryGoldenVector(t *testing.T) {
	n := 0
	for _, v := range loadVectors(t) {
		b := vectorBuilt(t, v)
		c1, ver := b.read(t)
		if ver.State == VerifyMalformed || ver.State == VerifyMismatch {
			t.Fatalf("%s: %+v", v.Name, ver)
		}
		c2, _ := b.read(t)
		d := Diff(c1, c2)
		if !d.Equal || len(d.Entries) != 0 || d.Total != 0 || d.Truncated || len(d.Parts) != 0 {
			t.Errorf("%s: Diff(a, a) = %+v, want empty", v.Name, d)
		}
		if d.Body.State == "differs" {
			t.Errorf("%s: Diff(a, a) body state = %q", v.Name, d.Body.State)
		}
		n++
	}
	if n < 20 {
		t.Fatalf("only %d vectors", n)
	}
}

func TestDiff_ReportsADifferenceExactlyWhenTheHashesDiffer(t *testing.T) {
	vs := loadVectors(t)
	var all []built
	var canons []*Canonical
	for _, v := range vs {
		b := vectorBuilt(t, v)
		c, ver := b.read(t)
		if ver.State != VerifyVerified {
			continue // a number of 1e21 or more is not recomputable; Diff never sees such a body as whole
		}
		all = append(all, b)
		canons = append(canons, c)
	}
	if len(all) < 20 {
		t.Fatalf("only %d verified vectors", len(all))
	}
	same, differ := 0, 0
	for i := range all {
		for j := range all {
			d := Diff(canons[i], canons[j])
			want := partsOf(all[i].rec, all[j].rec)
			if got := d.Parts; strings.Join(got, ",") != strings.Join(want, ",") {
				t.Fatalf("vectors %d (%s) and %d (%s): Diff says parts %v, the hashes say %v", i, vs[i].Name, j, vs[j].Name, got, want)
			}
			if wantEqual := all[i].rec.Hash == all[j].rec.Hash; d.Equal != wantEqual {
				t.Fatalf("vectors %d and %d: Equal = %v, hashes equal = %v", i, j, d.Equal, wantEqual)
			}
			if d.Equal {
				same++
			} else {
				differ++
				// a part that differs says something: an entry, or (a text body) a body state
				if len(d.Entries) == 0 && d.Body.State != "differs" && d.Body.State != "not_comparable" {
					t.Fatalf("vectors %d and %d differ but Diff names nothing: %+v", i, j, d)
				}
			}
		}
	}
	if same < len(all) || differ < 100 {
		t.Fatalf("pairs equal %d, differing %d: the matrix did not exercise both directions", same, differ)
	}
}

// ── generated cases ────────────────────────────────────────────────────────────────────────────────

func genJSON(r *rand.Rand, depth int) any {
	if depth <= 0 || r.Intn(4) == 0 {
		switch r.Intn(6) {
		case 0:
			return nil
		case 1:
			return r.Intn(2) == 0
		case 2:
			return r.Intn(5)
		case 3:
			return float64(r.Intn(100)) / 4
		case 4:
			return fmt.Sprintf("s%d", r.Intn(4))
		default:
			return "<masked>"
		}
	}
	if r.Intn(2) == 0 {
		n := r.Intn(4)
		arr := make([]any, n)
		for i := range arr {
			arr[i] = genJSON(r, depth-1)
		}
		return arr
	}
	obj := map[string]any{}
	for i, n := 0, r.Intn(4); i < n; i++ {
		obj[string(rune('a'+r.Intn(4)))] = genJSON(r, depth-1)
	}
	return obj
}

func mutateJSON(r *rand.Rand, v any) any {
	switch x := v.(type) {
	case []any:
		out := append([]any(nil), x...)
		switch {
		case len(out) > 0 && r.Intn(3) == 0:
			i, j := r.Intn(len(out)), r.Intn(len(out))
			out[i], out[j] = out[j], out[i]
		case len(out) > 0 && r.Intn(3) == 0:
			out = out[:len(out)-1]
		case r.Intn(3) == 0:
			out = append(out, genJSON(r, 1))
		default:
			for i := range out {
				if r.Intn(2) == 0 {
					out[i] = mutateJSON(r, out[i])
				}
			}
		}
		return out
	case map[string]any:
		out := map[string]any{}
		for k, e := range x {
			out[k] = e
		}
		switch r.Intn(4) {
		case 0:
			out[string(rune('a'+r.Intn(4)))] = genJSON(r, 1)
		case 1:
			for k := range out {
				delete(out, k)
				break
			}
		default:
			for k, e := range out {
				if r.Intn(2) == 0 {
					out[k] = mutateJSON(r, e)
				}
			}
		}
		return out
	}
	return genJSON(r, 1)
}

func TestDiff_GeneratedCasesAgreeWithTheHashes(t *testing.T) {
	r := rand.New(rand.NewSource(20261004))
	ruleSets := [][]string{
		{},
		{"Mask", "$.a"},
		{"Mask", "$.a; $.b[*]"},
		{"Unordered", "$"},
		{"Unordered", "$.a; $.b", "Mask", "$.c"},
		{"Unordered", "$.a[*]; $", "Mask", "$[0]; $.d"},
	}
	equal, differ := 0, 0
	for i := 0; i < 600; i++ {
		kv := ruleSets[r.Intn(len(ruleSets))]
		ja := genJSON(r, 3)
		jb := ja
		if r.Intn(5) != 0 {
			jb = mutateJSON(r, ja)
		}
		ba, _ := json.Marshal(ja)
		bb, _ := json.Marshal(jb)
		rules := mustRules(t, append([]string{"Reference", "measured"}, kv...)...)
		var ca, cb *Canonical
		var ra, rb OutputRecord
		var ver Verification
		a := buildMaybe(t, rules, ba)
		b := buildMaybe(t, rules, bb)
		if a == nil || b == nil {
			continue // a mask on a body that is not JSON is not recorded: nothing to compare
		}
		if ca, ver = a.read(t); ver.State != VerifyVerified {
			t.Fatalf("case %d: %+v for %s", i, ver, a.file)
		}
		if cb, ver = b.read(t); ver.State != VerifyVerified {
			t.Fatalf("case %d: %+v for %s", i, ver, b.file)
		}
		ra, rb = a.rec, b.rec
		d := Diff(ca, cb)
		wantEqual := ra.Hash == rb.Hash
		if d.Equal != wantEqual {
			t.Fatalf("case %d rules %v\n a %s\n b %s\n Diff.Equal = %v but the hashes say equal = %v (entries %+v)", i, kv, ba, bb, d.Equal, wantEqual, d.Entries)
		}
		if got, want := d.Parts, partsOf(ra, rb); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("case %d: parts %v, want %v", i, got, want)
		}
		if !wantEqual && len(d.Entries) == 0 && d.Body.State != "differs" {
			t.Fatalf("case %d differs by hash but Diff lists nothing: %+v", i, d)
		}
		// symmetry of the verdict (not of the wording)
		if back := Diff(cb, ca); back.Equal != d.Equal || back.Total != d.Total {
			t.Fatalf("case %d: Diff(b, a) = equal %v total %d; Diff(a, b) = equal %v total %d", i, back.Equal, back.Total, d.Equal, d.Total)
		}
		if wantEqual {
			equal++
		} else {
			differ++
		}
	}
	if equal < 60 || differ < 150 {
		t.Fatalf("generated %d equal and %d differing pairs: the generator must exercise both directions", equal, differ)
	}
}

func buildMaybe(t *testing.T, rules *Rules, body []byte) *built {
	t.Helper()
	r, st := BuildRecord(Response{Status: 200, Body: body}, rules.Spec(), "", 1)
	if r.State != StateRecorded || st == nil {
		return nil
	}
	return &built{rec: r, file: st.File(), rules: rules}
}

// ── what a Diff says ───────────────────────────────────────────────────────────────────────────────

func entryAt(d DiffResult, path string) *DiffEntry {
	for i := range d.Entries {
		if d.Entries[i].Path == path {
			return &d.Entries[i]
		}
	}
	return nil
}

func str(s string) *string { return &s }

func TestDiff_NamesTheChangedPathsFromTheTwoCanonicalTrees(t *testing.T) {
	a, _ := canonOf(t, nil, 200, nil, `{"a":1,"b":{"c":"x","d":[1,2,3]},"e":true,"gone":1}`)
	b, _ := canonOf(t, nil, 200, nil, `{"a":2,"b":{"c":"x","d":[1,9,3,4]},"e":true,"new":{"k":1}}`)
	d := Diff(a, b)
	if d.Equal || len(d.Parts) != 1 || d.Parts[0] != "body" {
		t.Fatalf("diff = %+v", d)
	}
	want := map[string]DiffEntry{
		"$.a":      {Part: "body", Path: "$.a", Change: "changed", A: str("1"), B: str("2")},
		"$.b.d[1]": {Part: "body", Path: "$.b.d[1]", Change: "changed", A: str("2"), B: str("9")},
		"$.b.d[3]": {Part: "body", Path: "$.b.d[3]", Change: "only_b", B: str("4")},
		"$.gone":   {Part: "body", Path: "$.gone", Change: "only_a", A: str("1")},
		"$.new":    {Part: "body", Path: "$.new", Change: "only_b", B: str(`{"k":1}`)},
	}
	if len(d.Entries) != len(want) || d.Total != len(want) {
		t.Fatalf("entries = %+v, want %d", d.Entries, len(want))
	}
	for p, w := range want {
		got := entryAt(d, p)
		if got == nil {
			t.Errorf("no entry at %s", p)
			continue
		}
		if got.Part != w.Part || got.Change != w.Change || fmt.Sprint(deref(got.A)) != fmt.Sprint(deref(w.A)) || fmt.Sprint(deref(got.B)) != fmt.Sprint(deref(w.B)) || got.Masked {
			t.Errorf("entry at %s = %+v (a %s, b %s), want %+v (a %s, b %s)", p, *got, deref(got.A), deref(got.B), w, deref(w.A), deref(w.B))
		}
	}
	if d.Body.AKind != KindJSON || d.Body.BKind != KindJSON || d.Body.State != "differs" {
		t.Errorf("body = %+v", d.Body)
	}
}

func deref(s *string) string {
	if s == nil {
		return "<none>"
	}
	return *s
}

func TestDiff_NullAndAbsentStayDifferent(t *testing.T) {
	withNull, _ := canonOf(t, nil, 200, nil, `{"a":null}`)
	absent, _ := canonOf(t, nil, 200, nil, `{}`)
	d := Diff(withNull, absent)
	e := entryAt(d, "$.a")
	if d.Equal || e == nil || e.Change != "only_a" || deref(e.A) != "null" || e.B != nil {
		t.Fatalf("null against absent = %+v", d)
	}
	zero, _ := canonOf(t, nil, 200, nil, `{"a":0}`)
	if d := Diff(withNull, zero); d.Equal || entryAt(d, "$.a") == nil || entryAt(d, "$.a").Change != "changed" {
		t.Errorf("null against 0 = %+v", d)
	}
}

func TestDiff_AMaskedPathIsReportedAsMaskedAndItsValuesAreNeverShown(t *testing.T) {
	// the same rules on both sides: nothing to report, nothing shown
	m1, _ := canonOf(t, []string{"Mask", "$.id"}, 200, nil, `{"id":"SECRET-ONE","v":1}`)
	m2, _ := canonOf(t, []string{"Mask", "$.id"}, 200, nil, `{"id":"SECRET-TWO","v":1}`)
	if d := Diff(m1, m2); !d.Equal || len(d.Entries) != 0 {
		t.Errorf("two masked values differ in the report: %+v", d)
	}
	if got := m1.MaskedPaths(); len(got) != 1 || got[0] != "$.id" {
		t.Errorf("masked paths = %v", got)
	}
	// rules that disagree (a run of another set, a stale record): a masked leaf on one side, a real value on the other.
	// It is a difference (the hashes differ) and it is reported as MASKED: neither value is carried.
	real, _ := canonOf(t, nil, 200, nil, `{"id":"SECRET-REAL","v":1}`)
	d := Diff(m1, real)
	e := entryAt(d, "$.id")
	if d.Equal || e == nil || !e.Masked || e.A != nil || e.B != nil || e.Change != "changed" {
		t.Fatalf("masked against real = %+v", d)
	}
	raw, _ := json.Marshal(d)
	for _, leak := range []string{"SECRET-REAL", "SECRET-ONE", "SECRET-TWO"} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("the diff carries %q: %s", leak, raw)
		}
	}
	// a masked leaf under an array wildcard, and a subtree
	n1, _ := canonOf(t, []string{"Mask", "$.items[*].at; $.meta"}, 200, nil, `{"items":[{"at":"A1","v":1},{"at":"A2","v":2}],"meta":{"x":"M1"}}`)
	n2, _ := canonOf(t, []string{"Mask", "$.items[*].at; $.meta"}, 200, nil, `{"items":[{"at":"B1","v":1},{"at":"B2","v":3}],"meta":{"x":"M2"}}`)
	d = Diff(n1, n2)
	if d.Total != 1 || entryAt(d, "$.items[1].v") == nil {
		t.Errorf("only the unmasked difference may be listed: %+v", d)
	}
	raw, _ = json.Marshal(d)
	for _, leak := range []string{"A1", "B1", "M1", "M2"} {
		if strings.Contains(string(raw), `"`+leak+`"`) {
			t.Errorf("the diff carries a masked value %q: %s", leak, raw)
		}
	}
}

func TestDiff_AnUnorderedArrayIsComparedAsASetOfItsElements(t *testing.T) {
	rules := []string{"Unordered", "$.items"}
	a, _ := canonOf(t, rules, 200, nil, `{"items":[3,1,2,2]}`)
	b, _ := canonOf(t, rules, 200, nil, `{"items":[2,3,2,1]}`)
	if d := Diff(a, b); !d.Equal {
		t.Errorf("the same elements in another order = %+v", d)
	}
	// 2 appears twice on one side and once on the other: the set is a MULTISET, and what is extra is named, not an index
	c, _ := canonOf(t, rules, 200, nil, `{"items":[1,2,3,4]}`)
	d := Diff(a, c)
	if d.Equal || len(d.Entries) != 2 {
		t.Fatalf("unordered diff = %+v", d)
	}
	var gotA, gotB []string
	for _, e := range d.Entries {
		if e.Path != "$.items[*]" {
			t.Errorf("an unordered element is named by the array, not by an index: %+v", e)
		}
		switch e.Change {
		case "only_a":
			gotA = append(gotA, deref(e.A))
		case "only_b":
			gotB = append(gotB, deref(e.B))
		}
	}
	sort.Strings(gotA)
	sort.Strings(gotB)
	if strings.Join(gotA, ",") != "2" || strings.Join(gotB, ",") != "4" {
		t.Errorf("only in a %v, only in b %v; want [2] and [4]", gotA, gotB)
	}
	// an ORDERED array is positional
	oa, _ := canonOf(t, nil, 200, nil, `{"items":[3,1,2]}`)
	ob, _ := canonOf(t, nil, 200, nil, `{"items":[1,2,3]}`)
	if d := Diff(oa, ob); d.Equal || d.Total != 3 || entryAt(d, "$.items[0]") == nil {
		t.Errorf("an ordered array in another order = %+v", d)
	}
	// declared on one side only: positional, as the hashes are
	if d := Diff(a, oa); d.Equal {
		t.Errorf("an unordered array against an ordered one reads equal: %+v", d)
	}
}

func TestDiff_TextAndMixedBodiesSayWhichKindEachIsAndWhetherTheyAreEqual(t *testing.T) {
	t1, _ := canonOf(t, nil, 200, nil, "hello\r\nworld")
	t2, _ := canonOf(t, nil, 200, nil, "hello\nworld")
	t3, _ := canonOf(t, nil, 200, nil, "hello\nthere")
	j1, _ := canonOf(t, nil, 200, nil, `{"a":1}`)
	e1, _ := canonOf(t, nil, 200, nil, "")
	if d := Diff(t1, t2); !d.Equal || d.Body.State != "equal" || d.Body.AKind != KindText {
		t.Errorf("CRLF is not a difference: %+v", d)
	}
	d := Diff(t2, t3)
	if d.Equal || len(d.Entries) != 0 || d.Body.State != "differs" || d.Body.AKind != KindText || d.Body.BKind != KindText || d.Total != 0 {
		t.Errorf("two different texts: %+v (no invented path)", d)
	}
	d = Diff(t2, j1)
	if d.Equal || len(d.Entries) != 0 || d.Body.State != "differs" || d.Body.AKind != KindText || d.Body.BKind != KindJSON {
		t.Errorf("text against JSON: %+v", d)
	}
	d = Diff(e1, t2)
	if d.Equal || d.Body.AKind != KindEmpty || d.Body.BKind != KindText {
		t.Errorf("empty against text: %+v", d)
	}
	if d := Diff(e1, e1); !d.Equal || d.Body.State != "equal" {
		t.Errorf("empty against empty: %+v", d)
	}
}

func TestDiff_StatusAndDeclaredHeaders(t *testing.T) {
	kv := []string{"Output", "status, body, header:Content-Type, header:X-Trace", "Mask", "header:X-Trace"}
	h1 := map[string][]string{"Content-Type": {"application/json"}, "X-Trace": {"t-ONE"}}
	h2 := map[string][]string{"Content-Type": {"text/plain"}, "X-Trace": {"t-TWO"}}
	a, ra := canonOf(t, kv, 200, h1, `{"a":1}`)
	b, rb := canonOf(t, kv, 404, h2, `{"a":1}`)
	d := Diff(a, b)
	if got, want := d.Parts, partsOf(ra, rb); strings.Join(got, ",") != strings.Join(want, ",") || len(got) != 2 {
		t.Fatalf("parts = %v want %v", got, want)
	}
	if e := entryAt(d, "status"); e == nil || e.Part != "status" || deref(e.A) != "200" || deref(e.B) != "404" {
		t.Errorf("status entry = %+v", e)
	}
	e := entryAt(d, "header:content-type")
	if e == nil || e.Part != "headers" || deref(e.A) != `"application/json"` || deref(e.B) != `"text/plain"` {
		t.Errorf("content-type entry = %+v", e)
	}
	if entryAt(d, "header:x-trace") != nil {
		t.Errorf("a masked header on both sides is not a difference: %+v", d.Entries)
	}
	// one side has a declared header the other lacks
	c, _ := canonOf(t, kv, 200, map[string][]string{"Content-Type": {"application/json"}}, `{"a":1}`)
	d = Diff(a, c)
	if e := entryAt(d, "header:x-trace"); e == nil || e.Change != "only_a" || !e.Masked || e.A != nil {
		t.Errorf("a masked header present on one side: %+v", e)
	}
	raw, _ := json.Marshal(d)
	if strings.Contains(string(raw), "t-ONE") || strings.Contains(string(raw), "t-TWO") {
		t.Errorf("a masked header value is in the diff: %s", raw)
	}
}

func TestDiff_IsBoundedSaysSoAndCountsTheRest(t *testing.T) {
	var ba, bb strings.Builder
	ba.WriteString("[")
	bb.WriteString("[")
	const n = MaxDiffEntries + 37
	for i := 0; i < n; i++ {
		if i > 0 {
			ba.WriteString(",")
			bb.WriteString(",")
		}
		fmt.Fprintf(&ba, "%d", i)
		fmt.Fprintf(&bb, "%d", i+1000)
	}
	ba.WriteString("]")
	bb.WriteString("]")
	a, _ := canonOf(t, nil, 200, nil, ba.String())
	b, _ := canonOf(t, nil, 200, nil, bb.String())
	d := Diff(a, b)
	if len(d.Entries) != MaxDiffEntries || !d.Truncated || d.Total != n {
		t.Fatalf("entries %d truncated %v total %d; want %d, true, %d", len(d.Entries), d.Truncated, d.Total, MaxDiffEntries, n)
	}
	// one long value is cut and says so
	long := strings.Repeat("y", 5*MaxDiffValueBytes)
	l1, _ := canonOf(t, nil, 200, nil, `{"s":"`+long+`"}`)
	l2, _ := canonOf(t, nil, 200, nil, `{"s":"z"}`)
	d = Diff(l1, l2)
	e := entryAt(d, "$.s")
	if e == nil || !e.Cut || len(deref(e.A)) > MaxDiffValueBytes || deref(e.B) != `"z"` {
		t.Fatalf("a long value: %+v", e)
	}
	// a diff of two small outputs is not truncated
	if d := Diff(l2, l2); d.Truncated {
		t.Errorf("an empty diff says truncated")
	}
}

func TestDiff_IsDeterministic(t *testing.T) {
	a, _ := canonOf(t, nil, 200, nil, `{"z":1,"a":{"q":[1,2],"b":2},"m":"x"}`)
	b, _ := canonOf(t, nil, 201, nil, `{"a":{"b":3,"q":[1,2,3]},"m":"y","z":1}`)
	first, _ := json.Marshal(Diff(a, b))
	for i := 0; i < 20; i++ {
		again, _ := json.Marshal(Diff(a, b))
		if string(again) != string(first) {
			t.Fatalf("run %d differs:\n%s\n%s", i, first, again)
		}
	}
	// the order is the order of the canonical trees: status first, then keys in byte order
	d := Diff(a, b)
	var paths []string
	for _, e := range d.Entries {
		paths = append(paths, e.Path)
	}
	if got := strings.Join(paths, " "); got != "status $.a.b $.a.q[2] $.m" {
		t.Errorf("entry order = %q", got)
	}
}

func TestDiff_ACutOrUnrecomputableBodyIsNotComparedAndSaysSo(t *testing.T) {
	rules := mustRules(t, "Reference", "measured", "Output", "status, body")
	big := `{"s":"` + strings.Repeat("x", StoredBodyLimit+10) + `"}`
	b := buildOutput(t, rules, 200, nil, []byte(big))
	cut, v := b.read(t)
	if v.State != VerifyPartial {
		t.Fatalf("fixture: %+v", v)
	}
	other, _ := canonOf(t, []string{"Output", "status, body"}, 200, nil, `{"s":"x"}`)
	d := Diff(cut, other)
	if d.Equal || d.Body.State != "not_comparable" || len(d.Entries) != 0 {
		t.Errorf("a cut body against a whole one = %+v", d)
	}
	if d := Diff(cut, cut); d.Body.State != "not_comparable" || d.Equal {
		t.Errorf("a cut body against itself must not read as equal: %+v", d)
	}
}

func TestDiff_ANilSideIsNotAPanic(t *testing.T) {
	a, _ := canonOf(t, nil, 200, nil, `{"a":1}`)
	if d := Diff(nil, a); d.Equal {
		t.Errorf("nil against an output reads equal")
	}
	if d := Diff(nil, nil); d.Equal {
		t.Errorf("nil against nil reads equal: nothing was compared")
	}
}

// ── a one-sided element that HOLDS a masked value ──────────────────────────────────

const maskSecret = "S3CR3T-VALUE-9f2c"

func marshalDiff(t *testing.T, d DiffResult) string {
	t.Helper()
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func noSecret(t *testing.T, what string, d DiffResult) {
	t.Helper()
	if raw := marshalDiff(t, d); strings.Contains(raw, maskSecret) {
		t.Errorf("%s: the difference carries the masked value: %s", what, raw)
	}
}

func TestDiff_AOneSidedElementHoldingAMaskedValueIsShownWithItsKeys(t *testing.T) {
	rules := []string{"Mask", "$.SessionData[*].Value"}
	a, _ := canonOf(t, rules, 200, nil, `{"SessionData":[{"Key":"UserLogin","Value":"`+maskSecret+`"}]}`)
	b, _ := canonOf(t, rules, 200, nil, `{"SessionData":[{"Key":"UserLogin","Value":"`+maskSecret+`"},{"Key":"Locale","Value":"`+maskSecret+`"}]}`)
	want := `{"Key":"Locale","Value":"<masked>"}`
	d := Diff(a, b)
	e := entryAt(d, "$.SessionData[1]")
	if d.Equal || e == nil || e.Change != "only_b" || !e.Masked || e.A != nil || deref(e.B) != want {
		t.Fatalf("extra element on b = %+v", d)
	}
	noSecret(t, "extra element on b", d)
	d = Diff(b, a)
	e = entryAt(d, "$.SessionData[1]")
	if d.Equal || e == nil || e.Change != "only_a" || !e.Masked || e.B != nil || deref(e.A) != want {
		t.Fatalf("extra element on a = %+v", d)
	}
	noSecret(t, "extra element on a", d)
}

func TestDiff_AOneSidedElementOfAnUnorderedSetHoldingAMaskedValueIsShownWithItsKeys(t *testing.T) {
	rules := []string{"Unordered", "$.SessionData", "Mask", "$.SessionData[*].Value"}
	a, _ := canonOf(t, rules, 200, nil, `{"SessionData":[{"Key":"UserLogin","Value":"`+maskSecret+`"}]}`)
	b, _ := canonOf(t, rules, 200, nil, `{"SessionData":[{"Key":"Locale","Value":"`+maskSecret+`"},{"Key":"UserLogin","Value":"`+maskSecret+`"}]}`)
	d := Diff(a, b)
	e := entryAt(d, "$.SessionData[*]")
	if d.Equal || len(d.Entries) != 1 || e == nil || e.Change != "only_b" || !e.Masked || e.A != nil || deref(e.B) != `{"Key":"Locale","Value":"<masked>"}` {
		t.Fatalf("unordered one-sided = %+v", d)
	}
	noSecret(t, "unordered", d)
}

func TestDiff_AnObjectKeyOnOneSideWithAMaskedLeafUnderItIsShownWithItsKeys(t *testing.T) {
	rules := []string{"Mask", "$.extra.token"}
	a, _ := canonOf(t, rules, 200, nil, `{"id":1}`)
	b, _ := canonOf(t, rules, 200, nil, `{"id":1,"extra":{"name":"n","token":"`+maskSecret+`"}}`)
	d := Diff(a, b)
	e := entryAt(d, "$.extra")
	if d.Equal || e == nil || e.Change != "only_b" || !e.Masked || e.A != nil || deref(e.B) != `{"name":"n","token":"<masked>"}` {
		t.Fatalf("one-sided key = %+v", d)
	}
	if e2 := entryAt(Diff(b, a), "$.extra"); e2 == nil || e2.Change != "only_a" || deref(e2.A) != `{"name":"n","token":"<masked>"}` || e2.B != nil {
		t.Errorf("the same on side a = %+v", e2)
	}
	noSecret(t, "object key", d)
}

func TestDiff_AOneSidedValueThatIsItselfMaskedStillCarriesNoAAndNoB(t *testing.T) {
	// an element whose own location is masked (`$.items[*]`), and an object key whose own location is masked
	rules := []string{"Mask", "$.items[*]; $.extra"}
	a, _ := canonOf(t, rules, 200, nil, `{"items":["`+maskSecret+`"]}`)
	b, _ := canonOf(t, rules, 200, nil, `{"items":["`+maskSecret+`","`+maskSecret+`2"],"extra":{"k":"`+maskSecret+`"}}`)
	d := Diff(a, b)
	for _, p := range []string{"$.items[1]", "$.extra"} {
		if e := entryAt(d, p); e == nil || !e.Masked || e.A != nil || e.B != nil {
			t.Errorf("%s = %+v", p, e)
		}
	}
	noSecret(t, "own location masked", d)
	if raw := marshalDiff(t, d); strings.Contains(raw, "<masked>") {
		t.Errorf("an entry whose own location is masked shows the token: %s", raw)
	}
}

func TestDiff_AMaskOnOneSideOnlyNeverShowsTheOtherSidesRealValue(t *testing.T) {
	masked, _ := canonOf(t, []string{"Mask", "$.items[*].Value"}, 200, nil, `{"items":[{"Key":"k","Value":"x"}]}`)
	real, _ := canonOf(t, nil, 200, nil, `{"items":[{"Key":"k","Value":"`+maskSecret+`"}],"o":{"Value":"plain"}}`)
	// same location, one side masked: no A, no B
	d := Diff(masked, real)
	if e := entryAt(d, "$.items[0].Value"); e == nil || !e.Masked || e.A != nil || e.B != nil || e.Change != "changed" {
		t.Errorf("masked against real = %+v", e)
	}
	// a kind mismatch where one side holds a mask: both sides stay hidden
	m2, _ := canonOf(t, []string{"Mask", "$.o.Value"}, 200, nil, `{"o":{"Value":"x"}}`)
	r2, _ := canonOf(t, nil, 200, nil, `{"o":"`+maskSecret+`"}`)
	d2 := Diff(m2, r2)
	if e := entryAt(d2, "$.o"); e == nil || !e.Masked || e.A != nil || e.B != nil || e.Change != "changed" {
		t.Errorf("kind mismatch with a mask = %+v", e)
	}
	noSecret(t, "one-sided mask", d)
	noSecret(t, "one-sided mask, kind mismatch", d2)
	noSecret(t, "one-sided mask, reversed", Diff(real, masked))
}

func TestDiff_NoMaskedValueReachesAnyEntryWhateverTheShape(t *testing.T) {
	// the guard: the same secret on both sides at every masked location, one-sided elements of every kind, headers.
	rules := []string{"Output", "status, body, header:X-Secret, header:X-Other", "Unordered", "$.set", "Mask", "$.list[*].v; $.set[*].v; $.obj.tok; $.own[*]; $.nested.deep[*].v; header:X-Secret"}
	body := func(extra bool) string {
		list := `{"k":"a","v":"` + maskSecret + `"}`
		set := `{"k":"a","v":"` + maskSecret + `"}`
		obj := `{"n":1}`
		own := `"` + maskSecret + `"`
		nested := ``
		if extra {
			list += `,{"k":"b","v":"` + maskSecret + `"}`
			set += `,{"k":"b","v":"` + maskSecret + `"}`
			obj = `{"n":1,"tok":"` + maskSecret + `"}`
			own += `,"` + maskSecret + `"`
			nested = `,"nested":{"deep":[{"v":"` + maskSecret + `"}]}`
		}
		return `{"list":[` + list + `],"set":[` + set + `],"obj":` + obj + `,"own":[` + own + `]` + nested + `}`
	}
	a, _ := canonOf(t, rules, 200, map[string][]string{"X-Secret": {maskSecret}}, body(false))
	b, _ := canonOf(t, rules, 200, map[string][]string{"X-Secret": {maskSecret}, "X-Other": {"o"}}, body(true))
	for _, d := range []DiffResult{Diff(a, b), Diff(b, a)} {
		if d.Equal || len(d.Entries) == 0 {
			t.Fatalf("expected differences: %+v", d)
		}
		noSecret(t, "guard", d)
	}
}
