package compare

import "testing"

// A row whose values are an empty (non-nil) list contributes nothing either: the root is the one of the same rows without the field.
func TestCMP11_F5_AnEmptyValuesListContributesNothingToTheRoot(t *testing.T) {
	plain := f5Plain()
	empty := f5Plain()
	for i := range empty {
		empty[i].Values = []ToleranceValue{}
	}
	if OutputsRoot(plain) != OutputsRoot(empty) {
		t.Errorf("an empty values list moved the root")
	}
}
