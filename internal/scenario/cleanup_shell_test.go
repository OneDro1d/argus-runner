package scenario

import (
	"strings"
	"testing"
)

// (#709) — a ```bash CLEANUP block runs under `sh -c` (dash), so bash-only syntax
// is warned about at authoring time. Warning only: the scenario stays valid.

func cleanupShellWarns(body string) []string {
	var out []string
	for _, w := range Warnings(cleanupMD(body)) {
		if strings.Contains(w, "POSIX `sh -c`") {
			out = append(out, w)
		}
	}
	return out
}

func TestCleanupShellWarning_EachBashism(t *testing.T) {
	cases := []struct{ name, body, want string }{
		{"here-string", `read -r X <<< "$(echo a)"`, "here-string"},
		{"double bracket", `if [[ -f /tmp/x ]]; then rm /tmp/x; fi`, "[[ … ]]"},
		{"array assignment", `files=(a b c)`, "array assignment"},
		{"array expansion", `echo "${files[0]}"`, "array expansion"},
		{"pattern substitution //", `echo "${NAME//a/b}"`, "pattern substitution"},
		{"pattern substitution /", `echo "${NAME/a/b}"`, "pattern substitution"},
		{"pipefail", `set -o pipefail`, "pipefail"},
		{"function keyword", "function cleanup { rm -f /tmp/x; }\ncleanup", "function"},
		{"process substitution <(", `diff <(echo a) <(echo b)`, "process substitution"},
		{"process substitution >(", `echo a > >(cat)`, "process substitution"},
		{"ansi-c string", `printf %s $'a\nb'`, "$'"},
		{"== in [ ]", `[ "$X" == "y" ] && rm /tmp/x`, "`==` inside"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			md := cleanupMD("```bash\n" + c.body + "\n```")
			w := cleanupShellWarns("```bash\n" + c.body + "\n```")
			if len(w) != 1 {
				t.Fatalf("want exactly one warning for the block; got %v", w)
			}
			if !strings.Contains(w[0], c.want) || !strings.Contains(w[0], "dash") || !strings.Contains(w[0], "block 1") {
				t.Errorf("warning must name the construct %q, the block and dash: %q", c.want, w[0])
			}
			if _, errs := Validate(md); find(errs, "CLEANUP") {
				t.Errorf("a warning must never be an error: %v", errs)
			}
		})
	}
}

func TestCleanupShellWarning_OnePerBlockListingEveryConstruct(t *testing.T) {
	w := cleanupShellWarns("```bash\nset -o pipefail\nread X <<< \"$Y\"\n```\n\n```bash\necho ok\n```\n\n```bash\nf=(a)\n```")
	if len(w) != 2 {
		t.Fatalf("one warning per offending block (blocks 1 and 3); got %v", w)
	}
	if !strings.Contains(w[0], "pipefail") || !strings.Contains(w[0], "here-string") || !strings.Contains(w[1], "block 3") {
		t.Errorf("block 1 must list both constructs, the next warning must be block 3: %v", w)
	}
}

func TestCleanupShellWarning_PosixBlockIsQuiet(t *testing.T) {
	body := "```bash\nrm -f /tmp/m_${correlation_id}\nif [ \"$X\" = y ]; then echo \"${X:-d}\" | wc -l; fi\ncleanup() { rm -f /tmp/x; }\ncleanup\nV=$(echo a)\n```"
	if w := cleanupShellWarns(body); len(w) != 0 {
		t.Fatalf("a POSIX block must not warn: %v", w)
	}
}

func TestCleanupShellWarning_SqlBlockIsQuiet(t *testing.T) {
	if w := cleanupShellWarns("```sql\nDELETE FROM t WHERE a <<< b [[ ${x//y};\n```"); len(w) != 0 {
		t.Fatalf("only bash blocks are checked: %v", w)
	}
}

func TestCleanupShellWarning_CommentsAndSingleQuotesAreQuiet(t *testing.T) {
	body := "```bash\n# read X <<< \"$Y\"; [[ a ]]; set -o pipefail\n   # f=(a)\necho 'x <<< y [[ ${a//b} ]]'\n```"
	if w := cleanupShellWarns(body); len(w) != 0 {
		t.Fatalf("constructs in a comment line or a single-quoted string must not warn: %v", w)
	}
	// ...but a real one next to them still does.
	if w := cleanupShellWarns("```bash\n# [[ a ]]\necho 'ok'\nread X <<< \"$Y\"\n```"); len(w) != 1 {
		t.Fatalf("a real here-string after a comment must still warn: %v", w)
	}
}
