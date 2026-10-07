package compare

import (
	"encoding/json"
	"strings"
	"testing"
)

// ARGUS-CMP-3 fixer round (PR #499): the stored file says when it was cut, and its ENCODED size, not
// the raw size of the body, is what is bounded.

func TestStoredFile_SaysItWasTruncatedOnlyWhenItWas(t *testing.T) {
	long := strings.Repeat("a", StoredBodyLimit+10)
	rec, st := BuildRecord(Response{Status: 200, Body: []byte(long)}, textSpec(), "", 1)
	if !rec.Truncated {
		t.Fatalf("the record must say truncated: %+v", rec)
	}
	var f map[string]any
	if err := json.Unmarshal(st.File(), &f); err != nil {
		t.Fatal(err)
	}
	if f["truncated"] != true {
		t.Errorf("a capped body's file must carry top-level truncated:true, keys %v", keysOf(f))
	}

	rec, st = BuildRecord(Response{Status: 200, Body: []byte("short")}, textSpec(), "", 1)
	if rec.Truncated {
		t.Fatalf("record %+v", rec)
	}
	f = map[string]any{}
	if err := json.Unmarshal(st.File(), &f); err != nil {
		t.Fatal(err)
	}
	if _, has := f["truncated"]; has {
		t.Errorf("a whole body's file must not carry the key: %s", st.File())
	}
}

func keysOf(m map[string]any) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestStoredFile_EncodedSizeIsBoundedForControlBytes_HashStaysOverTheWholeBody(t *testing.T) {
	// 300 KiB of 0x01: each byte is written as a 6-byte \u0001 escape, so the raw cut at 256 KiB is a
	// 1.5 MiB file. The file must be cut until its ENCODED size fits, and say so.
	body := strings.Repeat("\x01", 300<<10)
	rec, st := BuildRecord(Response{Status: 200, Body: []byte(body)}, textSpec(), "", 1)
	if rec.State != StateRecorded || !rec.Truncated {
		t.Fatalf("record %+v", rec)
	}
	file := st.File()
	if len(file) >= 1<<20 {
		t.Fatalf("the stored file is %d bytes, over the 1 MiB command-result cap", len(file))
	}
	var f map[string]any
	if err := json.Unmarshal(file, &f); err != nil || f["truncated"] != true {
		t.Fatalf("the cut file must be valid JSON and say truncated (%v)", err)
	}
	if want := PartHash(DomainBody, []byte(body)); rec.Parts.Body != want {
		t.Errorf("the body hash must be over the FULL body: %s != %s", rec.Parts.Body, want)
	}
	if rec.BodyBytes != len(body) {
		t.Errorf("body_bytes %d, want the full %d", rec.BodyBytes, len(body))
	}

	// an ordinary body of the same raw size is untouched by the encoded bound
	plain := strings.Repeat("a", StoredBodyLimit)
	rec, st = BuildRecord(Response{Status: 200, Body: []byte(plain)}, textSpec(), "", 1)
	if rec.Truncated {
		t.Errorf("an ordinary 256 KiB body must be stored whole: %+v", rec)
	}
	if !strings.Contains(string(st.File()), plain) {
		t.Errorf("the ordinary body is not whole in the file")
	}
}
