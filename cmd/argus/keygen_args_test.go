package main

import (
	"encoding/base64"
	"strings"
	"testing"
)

// looksLikeKey reports whether any line of out is a base64 Ed25519 private key (64 bytes), the shape
// `keygen` prints. The tests below never echo out: a key printed into a test log is still a key.
func looksLikeKey(out string) bool {
	for _, line := range strings.Split(out, "\n") {
		b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(line))
		if err == nil && len(b) == 64 {
			return true
		}
	}
	return false
}

// `keygen` takes no arguments. Asking it for help must print help and mint nothing: the caller only
// wanted to read the usage, and a key printed to a terminal counts as exposed.
func TestKeygen_HelpPrintsUsageAndNoKey(t *testing.T) {
	for _, h := range []string{"--help", "-h", "help"} {
		var code int
		out := captureStdout(t, func() { code = dispatch([]string{"keygen", h}) })
		if code != 0 {
			t.Errorf("keygen %s: exit %d, want 0", h, code)
		}
		if looksLikeKey(out) {
			t.Errorf("keygen %s generated and printed a key (output withheld)", h)
		} else if !strings.Contains(out, "usage: argus keygen") {
			t.Errorf("keygen %s: no usage line.\ngot: %s", h, out)
		}
	}
}

func TestKeygen_UnknownArgumentIsRefusedAndMintsNothing(t *testing.T) {
	var code int
	out := captureStdout(t, func() { code = dispatch([]string{"keygen", "--out", "/tmp/x"}) })
	if code != exitUsage {
		t.Errorf("exit %d, want %d (usage)", code, exitUsage)
	}
	if looksLikeKey(out) {
		t.Errorf("an unknown argument still generated and printed a key (output withheld)")
	}
}

func TestKeygen_BarePrintsOneKey(t *testing.T) {
	var code int
	out := captureStdout(t, func() { code = dispatch([]string{"keygen"}) })
	if code != 0 || !looksLikeKey(out) {
		t.Fatalf("bare keygen: exit %d, and its output is not one base64 Ed25519 key (output withheld)", code)
	}
}
