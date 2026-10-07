package envcapture

import "testing"

func TestCompareCaptures_Match(t *testing.T) {
	a := Capture{Captured: true, Fingerprint: "abc"}
	b := Capture{Captured: true, Fingerprint: "abc"}
	same, note := CompareCaptures(a, b)
	if !same {
		t.Errorf("same = false, note %q — want true on identical fingerprints", note)
	}
}

func TestCompareCaptures_Mismatch(t *testing.T) {
	a := Capture{Captured: true, Fingerprint: "abc"}
	b := Capture{Captured: true, Fingerprint: "xyz"}
	same, note := CompareCaptures(a, b)
	if same {
		t.Fatal("same = true on DIFFERENT fingerprints")
	}
	if note == "" {
		t.Error("no note given for a fingerprint mismatch")
	}
}

// TestCompareCaptures_MissingCaptureNeverReadsAsMatch: the one rule this whole feature exists for
// — a run that could not capture its environment must NEVER be treated as "the same" as another
// run, no matter what either side's zero-value fields happen to look like.
func TestCompareCaptures_MissingCaptureNeverReadsAsMatch(t *testing.T) {
	cases := []struct {
		name string
		a, b Capture
	}{
		{"both missing, both zero-value", Capture{}, Capture{}},
		{"one missing, one real", Capture{Captured: false, Reason: "forbidden: pods in namespace x"}, Capture{Captured: true, Fingerprint: "abc"}},
		{"both missing, same reason", Capture{Reason: "no SUT namespace declared"}, Capture{Reason: "no SUT namespace declared"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			same, note := CompareCaptures(c.a, c.b)
			if same {
				t.Errorf("same = true; want false whenever either side never captured (%+v vs %+v)", c.a, c.b)
			}
			if note == "" {
				t.Error("no note given for a missing-capture comparison")
			}
		})
	}
}
