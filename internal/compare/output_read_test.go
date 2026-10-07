package compare

// output_read_test.go -- ARGUS-CMP-9: Reconstruct (the relayed stored file, read back through a
// JSONB column, rebuilt into the canonical tree and verified against the control plane's record) and Diff.
//
// The transport is SIMULATED FAITHFULLY by jsonbLike: sorted keys, whitespace, HTML-safe escapes and numbers spelled
// out as PostgreSQL's numeric prints them. A test that fed Reconstruct the executor's own bytes would never meet the
// ambiguity this code exists for (a real "<masked>" string and a masked leaf are the same string after a JSONB
// round trip).

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// plainDecimal spells a JSON number the way a PostgreSQL numeric prints it: no exponent.
func plainDecimal(lit string) string {
	neg := strings.HasPrefix(lit, "-")
	s := strings.TrimPrefix(lit, "-")
	mant, exp := s, 0
	if i := strings.IndexAny(s, "eE"); i >= 0 {
		mant = s[:i]
		exp, _ = strconv.Atoi(strings.TrimPrefix(s[i+1:], "+"))
	}
	intp, frac, _ := strings.Cut(mant, ".")
	digits := intp + frac
	point := len(intp) + exp
	var out string
	switch {
	case point <= 0:
		out = "0." + strings.Repeat("0", -point) + digits
	case point >= len(digits):
		out = digits + strings.Repeat("0", point-len(digits))
	default:
		out = digits[:point] + "." + digits[point:]
	}
	if strings.Contains(out, ".") {
		out = strings.TrimRight(out, "0")
		out = strings.TrimSuffix(out, ".")
	}
	out = strings.TrimLeft(out, "0")
	if out == "" || strings.HasPrefix(out, ".") {
		out = "0" + out
	}
	if neg && out != "0" {
		out = "-" + out
	}
	return out
}

func fixNumbers(v any) any {
	switch x := v.(type) {
	case json.Number:
		return json.Number(plainDecimal(x.String()))
	case []any:
		for i := range x {
			x[i] = fixNumbers(x[i])
		}
	case map[string]any:
		for k := range x {
			x[k] = fixNumbers(x[k])
		}
	}
	return v
}

// jsonbLike is what a JSONB column does to a JSON document.
func jsonbLike(t *testing.T, b []byte) []byte {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("the stored file is not JSON: %v: %s", err, b)
	}
	out, err := json.MarshalIndent(fixNumbers(v), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// built is one recorded output: the control plane's record and the executor's stored file.
type built struct {
	rec   OutputRecord
	file  []byte
	rules *Rules
}

func buildOutput(t *testing.T, rules *Rules, status int, headers map[string][]string, body []byte) built {
	t.Helper()
	r, st := BuildRecord(Response{Status: status, Headers: headers, Body: body}, rules.Spec(), "", 1)
	if r.State != StateRecorded || st == nil {
		t.Fatalf("not recorded: %s", r.Reason)
	}
	return built{rec: r, file: st.File(), rules: rules}
}

func (b built) read(t *testing.T) (*Canonical, Verification) {
	t.Helper()
	return Reconstruct(jsonbLike(t, b.file), b.rec, b.rules)
}

func mustVerified(t *testing.T, b built) *Canonical {
	t.Helper()
	c, v := b.read(t)
	if v.State != VerifyVerified || c == nil {
		t.Fatalf("verification = %+v, want verified (file %s)", v, b.file)
	}
	return c
}

func loadVectors(t *testing.T) []canonVector {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "canonical_vectors.json"))
	if err != nil {
		t.Fatal(err)
	}
	var vs []canonVector
	if err := json.Unmarshal(raw, &vs); err != nil {
		t.Fatal(err)
	}
	return vs
}

func vectorBuilt(t *testing.T, v canonVector) built {
	t.Helper()
	body, err := base64.StdEncoding.DecodeString(v.BodyB64)
	if err != nil {
		t.Fatal(err)
	}
	var kvs []string
	for _, kv := range v.Rules {
		kvs = append(kvs, kv.Key, kv.Value)
	}
	r, st := BuildRecord(Response{Status: v.Status, Headers: v.Headers, Body: body}, mustRules(t, kvs...).Spec(), "", 1)
	if r.State != StateRecorded || st == nil {
		t.Skipf("vector %s is not recorded (%s)", v.Name, r.Reason)
	}
	return built{rec: r, file: st.File(), rules: mustRules(t, kvs...)}
}

// ── Reconstruct ────────────────────────────────────────────────────────────────────────────────────

func TestReconstruct_EveryGoldenVectorSurvivesTheJSONBRoundTripAndVerifies(t *testing.T) {
	vs := loadVectors(t)
	verified, partial := 0, 0
	for _, v := range vs {
		b := vectorBuilt(t, v)
		c, ver := b.read(t)
		switch ver.State {
		case VerifyVerified:
			verified++
		case VerifyPartial:
			partial++
			// the only golden vectors that may not be fully rechecked are the ones with a number of 1e21 or more
			if !strings.Contains(string(b.file), "e+") && !strings.Contains(v.Name, "number") {
				t.Errorf("%s: partial (%+v) for a body with no number of 1e21 or more", v.Name, ver)
			}
		default:
			t.Errorf("%s: verification = %+v; the executor's own file, after a JSONB round trip, must verify (file %s)", v.Name, ver, b.file)
			continue
		}
		if c == nil {
			t.Errorf("%s: no canonical output", v.Name)
		}
	}
	if verified < 20 || partial > 2 {
		t.Fatalf("verified %d, partial %d of %d vectors: want at least 20 verified and at most 2 partial", verified, partial, len(vs))
	}
}

