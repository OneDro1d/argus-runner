package scenario

import "testing"

// AC-D16 — the "broker refuses with <code>" grammar.
func TestParseRefusal(t *testing.T) {
	for _, c := range []struct {
		name     string
		expect   []string
		wantCode int
		wantOK   bool
	}{
		{"bare numeric code", []string{"broker refuses with 406"}, 406, true},
		{"PRECONDITION_FAILED name", []string{"broker refuses with PRECONDITION_FAILED"}, 406, true},
		{"ACCESS_REFUSED name", []string{"broker refuses with ACCESS_REFUSED"}, 403, true},
		{"NOT_FOUND name", []string{"broker refuses with NOT_FOUND"}, 404, true},
		{"RESOURCE_LOCKED name", []string{"broker refuses with RESOURCE_LOCKED"}, 405, true},
		{"case-insensitive name", []string{"broker refuses with precondition_failed"}, 406, true},
		{"the publish, explicitly", []string{"broker refuses the publish with 403"}, 403, true},
		{"one clause of a compound bullet", []string{"status=202, broker refuses with 406"}, 406, true},
		{"nothing declared", []string{"event == OrderCreated"}, 0, false},
		{"no bullets at all", nil, 0, false},
		{"mentions refusal mid-prose — not anchored, so it does not match", []string{"the malformed request is rejected before the broker refuses with anything"}, 0, false},
		{"an unknown name is not accepted", []string{"broker refuses with TEAPOT"}, 0, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			code, ok := ParseRefusal(c.expect)
			if ok != c.wantOK || code != c.wantCode {
				t.Errorf("ParseRefusal(%v) = (%d, %v), want (%d, %v)", c.expect, code, ok, c.wantCode, c.wantOK)
			}
		})
	}
}

// RefusalCodeNames is the closed set the grammar accepts — pinned so a silent addition/removal
// is caught here rather than discovered at runtime.
func TestRefusalCodeNames_PinnedSet(t *testing.T) {
	want := map[string]int{
		"ACCESS_REFUSED":      403,
		"NOT_FOUND":           404,
		"RESOURCE_LOCKED":     405,
		"PRECONDITION_FAILED": 406,
	}
	if len(RefusalCodeNames) != len(want) {
		t.Fatalf("RefusalCodeNames has %d entries, want %d", len(RefusalCodeNames), len(want))
	}
	for name, code := range want {
		if RefusalCodeNames[name] != code {
			t.Errorf("RefusalCodeNames[%q] = %d, want %d", name, RefusalCodeNames[name], code)
		}
	}
}
