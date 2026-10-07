package role

import "testing"

func TestParse(t *testing.T) {
	cases := []struct {
		in      string
		want    Role
		wantErr bool
	}{
		{"product", Product, false},
		{"test", Test, false},
		{"", "", true},
		{"admin", "", true},
		{"Product", "", true}, // case-sensitive on purpose
		{"PRODUCT", "", true},
	}
	for _, c := range cases {
		got, err := Parse(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("Parse(%q): want error, got %q", c.in, got)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("Parse(%q) = (%q,%v), want (%q,nil)", c.in, got, err, c.want)
		}
	}
}

// The hard dark-factory boundary: only the test hat may touch scenarios (VR-B1, UC-8).
func TestCanAccessScenarios(t *testing.T) {
	if Product.CanAccessScenarios() {
		t.Error("product MUST NOT access scenarios")
	}
	if !Test.CanAccessScenarios() {
		t.Error("test MUST access scenarios")
	}
}

// The redaction policy: product withholds expected + omits scenario; test keeps both (VR-C2/C3).
func TestRedactionPolicy(t *testing.T) {
	if !Product.MustRedactExpected() || !Product.MustOmitScenario() {
		t.Error("product MUST redact expected AND omit scenario")
	}
	if Test.MustRedactExpected() || Test.MustOmitScenario() {
		t.Error("test MUST NOT redact expected or omit scenario")
	}
}
