package sessioninit

// sessioninit_shell_test.go — the env file is SOURCED BY BASH (the generated helper and wrapper do
// `. file`), so what this package writes and what it reads must mean to it what they mean to bash.
// Every test here runs the real bash and compares; a hand-written model of bash's quoting is how these
// drifted apart in the first place (review findings 2, 3 and 4 on #363).
//
// Token values in these tests are synthetic. A mismatch reports the case and lengths, never the value.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func needBash(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash on PATH")
	}
	return p
}

// bashValue sources file in a fresh bash and prints $name, the way the generated helper reads it.
// The working directory is a scratch dir so a value that DID execute leaves evidence there.
func bashValue(t *testing.T, bash, file, name string) (string, error) {
	t.Helper()
	cmd := exec.Command(bash, "-c", `. "$1"; printf %s "${!2}"`, "bash", file, name)
	cmd.Dir = t.TempDir()
	out, err := cmd.Output()
	return string(out), err
}

var hostileValues = []struct{ name, value string }{
	{"plain url", "https://cp.example"},
	{"command substitution", "https://cp.example/$(touch PWNED)"},
	{"backticks", "https://cp.example/`touch PWNED`"},
	{"semicolon command", "https://cp.example;touch PWNED"},
	{"ampersand", "https://cp.example/?a=1&b=2"},
	{"pipe and redirect", "a|b>c<d"},
	{"spaces", "https://cp.example/a b  c"},
	{"leading and trailing space", "  padded  "},
	{"single quote", "it's"},
	{"only a single quote", "'"},
	{"quote at both ends", "'x'"},
	{"double quote", `say "hi"`},
	{"backslash", `a\b`},
	{"backslash then n", `a\nb`},
	{"trailing backslash", `abc\`},
	{"dollar variable", "$HOME"},
	{"braced variable", "${HOME}x"},
	{"hash comment lookalike", "abc # not a comment"},
	{"leading hash", "#abc"},
	{"tilde", "~/x"},
	{"glob", "*"},
	{"history bang", "a!b"},
	{"unicode", "héllo"},
	{"equals", "a=b=c"},
	{"leading dash", "-n"},
	{"parens", "$(a)(b)"},
	{"tab inside", "a\tb"},
	{"empty", ""},
}

// Finding 2: ANY byte string written must come back literally from bash, and must not execute.
func TestEnvWriter_EveryValueRoundTripsThroughBash(t *testing.T) {
	bash := needBash(t)
	for _, tc := range hostileValues {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			p := filepath.Join(dir, "tester.env")
			if _, err := ensureEnvLines(p, "# h\n", []envLine{{"ARGUS_CP_URL", tc.value}, {"ARGUS_TESTER_TOKEN_X", ""}}); err != nil {
				t.Fatalf("writer refused a value it must quote: %v", err)
			}
			got, err := bashValue(t, bash, p, "ARGUS_CP_URL")
			if err != nil {
				t.Fatalf("bash failed to source the file: %v", err)
			}
			if got != tc.value {
				t.Fatalf("bash read %d bytes, wrote %d: the value did not round-trip ", len(got), len(tc.value))
			}
			if _, err := os.Stat(filepath.Join(dir, "PWNED")); err == nil {
				t.Fatal("the value executed")
			}
			// and ReadToken, which decides what builder init verifies, must agree with what bash sent
			rt, err := ReadToken(p, "ARGUS_CP_URL")
			if err != nil {
				t.Fatalf("ReadToken on a file this package wrote: %v", err)
			}
			if rt != tc.value {
				t.Fatalf("ReadToken returned %d bytes, bash %d: they disagree", len(rt), len(got))
			}
		})
	}
}

// Through Init itself: --control-plane / $ARGUS_CP_URL is the hostile input in practice.
func TestInit_HostileControlPlaneRoundTripsOrIsRefused(t *testing.T) {
	bash := needBash(t)
	for _, tc := range hostileValues {
		if tc.value == "" || strings.TrimSpace(tc.value) != tc.value || strings.HasSuffix(tc.value, "/") {
			continue // Init trims whitespace and trailing slashes by design
		}
		t.Run(tc.name, func(t *testing.T) {
			o, _ := testerOpts(t)
			o.ControlPlane = tc.value
			res, err := Init(o)
			if err != nil {
				t.Fatalf("Init refused: %v", err)
			}
			got, err := bashValue(t, bash, res.EnvFile, "ARGUS_CP_URL")
			if err != nil || got != tc.value {
				t.Fatalf("bash read %d bytes (err %v), want %d", len(got), err, len(tc.value))
			}
		})
	}
}

// A value a one-line env file cannot carry is refused with a clear error, not half-written.
func TestEnvWriter_RefusesLineBreaksAndNUL(t *testing.T) {
	for name, v := range map[string]string{"newline": "a\nb", "carriage return": "a\rb", "NUL": "a\x00b", "newline then command": "x\ntouch PWNED"} {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "tester.env")
			_, err := ensureEnvLines(p, "# h\n", []envLine{{"ARGUS_CP_URL", v}})
			if err == nil || !strings.Contains(err.Error(), "ARGUS_CP_URL") {
				t.Fatalf("err = %v, want a refusal naming the variable", err)
			}
			if _, serr := os.Stat(p); serr == nil {
				t.Fatal("a file was left behind by a refused write")
			}
		})
	}
}

// Finding 3: ReadToken must parse like bash for what a human edits by hand. Compared against real bash.
func TestReadToken_AgreesWithBashOnHandEditedForms(t *testing.T) {
	bash := needBash(t)
	for _, line := range []string{
		`T="abc" # note`,
		`T='abc' # note`,
		`T=abc # note`,
		`T=abc	# tab before the note`,
		`T="abc"#joined`,
		`T=abc#inside`,
		`T="abc"`,
		`T='abc'`,
		`T="a b"`,
		`T='a b'   `,
		`T="a \"q\" b"`,
		`T="a\\b"`,
		`T="a\nb"`,
		`T="a\zb"`,
		`T='a\nb'`,
		`T='a'"b"c`,
		`T=a\ b`,
		`T=a\#b`,
		`T=`,
		`T=""`,
		`T=''`,
		`T=  # only a note`,
		`export T="x y" # note`,
		`   T=indented`,
		`T="has # hash inside"`,
		`T='has # hash inside' # and a note`,
		`T=odts_synthetic.value-1_2`,
	} {
		t.Run(line, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "x.env")
			if err := os.WriteFile(p, []byte(line+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			want, err := bashValue(t, bash, p, "T")
			if err != nil {
				t.Fatalf("bash rejected the fixture: %v", err)
			}
			got, err := ReadToken(p, "T")
			if err != nil {
				t.Fatalf("ReadToken: %v (bash read %d bytes)", err, len(want))
			}
			if got != want {
				t.Fatalf("ReadToken returned %d bytes, bash %d: builder init would verify a different token than the helper sends ", len(got), len(want))
			}
		})
	}
}

