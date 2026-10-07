package compare

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

// ARGUS-CMP-3: the stored file must keep EXACTLY the bytes that were hashed. A text body that is not
// valid UTF-8 cannot ride a JSON string (decoding turns each bad byte into U+FFFD), so File() writes it
// as base64 with a marker.

func textSpec() Spec { return Spec{Status: true, Body: true} }

func TestStoredFile_InvalidUTF8TextBodyIsStoredLosslessly(t *testing.T) {
	raw := []byte("ab\xff\xfe\x00cd\r\nef")
	rec, st := BuildRecord(Response{Status: 200, Body: raw}, textSpec(), "", 1)
	if rec.State != StateRecorded || rec.BodyKind != KindText {
		t.Fatalf("record %+v", rec)
	}
	file := st.File()
	if !json.Valid(file) {
		t.Fatalf("the stored file is not valid JSON (raw bad bytes in a string): %q", file)
	}
	var f struct {
		Body     *string `json:"body"`
		Encoding string  `json:"body_encoding"`
		B64      string  `json:"body_b64"`
	}
	if err := json.Unmarshal(file, &f); err != nil {
		t.Fatal(err)
	}
	if f.Encoding != "base64" || f.Body != nil {
		t.Fatalf("expected body null + body_encoding base64, got %s", file)
	}
	got, err := base64.StdEncoding.DecodeString(f.B64)
	if err != nil {
		t.Fatal(err)
	}
	// the canonical text is the body with CRLF -> LF; the bad bytes survive exactly
	want := "ab\xff\xfe\x00cd\nef"
	if string(got) != want || st.CanonicalBody() != want {
		t.Errorf("stored bytes %q, want %q", got, want)
	}
	// and the part hash is over exactly those bytes
	if rec.Parts.Body != PartHash(DomainBody, []byte(want)) {
		t.Errorf("body part hash is not over the stored bytes")
	}
}

func TestStoredFile_ValidUTF8TextAndJSONStayAsBefore(t *testing.T) {
	_, st := BuildRecord(Response{Status: 200, Body: []byte("héllo\nworld")}, textSpec(), "", 1)
	if f := string(st.File()); !strings.Contains(f, `"body":"héllo\nworld"`) || strings.Contains(f, "base64") {
		t.Errorf("valid UTF-8 text must stay a JSON string: %s", f)
	}
	_, st = BuildRecord(Response{Status: 200, Body: []byte(`{"b":1,"a":[2]}`)}, textSpec(), "", 1)
	if f := string(st.File()); !strings.Contains(f, `"body":{"a":[2],"b":1}`) || strings.Contains(f, "base64") {
		t.Errorf("JSON must stay embedded: %s", f)
	}
}

func TestStoredFile_InvalidUTF8HeaderValueIsStoredLosslessly(t *testing.T) {
	spec := Spec{Status: true, Headers: []string{"x-v"}}
	_, st := BuildRecord(Response{Status: 200, Headers: map[string][]string{"X-V": {"a\xffb"}}}, spec, "", 1)
	file := st.File()
	if !json.Valid(file) {
		t.Fatalf("stored file not valid JSON: %q", file)
	}
	var f struct {
		Headers    json.RawMessage `json:"headers"`
		HeadersB64 string          `json:"headers_b64"`
	}
	_ = json.Unmarshal(file, &f)
	got, err := base64.StdEncoding.DecodeString(f.HeadersB64)
	if err != nil || string(got) != st.CanonicalHeaders() || !strings.Contains(string(got), "a\xffb") {
		t.Errorf("headers not stored losslessly: %q (%v) in %s", got, err, file)
	}
}