func TestReconstruct_ATamperedPartIsAMismatchAndIsNeverShown(t *testing.T) {
	rules := mustRules(t, "Reference", "measured", "Output", "status, body, header:Content-Type", "Mask", "$.id")
	b := buildOutput(t, rules, 200, map[string][]string{"Content-Type": {"application/json"}}, []byte(`{"id":7,"name":"ada","tags":["a","b"]}`))
	mustVerified(t, b)
	for name, mutate := range map[string]func(string) string{
		"the body":     func(s string) string { return strings.Replace(s, `"ada"`, `"eve"`, 1) },
		"the status":   func(s string) string { return strings.Replace(s, `"status":200`, `"status":201`, 1) },
		"the header":   func(s string) string { return strings.Replace(s, `application/json`, `text/html`, 1) },
		"a body key":   func(s string) string { return strings.Replace(s, `"name"`, `"nome"`, 1) },
		"a number":     func(s string) string { return strings.Replace(s, `"tags":["a","b"]`, `"tags":["a","b"],"n":1`, 1) },
		"an array":     func(s string) string { return strings.Replace(s, `["a","b"]`, `["b","a"]`, 1) },
		"the mask":     func(s string) string { return strings.Replace(s, `"<masked>"`, `7`, 1) },
		"a swap of ab": func(s string) string { return strings.Replace(s, `"a","b"`, `"a","c"`, 1) },
	} {
		file := []byte(mutate(string(b.file)))
		if bytes.Equal(file, b.file) {
			t.Fatalf("%s: the edit did not apply to %s", name, b.file)
		}
		c, ver := Reconstruct(jsonbLike(t, file), b.rec, b.rules)
		if ver.State != VerifyMismatch || c != nil {
			t.Errorf("%s changed in the stored file: verification = %+v, canonical = %v; want a mismatch and nothing to show", name, ver, c != nil)
		}
		if len(ver.Mismatched) == 0 {
			t.Errorf("%s: a mismatch must name the parts that differ: %+v", name, ver)
		}
	}
	// the record is what is wrong, the file is right: the same outcome
	wrong := b.rec
	wrong.Hash = strings.Repeat("0", 64)
	if c, ver := Reconstruct(jsonbLike(t, b.file), wrong, rules); ver.State != VerifyMismatch || c != nil {
		t.Errorf("a file that does not hash to the recorded total = %+v", ver)
	}
	wrong = b.rec
	wrong.Parts.Body = strings.Repeat("1", 64)
	if c, ver := Reconstruct(jsonbLike(t, b.file), wrong, rules); ver.State != VerifyMismatch || c != nil {
		t.Errorf("a file whose body does not hash to the recorded body part = %+v", ver)
	}
}

func TestReconstruct_ARealStringEqualToTheMaskTokenIsNotAMask(t *testing.T) {
	// After a JSONB round trip a masked leaf and a real "<masked>" string are the same text. The rules say which
	// is which: only a leaf at a declared mask path was masked.
	rules := mustRules(t, "Reference", "measured", "Mask", "$.id")
	b := buildOutput(t, rules, 200, nil, []byte(`{"id":"secret","note":"<masked>","list":["<masked>","<tolerance>"]}`))
	c := mustVerified(t, b)
	if got := c.MaskedPaths(); len(got) != 1 || got[0] != "$.id" {
		t.Errorf("masked paths = %v, want only $.id (the real strings are not masks)", got)
	}
	// and the other way: a mask that hid an object keeps its path
	rules = mustRules(t, "Reference", "measured", "Mask", "$.items[*].createdAt; $.meta")
	b = buildOutput(t, rules, 200, nil, []byte(`{"items":[{"createdAt":"t1","v":1},{"createdAt":"t2","v":2}],"meta":{"a":1}}`))
	c = mustVerified(t, b)
	want := []string{"$.items[0].createdAt", "$.items[1].createdAt", "$.meta"}
	if got := c.MaskedPaths(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("masked paths = %v, want %v", got, want)
	}
}

func TestReconstruct_ATextBodyAnEmptyBodyAndAStatusOnlyCheckVerify(t *testing.T) {
	text := mustRules(t, "Reference", "measured")
	for name, body := range map[string]string{
		"text with CRLF":       "line1\r\nline2",
		"empty":                "",
		"not UTF-8":            "caf\xe9 \xff",
		"text that looks JSON": `{"a":1,"a":2}`,
		"quotes and slashes":   "a \"b\" \\ c </script>",
	} {
		b := buildOutput(t, text, 200, nil, []byte(body))
		c := mustVerified(t, b)
		if got := c.Shown(); got.Status != 200 || got.Headers == nil {
			t.Errorf("%s: shown = %+v", name, got)
		}
	}
	statusOnly := mustRules(t, "Reference", "measured", "Output", "status")
	b := buildOutput(t, statusOnly, 404, nil, []byte(`{"a":1}`))
	c := mustVerified(t, b)
	if sh := c.Shown(); sh.Status != 404 || string(sh.Body) != "null" || sh.BodyKind != "" {
		t.Errorf("a status-only check shows %+v; want status 404 and no body", sh)
	}
}