// A line whose value bash would EXPAND or run cannot be read without running bash: refuse it loudly
// rather than return something the helper would not send.
func TestReadToken_RefusesWhatOnlyBashCanEvaluate(t *testing.T) {
	for _, line := range []string{
		`T=$HOME`,
		`T="$HOME"`,
		"T=`id`",
		`T="$(id)"`,
		`T=~/x`,
		`T=abc;id`,
		`T=abc&`,
		`T=abc def`,
		`T=(a b)`,
		`T="unterminated`,
		`T='unterminated`,
	} {
		t.Run(line, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "x.env")
			if err := os.WriteFile(p, []byte(line+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := ReadToken(p, "T")
			if err == nil {
				t.Fatalf("ReadToken returned %d bytes with no error for a line bash evaluates differently", len(got))
			}
		})
	}
}

// Finding 4: nothing is written THROUGH a symlink.
func TestInit_RefusesToWriteThroughASymlink(t *testing.T) {
	link := func(t *testing.T, path, victim string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(victim, path); err != nil {
			t.Skipf("no symlinks here: %v", err)
		}
	}
	for _, tc := range []struct {
		name string
		path func(o Options) string
		kind Kind
	}{
		{"tester env file", func(o Options) string { return filepath.Join(o.Home, ".config", "argus", "tester.env") }, Tester},
		{"tester helper", func(o Options) string { return filepath.Join(o.Dir, "argus-headers-msgbus.sh") }, Tester},
		{"tester wrapper", func(o Options) string { return filepath.Join(o.Dir, "argus-msgbus") }, Tester},
		{"builder env file", func(o Options) string { return filepath.Join(o.Dir, BuilderStateDir, "builder.env") }, Builder},
		{"builder helper", func(o Options) string {
			return filepath.Join(o.Dir, BuilderStateDir, "argus-headers-msgbus-builder.sh")
		}, Builder},
		{"builder gitignore", func(o Options) string { return filepath.Join(o.Dir, ".gitignore") }, Builder},
	} {
		for _, dangling := range []bool{false, true} {
			name := tc.name
			if dangling {
				name += " (dangling)"
			}
			t.Run(name, func(t *testing.T) {
				o, _ := testerOpts(t)
				o.Kind = tc.kind
				o.App = "msgbus"
				victimDir := t.TempDir()
				victim := filepath.Join(victimDir, "victim")
				const before = "VICTIM-CONTENT\n"
				if !dangling {
					if err := os.WriteFile(victim, []byte(before), 0o644); err != nil {
						t.Fatal(err)
					}
					if err := os.Chmod(victim, 0o644); err != nil { // the umask may have narrowed it
						t.Fatal(err)
					}
				}
				link(t, tc.path(o), victim)
				_, err := Init(o)
				if err == nil || !strings.Contains(err.Error(), "symbolic link") {
					t.Fatalf("Init err = %v, want a refusal naming the symlink", err)
				}
				if dangling {
					if _, serr := os.Lstat(victim); serr == nil {
						t.Fatal("a write went through the dangling symlink and created its target")
					}
					return
				}
				got, _ := os.ReadFile(victim)
				if string(got) != before {
					t.Fatalf("the symlink target was modified (%d bytes now)", len(got))
				}
				if st, _ := os.Stat(victim); st.Mode().Perm() != 0o644 {
					t.Fatalf("the symlink target's mode was changed to %o", st.Mode().Perm())
				}
			})
		}
	}
}
