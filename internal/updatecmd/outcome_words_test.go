package updatecmd

import "testing"

// AC-D58 (#392): the unrecorded words are never read as success by anything that matches `updated` or
// `rolled-back-to ` — the shell case in finish(), `argus up`, the page.
func TestOutcomeWords_UnrecordedIsNeverSuccess(t *testing.T) {
	for word, c := range map[string]struct{ unrecorded, attention bool }{
		"updated":                          {false, false},
		"rolled-back-to 0.3.31":            {false, false},
		"untouched":                        {false, false},
		"rolled-back":                      {false, true},
		"rollback-failed":                  {false, true},
		"rollback-unreachable":             {false, true}, // AC-D53: never unrecorded, never success
		"unrecorded-updated":               {true, true},
		"unrecorded-rolled-back-to 0.3.31": {true, true},
		"some-word-from-a-newer-release":   {false, true},
	} {
		if got := OutcomeUnrecorded(word); got != c.unrecorded {
			t.Errorf("OutcomeUnrecorded(%q) = %v, want %v", word, got, c.unrecorded)
		}
		if got := OutcomeNeedsAttention(word); got != c.attention {
			t.Errorf("OutcomeNeedsAttention(%q) = %v, want %v", word, got, c.attention)
		}
	}
}