func TestReconstruct_ACutBodyIsPartialAndHonestAboutIt(t *testing.T) {
	rules := mustRules(t, "Reference", "measured", "Output", "status, body")
	big := `{"s":"` + strings.Repeat("x", StoredBodyLimit+10) + `"}`
	b := buildOutput(t, rules, 200, nil, []byte(big))
	if !b.rec.Truncated {
		t.Fatalf("the fixture is not truncated")
	}
	c, v := b.read(t)
	if v.State != VerifyPartial || c == nil || len(v.Unchecked) != 1 || v.Unchecked[0] != "body" || v.Note != NoteBodyCut {
		t.Fatalf("a cut body: verification = %+v", v)
	}
	if sh := c.Shown(); !sh.Truncated || sh.Status != 200 {
		t.Errorf("shown = %+v; a cut body must say it is cut", sh)
	}
	// the status of a cut output is still checked: a wrong status is a mismatch, not a partial
	file := strings.Replace(string(b.file), `"status":200`, `"status":500`, 1)
	if c, v := Reconstruct(jsonbLike(t, []byte(file)), b.rec, rules); v.State != VerifyMismatch || c != nil {
		t.Errorf("a cut output with a wrong status = %+v", v)
	}
	// a file that says it is cut for a record that says it is whole is not the recorded output
	rc := b.rec
	rc.Truncated = false
	if c, v := Reconstruct(jsonbLike(t, b.file), rc, rules); v.State != VerifyMismatch || c != nil {
		t.Errorf("a cut file for a whole record = %+v", v)
	}
}

func TestReconstruct_ANumberOf1e21OrMoreCannotBeRecomputedAndIsSaid(t *testing.T) {
	rules := mustRules(t, "Reference", "measured")
	b := buildOutput(t, rules, 200, nil, []byte(`{"big":1e21,"small":1e-7,"n":12345678901234567890}`))
	c, v := b.read(t)
	if v.State != VerifyPartial || c == nil || v.Note != NoteBigNumber {
		t.Fatalf("a body with 1e21: verification = %+v", v)
	}
	// without that number the same shapes verify
	b = buildOutput(t, rules, 200, nil, []byte(`{"small":1e-7,"n":12345678901234567890,"f":1.5}`))
	mustVerified(t, b)
}

func TestReconstruct_AFileThatIsNotAStoredOutputIsMalformed(t *testing.T) {
	rules := mustRules(t, "Reference", "measured")
	b := buildOutput(t, rules, 200, nil, []byte(`{"a":1}`))
	for name, file := range map[string]string{
		"not JSON":          "<html>",
		"null":              "null",
		"an array":          "[]",
		"a string status":   `{"status":"200","headers":{},"body_kind":"json","body":{"a":1}}`,
		"headers an array":  `{"status":200,"headers":[],"body_kind":"json","body":{"a":1}}`,
		"a bad body kind":   `{"status":200,"headers":{},"body_kind":"xml","body":{"a":1}}`,
		"a missing body":    `{"status":200,"headers":{},"body_kind":"json"}`,
		"text that is JSON": `{"status":200,"headers":{},"body_kind":"text","body":{"a":1}}`,
	} {
		c, v := Reconstruct([]byte(file), b.rec, rules)
		if v.State != VerifyMalformed || c != nil {
			t.Errorf("%s: verification = %+v, canonical = %v; want malformed and nothing to show", name, v, c != nil)
		}
	}
}

func TestReconstruct_TheStatusAndHeadersAreShownFromTheVerifiedFile(t *testing.T) {
	rules := mustRules(t, "Reference", "measured", "Output", "status, body, header:Content-Type, header:X-Trace", "Mask", "header:X-Trace")
	b := buildOutput(t, rules, 201, map[string][]string{"Content-Type": {"application/json", "charset=utf-8"}, "X-Trace": {"abc"}, "Set-Cookie": {"s=1"}}, []byte(`{"ok":true}`))
	// Set-Cookie is refused at validate time; here it is simply not declared, so it is never in the file
	c := mustVerified(t, b)
	sh := c.Shown()
	if sh.Status != 201 || sh.Headers["content-type"] != "application/json, charset=utf-8" || sh.Headers["x-trace"] != MaskToken {
		t.Errorf("shown = %+v", sh)
	}
	if _, ok := sh.Headers["set-cookie"]; ok {
		t.Errorf("an undeclared header was shown: %+v", sh.Headers)
	}
	if got := c.MaskedPaths(); len(got) != 1 || got[0] != "header:x-trace" {
		t.Errorf("masked paths = %v", got)
	}
}
