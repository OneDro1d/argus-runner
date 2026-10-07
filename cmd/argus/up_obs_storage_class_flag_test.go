package main

import (
	"strings"
	"testing"
)

// , defect 2 of the review of PR 546: registering --obs-storage-class on `argus up` but binding
// it to a throwaway variable left every test green, because the existing forwarding test builds an upArgs by
// hand. This one goes through the REAL flag registration (upFlagSet, the function cmdUp itself parses with)
// and on to the onboard.sh argument vector upFirstRun execs.
func TestUpFlags_ObsStorageClassParsedIntoUpArgs(t *testing.T) {
	var a upArgs
	if err := upFlagSet(&a).Parse([]string{"--tier", "aks", "--obs-storage-class", "fast-ssd", "--storage-class", "my-rwx"}); err != nil {
		t.Fatal(err)
	}
	if a.ObsStorageClass != "fast-ssd" || a.StorageClass != "my-rwx" {
		t.Fatalf("parsed ObsStorageClass=%q StorageClass=%q, want fast-ssd / my-rwx (each flag in its own field)", a.ObsStorageClass, a.StorageClass)
	}
	argv := strings.Join(upOnboardArgs(a, "/p", "/s", "inst-1"), " ")
	if !strings.Contains(argv, "--obs-storage-class fast-ssd") || !strings.Contains(argv, "--storage-class my-rwx") {
		t.Errorf("onboard.sh argv lacks the parsed class flags: %q", argv)
	}

	var none upArgs
	if err := upFlagSet(&none).Parse([]string{"--tier", "aks"}); err != nil {
		t.Fatal(err)
	}
	if none.ObsStorageClass != "" || strings.Contains(strings.Join(upOnboardArgs(none, "/p", "/s", "i"), " "), "--obs-storage-class") {
		t.Errorf("no flag given, yet ObsStorageClass=%q / forwarded", none.ObsStorageClass)
	}
}
