package compare

import "testing"

func TestParsePath_Grammar(t *testing.T) {
	good := map[string]string{
		"$":                    "$",
		"$.id":                 "$.id",
		"$.items[*].createdAt": "$.items[*].createdAt",
		"$.a[0].b-c_d":         "$.a[0].b-c_d",
		"$.a.b.c.d.e.f.g.h":    "$.a.b.c.d.e.f.g.h", // exactly 8 segments
		"header:Content-Type":  "header:content-type",
		"header:ETag":          "header:etag",
	}
	for in, want := range good {
		p, err := ParsePath(in)
		if err != nil {
			t.Errorf("ParsePath(%q): unexpected error %v", in, err)
			continue
		}
		if got := p.String(); got != want {
			t.Errorf("ParsePath(%q).String() = %q, want %q", in, got, want)
		}
	}
	bad := []string{
		"", "id", "$.", "$..id", "$.a.", "$.a[", "$.a[]", "$.a[x]", "$.a[-1]", "$.a[1.5]",
		"$.a[?(@.x)]", "$..a", "$.a b", "$.*", "$[*]x", "$.a.b.c.d.e.f.g.h.i", // 9 segments
		"header:", "header:Bad Name", "header:a:b", "headers:Date", "$.a[*", "$.\"a\"",
	}
	for _, in := range bad {
		if _, err := ParsePath(in); err == nil {
			t.Errorf("ParsePath(%q): want an error, got none", in)
		}
	}
}

func TestParsePath_HeaderFlag(t *testing.T) {
	p, err := ParsePath("header:X-Req-Id")
	if err != nil {
		t.Fatal(err)
	}
	if !p.IsHeader() || p.Header() != "x-req-id" {
		t.Errorf("header path = %+v", p)
	}
	q, _ := ParsePath("$.a")
	if q.IsHeader() {
		t.Error("a body path must not be a header path")
	}
}
