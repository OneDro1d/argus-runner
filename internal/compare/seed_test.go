package compare

// seed_test.go -- ARGUS-CMP-7: the seed rule at creation, pure. (A separate file: it names
// SeedRefusal, IsSeed and HasMeasured, which do not exist before this change.)

import (
	"strings"
	"testing"
)

func TestIsSeedIsTheExactTag(t *testing.T) {
	for tags, want := range map[string]bool{"seed": true, "smoke,seed": true, "seeded": false, "Seed": false, "seed-1": false, "": false} {
		var ts []string
		if tags != "" {
			ts = strings.Split(tags, ",")
		}
		if got := IsSeed(ts); got != want {
			t.Errorf("IsSeed(%q) = %v, want %v", tags, got, want)
		}
	}
}

func TestSeedRefusal(t *testing.T) {
	ck := func(id string, seed, compared bool) SetCheck { return SetCheck{ID: id, Seed: seed, Compared: compared} }
	cases := []struct {
		name string
		set  []SetCheck
		want []string // nil = accepted
	}{
		{"no seed at all", []SetCheck{ck("B", false, true), ck("A", false, true)}, nil},
		{"seed first", []SetCheck{ck("00-S", true, false), ck("A", false, true)}, nil},
		{"two seeds first, byte order", []SetCheck{ck("A", false, true), ck("00-S1", true, false), ck("00-S2", true, false)}, nil},
		{"seed after a check", []SetCheck{ck("A", false, true), ck("Z-S", true, false)}, []string{"Z-S", "A", "sorts after", "rename"}},
		{"names the lowest check and the lowest late seed", []SetCheck{ck("M", false, true), ck("B", false, true), ck("C-S", true, false), ck("Z-S", true, false), ck("0-S", true, false)}, []string{"C-S", "B"}},
		{"byte order, not case folding: lower-case sorts after upper-case", []SetCheck{ck("B", false, true), ck("a-seed", true, false)}, []string{"a-seed", "B"}},
		{"only seeds", []SetCheck{ck("0-S", true, true), ck("1-S", true, true)}, []string{"every check", "seed"}},
		{"only the seed declares ## COMPARE", []SetCheck{ck("0-S", true, true), ck("A", false, false)}, []string{"## COMPARE", "seed"}},
	}
	for _, c := range cases {
		got := SeedRefusal(c.set)
		if c.want == nil {
			if got != "" {
				t.Errorf("%s: refused: %q", c.name, got)
			}
			continue
		}
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: %q does not contain %q", c.name, got, w)
			}
		}
	}
}

func TestHasMeasured(t *testing.T) {
	m, f := &Rules{Reference: RefMeasured}, &Rules{Reference: RefFixed}
	if HasMeasured(nil) || HasMeasured([]*Rules{nil, f}) || !HasMeasured([]*Rules{f, nil, m}) {
		t.Error("HasMeasured is wrong")
	}
}
