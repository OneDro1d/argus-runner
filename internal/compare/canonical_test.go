package compare

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type canonVector struct {
	Name               string                        `json:"name"`
	Status             int                           `json:"status"`
	Headers            map[string][]string           `json:"headers"`
	BodyB64            string                        `json:"body_b64"`
	Rules              []struct{ Key, Value string } `json:"rules"`
	ExpectBody         string                        `json:"expect_body"`
	ExpectHeaders      string                        `json:"expect_headers"`
	ExpectBodyKind     string                        `json:"expect_body_kind"`
	ExpectMasksApplied int                           `json:"expect_masks_applied"`
	ExpectHash         string                        `json:"expect_hash"`
	ExpectValues       []ToleranceValue              `json:"expect_values"`
}

func mustRules(t *testing.T, kvs ...string) *Rules {
	t.Helper()
	var raw []RawKV
	for i := 0; i+1 < len(kvs); i += 2 {
		raw = append(raw, RawKV{Key: kvs[i], Value: kvs[i+1], Line: i/2 + 1})
	}
	r, probs := BuildRules(raw)
	if len(probs) > 0 {
		t.Fatalf("BuildRules(%v) problems: %+v", kvs, probs)
	}
	return r
}

// The vectors under testdata/ were written by hand and hashed by an independent script
// (canon_vectors.py), so a change to the algorithm cannot also change its own expected values.
func TestCanonicalVectors(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "canonical_vectors.json"))
	if err != nil {
		t.Fatal(err)
	}
	var vs []canonVector
	if err := json.Unmarshal(b, &vs); err != nil {
		t.Fatal(err)
	}
	if len(vs) < 20 {
		t.Fatalf("only %d canonical vectors; the file lost cases", len(vs))
	}
	for _, v := range vs {
		t.Run(v.Name, func(t *testing.T) {
			body, err := base64.StdEncoding.DecodeString(v.BodyB64)
			if err != nil {
				t.Fatal(err)
			}
			var kvs []string
			for _, kv := range v.Rules {
				kvs = append(kvs, kv.Key, kv.Value)
			}
			rules := mustRules(t, kvs...)
			rec, stored := BuildRecord(Response{Status: v.Status, Headers: v.Headers, Body: body}, rules.Spec(), "", 1)
			if rec.State != StateRecorded {
				t.Fatalf("state = %q reason = %q, want recorded", rec.State, rec.Reason)
			}
			if rec.Hash != v.ExpectHash {
				t.Errorf("hash = %s, want %s", rec.Hash, v.ExpectHash)
			}
			if rec.BodyKind != v.ExpectBodyKind {
				t.Errorf("body_kind = %q, want %q", rec.BodyKind, v.ExpectBodyKind)
			}
			if rec.MasksApplied != v.ExpectMasksApplied {
				t.Errorf("masks_applied = %d, want %d", rec.MasksApplied, v.ExpectMasksApplied)
			}
			if stored == nil {
				t.Fatal("no stored form for a recorded output")
			}
			if got := stored.CanonicalBody(); got != v.ExpectBody {
				t.Errorf("canonical body = %q, want %q", got, v.ExpectBody)
			}
			if got := stored.CanonicalHeaders(); v.ExpectHeaders != "" && got != v.ExpectHeaders {
				t.Errorf("canonical headers = %q, want %q", got, v.ExpectHeaders)
			}
			if len(v.ExpectValues) > 0 {
				if len(rec.Values) != len(v.ExpectValues) {
					t.Fatalf("values = %+v, want %+v", rec.Values, v.ExpectValues)
				}
				for i, want := range v.ExpectValues {
					if rec.Values[i] != want {
						t.Errorf("values[%d] = %+v, want %+v", i, rec.Values[i], want)
					}
				}
			}
		})
	}
}

func rec(t *testing.T, body string, kvs ...string) OutputRecord {
	t.Helper()
	rules := mustRules(t, append([]string{"Reference", "measured"}, kvs...)...)
	r, _ := BuildRecord(Response{Status: 200, Body: []byte(body)}, rules.Spec(), "", 1)
	return r
}

func TestNullAndAbsentStayDifferent(t *testing.T) {
	a, b := rec(t, `{"a":null}`), rec(t, `{}`)
	if a.Hash == b.Hash {
		t.Fatal("null and absent must hash differently")
	}
	if rec(t, `{"a":null}`).Hash == rec(t, `{"a":0}`).Hash || rec(t, `{"a":null}`).Hash == rec(t, `{"a":""}`).Hash {
		t.Fatal("null must differ from 0 and from the empty string")
	}
}

