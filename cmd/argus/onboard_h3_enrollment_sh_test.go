package main

import (
	"os"
	"testing"
)

func readOnboardSh(t *testing.T) string {
	t.Helper()
	blob, err := os.ReadFile("../../onboarding/onboard.sh")
	if err != nil {
		t.Fatalf("read onboard.sh: %v", err)
	}
	return string(blob)
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
