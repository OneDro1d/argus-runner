package main

import (
	"flag"
	"os"
	"regexp"
	"strings"
	"testing"
)

func commonFS() *flag.FlagSet {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	(&commonFlags{}).bind(fs)
	return fs
}

// The measured case: `--scenarios-dir` on validate-config vanished, the default path was checked,
// 0 scenarios were found, and the answer was VALID.
func TestUnknownFlag_ValidateConfigRefusesAMistypedFlagAndNamesTheRealOne(t *testing.T) {
	fs := commonFS()
	common, sub := subcommandArgv("validate-config", fs, []string{"--config", "c.yaml", "--scenarios-dir", "s"})
	if err := fs.Parse(common); err != nil {
		t.Fatal(err)
	}
	msg := unknownFlagError("validate-config", fs, sub)
	if msg == "" {
		t.Fatal("--scenarios-dir was accepted by validate-config — it will be ignored and the default path validated")
	}
	for _, want := range []string{`"--scenarios-dir"`, "IGNORED", "--scenarios"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal lacks %q: %s", want, msg)
		}
	}
}

func TestUnknownFlag_LeavesWhatIsNotAMistake(t *testing.T) {
	fs := commonFS()
	for _, tc := range []struct {
		cmd string
		sub []string
	}{
		{"validate-config", nil},                              // nothing left over
		{"validate-config", []string{"positional"}},           // a positional is not a flag someone meant
		{"validate-config", []string{"--", "--anything"}},     // explicit pass-through
		{"update", []string{"plan", "--router-state", "x"}},   // owns its argv
		{"preflight", []string{"--tier", "managed"}},          // owns its argv
		{"cloud-onboarding-state", []string{"--state", "x"}},  // handed subArgs
		{"runner-id", []string{"mint", "--instance-id", "x"}}, // handed subArgs
		{"skills", []string{"--hat", "author"}},               // handed subArgs
		{"keygen", []string{"--help"}},                        // reads rest itself
	} {
		if msg := unknownFlagError(tc.cmd, fs, tc.sub); msg != "" {
			t.Errorf("%s %v was refused: %s", tc.cmd, tc.sub, msg)
		}
	}
}

// `--help` is not a common flag either, and it used to be dropped the same way (the command RAN).
// It answers with the usage — never "unknown flag", which would punish the caller asking.
func TestUnknownFlag_HelpShowsUsageInsteadOfRunningOrRefusing(t *testing.T) {
	for _, h := range []string{"--help", "-h"} {
		if !wantsHelp("validate-config", []string{h}) {
			t.Errorf("%s on validate-config is not treated as a help request", h)
		}
		if rc := dispatch([]string{"validate-config", h}); rc != 0 {
			t.Errorf("validate-config %s returned %d, want 0 (the usage)", h, rc)
		}
	}
	if wantsHelp("update", []string{"--help"}) {
		t.Error("update owns its argv — its own --help handling must not be pre-empted")
	}
}

// Through dispatch: the refusal happens before anything runs — exitUsage, not a validated default.
func TestUnknownFlag_DispatchStopsBeforeTheCommandRuns(t *testing.T) {
	if rc := dispatch([]string{"validate-config", "--config", "/nonexistent/argus-config.yaml", "--scenarios-dir", "/x"}); rc != exitUsage {
		t.Fatalf("dispatch returned %d, want exitUsage (%d)", rc, exitUsage)
	}
}

// ⛔ THE LIST CANNOT DRIFT. Every command dispatch hands subArgs (or rest) to must be in
// readsSubArgs — otherwise its own flags would be refused here before it could parse them, which is
// V19-010 in the other direction. Read from main.go itself, so a new consumer fails this test.
func TestUnknownFlag_EveryCommandHandedSubArgsIsAllowed(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	cmdRe := regexp.MustCompile(`if cmd == "([a-z-]+)"`)
	cur, found := "", 0
	for _, line := range strings.Split(string(src), "\n") {
		if m := cmdRe.FindStringSubmatch(line); m != nil {
			cur = m[1]
			continue
		}
		t2 := strings.TrimSpace(line)
		if strings.HasPrefix(t2, "//") || cur == "" {
			continue
		}
		if strings.Contains(t2, "(subArgs") || strings.Contains(t2, "subArgs,") || strings.Contains(t2, "len(rest)") {
			found++
			if !readsSubArgs(cur) {
				t.Errorf("dispatch hands subArgs/rest to %q but readsSubArgs(%q) is false — its own flags would be refused", cur, cur)
			}
		}
	}
	if found < 10 {
		t.Fatalf("found only %d subArgs consumers in main.go — the scan stopped matching and would pass vacuously", found)
	}
}