func TestObjectKeyOrderDoesNotChangeTheHash(t *testing.T) {
	if rec(t, `{"a":1,"b":2}`).Hash != rec(t, `{"b":2,"a":1}`).Hash {
		t.Fatal("key order must not matter")
	}
	if rec(t, `[1,2]`).Hash == rec(t, `[2,1]`).Hash {
		t.Fatal("array order matters unless declared unordered")
	}
}

func TestEquivalentNumbersHashEqual(t *testing.T) {
	if rec(t, `{"n":1}`).Hash != rec(t, `{"n":1.0}`).Hash || rec(t, `{"n":100}`).Hash != rec(t, `{"n":1e2}`).Hash {
		t.Fatal("1, 1.0 and 1e0 are one number")
	}
	if rec(t, `{"n":9007199254740993}`).Hash == rec(t, `{"n":9007199254740992}`).Hash {
		t.Fatal("integers beyond 2^53 are kept as written and must not collapse")
	}
	if rec(t, `{"n":1}`).Hash == rec(t, `{"n":"1"}`).Hash {
		t.Fatal("a number and a string are different")
	}
}

// DECISION: a mask hides a VALUE, never the presence of a field. A path present on one side and
// absent on the other stays a visible difference.
func TestMaskDoesNotHideAbsence(t *testing.T) {
	with := rec(t, `{"id":1,"v":1}`, "Mask", "$.id")
	without := rec(t, `{"v":1}`, "Mask", "$.id")
	other := rec(t, `{"id":99,"v":1}`, "Mask", "$.id")
	if with.Hash == without.Hash {
		t.Fatal("masked-present must differ from absent")
	}
	if with.Hash != other.Hash {
		t.Fatal("two masked values must hash equal")
	}
	if with.MasksApplied != 1 || without.MasksApplied != 0 {
		t.Fatalf("masks_applied = %d / %d, want 1 / 0", with.MasksApplied, without.MasksApplied)
	}
}

// DECISION: a real string equal to the mask token cannot be confused with a masked field.
func TestRealValueEqualToMaskTokenIsNotAMaskedField(t *testing.T) {
	masked := rec(t, `{"a":1}`, "Mask", "$.a")
	real := rec(t, `{"a":"<masked>"}`)
	if masked.Hash == real.Hash {
		t.Fatal("a real \"<masked>\" string hashed like a masked field")
	}
	rt := rec(t, `{"a":1}`, "Mask", "$.a", "Tolerance", "$.zzz abs 1")
	if rt.Hash != masked.Hash {
		t.Fatal("an unrelated tolerance rule must not change the hash of a body it does not touch")
	}
	tol := rec(t, `{"a":1}`, "Tolerance", "$.a abs 1")
	if tol.Hash == real.Hash {
		t.Fatal("a real \"<tolerance>\" lookalike hashed like a tolerant leaf")
	}
	realTol := rec(t, `{"a":"<tolerance>"}`)
	if tol.Hash == realTol.Hash {
		t.Fatal("a real \"<tolerance>\" string hashed like a tolerant leaf")
	}
}

func TestAlmostJSONIsText(t *testing.T) {
	for name, body := range map[string]string{
		"bom":            "\xef\xbb\xbf{\"a\":1}",
		"trailing":       `{"a":1} x`,
		"two values":     `{"a":1}{"b":2}`,
		"dup key":        `{"a":1,"a":2}`,
		"nested dup":     `{"o":{"k":1,"k":1}}`,
		"trailing comma": `{"a":1,}`,
		"single quotes":  `{'a':1}`,
		"nan":            `{"a":NaN}`,
		"leading plus":   `{"a":+1}`,
	} {
		r := rec(t, body)
		if r.State != StateRecorded || r.BodyKind != KindText {
			t.Errorf("%s: state=%q kind=%q reason=%q, want recorded text", name, r.State, r.BodyKind, r.Reason)
		}
	}
	if rec(t, " \n{\"a\" : 1}\n").BodyKind != KindJSON {
		t.Error("insignificant whitespace around JSON is still JSON")
	}
	deep := strings.Repeat("[", 200) + strings.Repeat("]", 200)
	if rec(t, deep).BodyKind != KindText {
		t.Error("a body nested deeper than the limit must be text, never a stack overflow")
	}
}

