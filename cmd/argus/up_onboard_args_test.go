package main

import (
	"strings"
	"testing"
)

// T2.2 — `argus up` must hand --storage-class / --results-access-mode to onboard.sh verbatim when
// given, and say nothing when not (so onboard.sh renders exactly what it rendered before). Tested on
// upOnboardArgs, the vector upFirstRun execs, because deleting the forwarding lines from up.go left
// every other test green.
func TestUpOnboardArgs_StorageFlagsForwardedOnlyWhenGiven(t *testing.T) {
	given := upOnboardArgs(upArgs{Tier: "managed", StorageClass: "nfs-rwx", ResultsAccessMode: "ReadWriteOnce"},
		"/p", "/s", "inst-1")
	joined := strings.Join(given, " ")
	for _, want := range []string{"--storage-class nfs-rwx", "--results-access-mode ReadWriteOnce", "--tier managed", "--instance-id inst-1"} {
		if !strings.Contains(joined, want) {
			t.Errorf("onboard.sh args lack %q: %q", want, joined)
		}
	}

	absent := strings.Join(upOnboardArgs(upArgs{Tier: "managed"}, "/p", "/s", "inst-1"), " ")
	for _, flag := range []string{"--storage-class", "--results-access-mode"} {
		if strings.Contains(absent, flag) {
			t.Errorf("onboard.sh args carry %s although it was not given: %q", flag, absent)
		}
	}
}