func TestEmptyIsItsOwnKind(t *testing.T) {
	r := rec(t, ``)
	if r.BodyKind != KindEmpty || r.State != StateRecorded {
		t.Fatalf("empty body: %+v", r)
	}
	if r.Hash == rec(t, `""`).Hash || r.Hash == rec(t, ` `).Hash {
		t.Fatal("empty must differ from the empty JSON string and from whitespace-only text")
	}
	// an empty body has nothing to mask: declared masks do not make it unrecordable
	if m := rec(t, ``, "Mask", "$.id"); m.State != StateRecorded {
		t.Fatalf("empty body with a mask = %+v", m)
	}
}

func TestPathRulesNeedJSON(t *testing.T) {
	m := rec(t, "plain text", "Mask", "$.id")
	if m.State != StateNotRecorded || m.Reason != ReasonMaskNeedsJSON || m.Hash != "" {
		t.Fatalf("mask on text: %+v", m)
	}
	u := rec(t, "plain text", "Unordered", "$.items")
	if u.State != StateNotRecorded || u.Reason != ReasonPathRuleNeedsJSON {
		t.Fatalf("unordered on text: %+v", u)
	}
	tl := rec(t, "plain text", "Tolerance", "$.x abs 1")
	if tl.State != StateNotRecorded || tl.Reason != ReasonPathRuleNeedsJSON {
		t.Fatalf("tolerance on text: %+v", tl)
	}
}

func TestBodyTooLargeIsNeverHashedAsAPrefix(t *testing.T) {
	big := bytes.Repeat([]byte("a"), MaxBodyBytes+1)
	rules := mustRules(t, "Reference", "measured")
	r, st := BuildRecord(Response{Status: 200, Body: big}, rules.Spec(), "", 1)
	if r.State != StateNotRecorded || r.Reason != ReasonBodyTooLarge || r.Hash != "" || st != nil {
		t.Fatalf("over-cap body: %+v", r)
	}
	ok := bytes.Repeat([]byte("a"), MaxBodyBytes)
	r2, _ := BuildRecord(Response{Status: 200, Body: ok}, rules.Spec(), "", 1)
	if r2.State != StateRecorded {
		t.Fatalf("a body of exactly the cap must record: %+v", r2)
	}
	// a body that is not compared cannot make the output unrecordable
	so := mustRules(t, "Reference", "measured", "Output", "status")
	r3, _ := BuildRecord(Response{Status: 200, Body: big}, so.Spec(), "", 1)
	if r3.State != StateRecorded {
		t.Fatalf("status-only output with a big body: %+v", r3)
	}
}

func TestStoredBodyIsCappedButHashCoversEverything(t *testing.T) {
	rules := mustRules(t, "Reference", "measured")
	a := bytes.Repeat([]byte("x"), StoredBodyLimit+10)
	b := append(bytes.Repeat([]byte("x"), StoredBodyLimit+9), 'y')
	ra, sa := BuildRecord(Response{Status: 200, Body: a}, rules.Spec(), "", 1)
	rb, _ := BuildRecord(Response{Status: 200, Body: b}, rules.Spec(), "", 1)
	if !ra.Truncated || sa == nil {
		t.Fatalf("record = %+v", ra)
	}
	if len(sa.CanonicalBody()) > StoredBodyLimit {
		t.Fatalf("stored body is %d bytes, cap %d", len(sa.CanonicalBody()), StoredBodyLimit)
	}
	if ra.Hash == rb.Hash {
		t.Fatal("the hash must cover the whole body, not the stored prefix")
	}
	if ra.BodyBytes != len(a) {
		t.Fatalf("body_bytes = %d, want %d (bytes read)", ra.BodyBytes, len(a))
	}
}

func TestHeaderRules(t *testing.T) {
	rules := mustRules(t, "Reference", "measured", "Output", "status, header:X-A, header:x-b")
	r1, _ := BuildRecord(Response{Status: 200, Headers: map[string][]string{"X-A": {" 1 "}, "X-B": {"2"}, "X-C": {"3"}}}, rules.Spec(), "", 1)
	r2, _ := BuildRecord(Response{Status: 200, Headers: map[string][]string{"x-a": {"1"}, "x-b": {"2"}}}, rules.Spec(), "", 1)
	if r1.Hash != r2.Hash {
		t.Fatal("header names are case-folded, values trimmed, undeclared headers ignored")
	}
	r3, _ := BuildRecord(Response{Status: 200, Headers: map[string][]string{"x-a": {"1"}, "x-b": {"changed"}}}, rules.Spec(), "", 1)
	if r1.Hash == r3.Hash {
		t.Fatal("a declared header value change must change the hash")
	}
	r4, _ := BuildRecord(Response{Status: 200, Headers: map[string][]string{"x-a": {"1"}}}, rules.Spec(), "", 1)
	if r4.Hash == r1.Hash {
		t.Fatal("a declared header that is absent must differ from one that is present")
	}
	if r1.Parts.Body != "" || r1.Parts.Status == "" || r1.Parts.Headers == "" {
		t.Fatalf("parts = %+v: only the selected parts carry a hash", r1.Parts)
	}
	multi, _ := BuildRecord(Response{Status: 200, Headers: map[string][]string{"x-a": {"1", "2"}, "x-b": {"2"}}}, rules.Spec(), "", 1)
	joined, _ := BuildRecord(Response{Status: 200, Headers: map[string][]string{"x-a": {"1, 2"}, "x-b": {"2"}}}, rules.Spec(), "", 1)
	if multi.Hash != joined.Hash {
		t.Fatal("repeated header lines are joined with a comma and a space")
	}
}

func TestRefusedHeadersAreNeverRecorded(t *testing.T) {
	for _, name := range []string{"Set-Cookie", "set-cookie", "Authorization", "Proxy-Authorization", "WWW-Authenticate", "Cookie"} {
		if !RefusedHeader(name) {
			t.Errorf("%q must be on the refused list", name)
		}
		_, probs := BuildRules([]RawKV{{Key: "Reference", Value: "measured"}, {Key: "Output", Value: "header:" + name}})
		if len(probs) == 0 {
			t.Errorf("Output header:%s must be refused at validate time", name)
		}
		// defence in depth: a hand-built Spec that names one is still refused at record time
		spec := Spec{Status: true, Headers: []string{strings.ToLower(name)}}
		r, st := BuildRecord(Response{Status: 200, Headers: map[string][]string{name: {"secret"}}}, spec, "", 1)
		if r.State != StateNotRecorded || r.Reason != ReasonHeaderNotAllowed || st != nil || r.Hash != "" {
			t.Errorf("record with %s: %+v", name, r)
		}
	}
	if RefusedHeader("Content-Type") {
		t.Error("Content-Type is an ordinary header")
	}
}

func TestUnorderedSortsAfterMaskingAndScrubbing(t *testing.T) {
	// ids differ (masked), so the order must come from what is left, on both sides
	a := rec(t, `{"items":[{"id":9,"k":"b"},{"id":1,"k":"a"}]}`, "Unordered", "$.items", "Mask", "$.items[*].id")
	b := rec(t, `{"items":[{"id":2,"k":"a"},{"id":8,"k":"b"}]}`, "Unordered", "$.items", "Mask", "$.items[*].id")
	if a.Hash != b.Hash {
		t.Fatal("unordered arrays of masked objects must hash equal")
	}
	c := rec(t, `{"items":[{"id":2,"k":"a"},{"id":8,"k":"c"}]}`, "Unordered", "$.items", "Mask", "$.items[*].id")
	if a.Hash == c.Hash {
		t.Fatal("a different unmasked element must still differ")
	}
	// nested: inner arrays sort first so the outer sort sees canonical elements
	n1 := rec(t, `{"g":[{"t":[2,1]},{"t":[4,3]}]}`, "Unordered", "$.g; $.g[*].t")
	n2 := rec(t, `{"g":[{"t":[3,4]},{"t":[1,2]}]}`, "Unordered", "$.g; $.g[*].t")
	if n1.Hash != n2.Hash {
		t.Fatal("nested unordered arrays must normalise inside-out")
	}
}

func TestScrubRunsAfterMaskBeforeHash(t *testing.T) {
	rules := mustRules(t, "Reference", "measured", "Mask", "$.skip")
	spec := rules.Spec()
	spec.Scrub = func(s string) string { return strings.ReplaceAll(s, "SECRET", "${saved.x}") }
	a, sa := BuildRecord(Response{Status: 200, Body: []byte(`{"tok":"a SECRET b","skip":"SECRET"}`)}, spec, "", 1)
	b, _ := BuildRecord(Response{Status: 200, Body: []byte(`{"tok":"a ${saved.x} b","skip":"other"}`)}, spec, "", 1)
	if a.Hash != b.Hash {
		t.Fatal("a scrubbed value must hash like its placeholder")
	}
	if strings.Contains(sa.CanonicalBody(), "SECRET") {
		t.Fatalf("stored bytes still carry the secret: %s", sa.CanonicalBody())
	}
	plain := mustRules(t, "Reference", "measured").Spec()
	plain.Scrub = spec.Scrub
	text, st := BuildRecord(Response{Status: 200, Body: []byte("plain SECRET\r\n")}, plain, "", 1)
	if text.BodyKind != KindText || strings.Contains(st.CanonicalBody(), "SECRET") {
		t.Fatalf("text bodies are scrubbed too: %q", st.CanonicalBody())
	}
	hd := mustRules(t, "Reference", "measured", "Output", "header:X-T")
	hs := hd.Spec()
	hs.Scrub = spec.Scrub
	_, sh := BuildRecord(Response{Status: 200, Headers: map[string][]string{"x-t": {"SECRET"}}}, hs, "", 1)
	if strings.Contains(sh.CanonicalHeaders(), "SECRET") {
		t.Fatalf("header values are scrubbed too: %s", sh.CanonicalHeaders())
	}
}

func TestOnlyTheRecordedPartsAreHashed(t *testing.T) {
	so := mustRules(t, "Reference", "measured", "Output", "status")
	a, _ := BuildRecord(Response{Status: 200, Body: []byte("one")}, so.Spec(), "", 1)
	b, _ := BuildRecord(Response{Status: 200, Body: []byte("two")}, so.Spec(), "", 1)
	if a.Hash != b.Hash || a.BodyKind != "" {
		t.Fatalf("status-only must ignore the body: %+v", a)
	}
	c, _ := BuildRecord(Response{Status: 404, Body: []byte("one")}, so.Spec(), "", 1)
	if a.Hash == c.Hash {
		t.Fatal("status must change the hash")
	}
}

func TestRecordMetadata(t *testing.T) {
	rules := mustRules(t, "Reference", "measured")
	r, _ := BuildRecord(Response{Status: 200, Body: []byte(`{"a":1}`)}, rules.Spec(), "create", 3)
	if r.V != 1 || r.Step != "create" || r.Sample != 3 || r.Status != 200 || r.BodyBytes != 7 || r.State != StateRecorded || r.Reason != "" {
		t.Fatalf("record = %+v", r)
	}
	over, _ := BuildRecord(Response{Status: 200, Body: []byte("x")}, rules.Spec(), "", MaxSamples+1)
	if over.State != StateNotRecorded || over.Reason != ReasonTooManySamples {
		t.Fatalf("sample past the cap = %+v", over)
	}
	noresp, st := BuildRecord(Response{}, rules.Spec(), "", 1)
	if noresp.State != StateNotRecorded || noresp.Reason != ReasonNoResponse || st != nil {
		t.Fatalf("no response = %+v", noresp)
	}
}

func TestToleranceValueCap(t *testing.T) {
	rules := mustRules(t, "Reference", "measured", "Tolerance", "$.v[*] abs 1")
	var sb strings.Builder
	sb.WriteString(`{"v":[`)
	for i := 0; i <= MaxToleranceValues; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString("1")
	}
	sb.WriteString(`]}`)
	r, _ := BuildRecord(Response{Status: 200, Body: []byte(sb.String())}, rules.Spec(), "", 1)
	if r.State != StateNotRecorded || r.Reason != ReasonTooManyValues {
		t.Fatalf("65 tolerant values = %+v", r)
	}
}

func TestStoredFileShape(t *testing.T) {
	rules := mustRules(t, "Reference", "measured", "Output", "status, body, header:X-A")
	_, st := BuildRecord(Response{Status: 201, Headers: map[string][]string{"X-A": {"v"}}, Body: []byte(`{"a":1}`)}, rules.Spec(), "", 1)
	var got struct {
		Status   int               `json:"status"`
		Headers  map[string]string `json:"headers"`
		BodyKind string            `json:"body_kind"`
		Body     json.RawMessage   `json:"body"`
	}
	if err := json.Unmarshal(st.File(), &got); err != nil {
		t.Fatalf("stored file is not JSON: %v", err)
	}
	if got.Status != 201 || got.Headers["x-a"] != "v" || got.BodyKind != KindJSON || string(got.Body) != `{"a":1}` {
		t.Fatalf("stored = %+v", got)
	}
}
