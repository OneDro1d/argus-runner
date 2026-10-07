// Package sessioninit is the logic behind `argus tester init <app>` and `argus builder init <app>`
// (msgbus onboarding review, items 18 and 19): it writes the credential plumbing a Claude Code session
// needs on a machine whose sessions do NOT source ~/.bashrc, so the token reaches the MCP client and the
// CLI without ever passing through a shell profile, argv, or this program's output.
//
// The pattern is the one docs/runbooks/TESTER-SESSION-PROMPT.md (phase 3) documents and that worked on
// Coder: ONE env file holds a token per app; a `headersHelper` script prints the Authorization header the
// MCP client consumes, reading the file at CALL time; a CLI wrapper does the same for `argus`; `.mcp.json`
// names the helper. Nothing is generated here that phase 3 does not already describe, except the builder
// variant (runner scope, its own file, its own wrapper that can never carry an author credential).
//
// ⛔ A token VALUE never enters a message, a log or an error from this package. `Init` (both kinds) only
// ever asks an env file whether a variable is empty. Only ReadToken hands a value back — for the builder
// verification and doctor, which need it as a bearer and nowhere else.
package sessioninit

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/OneDro1d/argus-runner/internal/agentcfg"
	"github.com/OneDro1d/argus-runner/internal/buildinfo"
)

// Kind is which session the plumbing is for.
type Kind string

const (
	Tester  Kind = "tester"
	Builder Kind = "builder"
)

// The control plane the runbook points testers at is buildinfo.DefaultControlPlane(): one build-time
// setting, not a literal here.

// MCPKey is the server key written into .mcp.json (the runbook's).
const MCPKey = "argus"

// BuilderStateDir is the directory inside a builder's notepad that holds its token file, helper and wrapper.
const BuilderStateDir = ".argus-builder"

var appRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9._ -]*$`)

// AppNames returns the upper-cased variable form (letters and digits kept, anything else `_`) and the
// lower-cased file-name form (anything but letters/digits `-`) of an app name.
func AppNames(app string) (upper, lower string, err error) {
	app = strings.TrimSpace(app)
	if !appRe.MatchString(app) {
		return "", "", fmt.Errorf("app name %q: want a letter first, then letters, digits, '.', '-', '_' or space", app)
	}
	clean := func(r rune, sub rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			return r
		}
		return sub
	}
	upper = strings.ToUpper(strings.Map(func(r rune) rune { return clean(r, '_') }, app))
	lower = strings.ToLower(strings.Map(func(r rune) rune { return clean(r, '-') }, app))
	return upper, lower, nil
}

// Options drive Init.
type Options struct {
	Kind Kind
	App  string
	// Home is the home directory the tester's ~/.config/argus lives in (Tester only).
	Home string
	// Dir: Tester — where the helper and wrapper go. Builder — the builder's notepad directory; everything
	// goes under Dir/.argus-builder.
	Dir string
	// ControlPlane is written as ARGUS_CP_URL (only if the env file has none) and into the .mcp.json url.
	ControlPlane string
	// MCPJSON is the .mcp.json to merge the entry into. "" = do not touch one (the builder path wires it
	// separately, after the token is verified).
	MCPJSON string
}

// Result reports what Init did. It carries paths and booleans, never a value.
type Result struct {
	Kind     Kind
	App      string
	TokenVar string
	EnvFile  string
	Helper   string
	Wrapper  string
	MCPJSON  string
	// TokenSet is whether TokenVar already has a non-empty value in EnvFile.
	TokenSet bool
	// Changed lists what this run wrote; an idempotent re-run leaves it empty.
	Changed []string
	Control string
}

// Init writes the plumbing. Idempotent: an existing non-empty token is never touched, an existing file
// with the same content is left alone.
func Init(o Options) (*Result, error) {
	up, low, err := AppNames(o.App)
	if err != nil {
		return nil, err
	}
	given := strings.TrimRight(strings.TrimSpace(o.ControlPlane), "/")
	cp := given
	if cp == "" {
		cp = buildinfo.DefaultControlPlane()
	}
	res := &Result{Kind: o.Kind, App: o.App}
	var envDir, header string
	switch o.Kind {
	case Tester:
		if o.Home == "" {
			return nil, errors.New("sessioninit: no home directory")
		}
		envDir = filepath.Join(o.Home, ".config", "argus")
		res.EnvFile = filepath.Join(envDir, "tester.env")
		res.TokenVar = "ARGUS_TESTER_TOKEN_" + up
		res.Helper = filepath.Join(o.Dir, "argus-headers-"+low+".sh")
		res.Wrapper = filepath.Join(o.Dir, "argus-"+low)
		header = "# Argus tester credentials: ONE token per app, one variable each. Mode 600. Never commit it,\n" +
			"# never source it from ~/.bashrc (that would put every token into every session's shell).\n"
	case Builder:
		if o.Dir == "" {
			return nil, errors.New("sessioninit: no notepad directory")
		}
		envDir = filepath.Join(o.Dir, BuilderStateDir)
		res.EnvFile = filepath.Join(envDir, "builder.env")
		res.TokenVar = "ARGUS_BUILDER_TOKEN_" + up
		res.Helper = filepath.Join(envDir, "argus-headers-"+low+"-builder.sh")
		res.Wrapper = filepath.Join(envDir, "argus-"+low+"-builder")
		header = "# Argus BUILDER (runner scope) token for this notepad. Mode 600. Never commit it (see .gitignore).\n" +
			"# A builder token is runner scope only: if it can see author tools, it is the wrong token.\n"
		for _, p := range []string{res.EnvFile, envDir} {
			if strings.ContainsAny(p, "'\n") {
				return nil, fmt.Errorf("sessioninit: path %q contains a quote or newline", p)
			}
		}
	default:
		return nil, fmt.Errorf("sessioninit: unknown kind %q", o.Kind)
	}

	if err := os.MkdirAll(envDir, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(envDir, 0o700); err != nil {
		return nil, err
	}
	changed, err := ensureEnvLines(res.EnvFile, header, []envLine{{"ARGUS_CP_URL", cp}, {res.TokenVar, ""}})
	if err != nil {
		return nil, err
	}
	res.Changed = append(res.Changed, changed...)
	// The control plane the .mcp.json names: an explicit one, else the env file's own ARGUS_CP_URL (which an
	// earlier init or the human set), else the default. The file's line is never overwritten by this.
	if given == "" {
		if fromFile, _ := ReadToken(res.EnvFile, "ARGUS_CP_URL"); strings.TrimSpace(fromFile) != "" {
			cp = strings.TrimRight(strings.TrimSpace(fromFile), "/")
		}
	}
	res.Control = cp
	if res.TokenSet, err = TokenSet(res.EnvFile, res.TokenVar); err != nil {
		return nil, err
	}

	if err := os.MkdirAll(filepath.Dir(res.Helper), 0o700); err != nil {
		return nil, err
	}
	var helper, wrapper string
	if o.Kind == Tester {
		helper = testerHelper(up)
		wrapper = testerWrapper(up)
	} else {
		helper = builderHelper(up, res.EnvFile)
		wrapper = builderWrapper(up, res.EnvFile, filepath.Join(envDir, "no-author-session.json"))
	}
	for _, f := range []struct{ path, body string }{{res.Helper, helper}, {res.Wrapper, wrapper}} {
		ch, err := writeExecutable(f.path, f.body)
		if err != nil {
			return nil, err
		}
		if ch {
			res.Changed = append(res.Changed, f.path)
		}
	}
	if o.Kind == Builder {
		ch, err := ensureGitignore(o.Dir, BuilderStateDir+"/")
		if err != nil {
			return nil, err
		}
		if ch {
			res.Changed = append(res.Changed, filepath.Join(o.Dir, ".gitignore"))
		}
	}
	if o.MCPJSON != "" {
		ch, err := WireMCP(o.MCPJSON, res.Helper, cp)
		if err != nil {
			return nil, err
		}
		res.MCPJSON = o.MCPJSON
		if ch {
			res.Changed = append(res.Changed, o.MCPJSON)
		}
	}
	return res, nil
}

// WireMCP merges the `argus` server entry (url + headersHelper, no token) into a .mcp.json, keeping every
// other byte of the file. It reports whether the file changed.
func WireMCP(mcpPath, helper, controlPlane string) (bool, error) {
	entry, _ := json.Marshal(map[string]string{
		"type": "http", "url": strings.TrimRight(controlPlane, "/") + "/mcp", "headersHelper": helper,
	})
	before, _ := os.ReadFile(mcpPath)
	if _, err := agentcfg.Merge(mcpPath, MCPKey, entry); err != nil {
		return false, fmt.Errorf("%s: %w", mcpPath, err)
	}
	after, _ := os.ReadFile(mcpPath)
	return string(before) != string(after), nil
}

// UnwireMCP removes the `argus` entry (used when a builder token fails verification, so no stale entry
// keeps pointing a session at a bad token). A missing file or entry is fine.
func UnwireMCP(mcpPath string) (bool, error) {
	removed, _, err := agentcfg.Remove(mcpPath, MCPKey, false)
	return removed, err
}

func testerHelper(up string) string {
	return "#!/usr/bin/env bash\nset -euo pipefail\nset -a\n. ~/.config/argus/tester.env\nset +a\n" +
		"printf '{\"Authorization\":\"Bearer %s\"}\\n' \"$ARGUS_TESTER_TOKEN_" + up + "\"\n"
}

func testerWrapper(up string) string {
	return "#!/usr/bin/env bash\nset -euo pipefail\nset -a\n. ~/.config/argus/tester.env\nset +a\n" +
		"export ARGUS_CP_AUTHOR_TOKEN=\"$ARGUS_TESTER_TOKEN_" + up + "\"\n" +
		"export ARGUS_CP_TOKEN=\"$ARGUS_TESTER_TOKEN_" + up + "\"\n" +
		"exec argus \"$@\"\n"
}

func builderHelper(up, envFile string) string {
	return "#!/usr/bin/env bash\nset -euo pipefail\nset -a\n. '" + envFile + "'\nset +a\n" +
		"printf '{\"Authorization\":\"Bearer %s\"}\\n' \"$ARGUS_BUILDER_TOKEN_" + up + "\"\n"
}

// builderWrapper hands the CLI the builder token as the RUNNER hat and nothing else: every variable that
// could carry an author credential is unset first, and the session-file fallback (a shared author login)
// is pointed at a file that does not exist.
func builderWrapper(up, envFile, noSession string) string {
	return "#!/usr/bin/env bash\nset -euo pipefail\n" +
		"unset ARGUS_CP_AUTHOR_TOKEN ARGUS_CP_TOKEN ARGUS_EXECUTOR_SECRET ARGUS_AUTHOR_TOKEN ARGUS_TOKEN ARGUS_RUNNER_TOKEN\n" +
		"set -a\n. '" + envFile + "'\nset +a\n" +
		"export ARGUS_RUNNER_TOKEN=\"$ARGUS_BUILDER_TOKEN_" + up + "\"\n" +
		"export ARGUS_TOKEN=\"$ARGUS_BUILDER_TOKEN_" + up + "\"\n" +
		"export ARGUS_SESSION_FILE='" + noSession + "'\n" +
		"exec argus \"$@\"\n"
}

// refuseSymlink errors when path is a symlink (dangling or not). Every file this package writes lives in a
// directory the user may share with something else (a notepad checkout, ~/.config), and opening or
// chmod-ing through a planted link would write a credential file's contents, or change a mode, somewhere
// else entirely. A missing path is fine: the write creates it.
func refuseSymlink(path string) error {
	st, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if st.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("sessioninit: %s is a symbolic link; refusing to write through it (remove the link, then re-run)", path)
	}
	return nil
}

// writeExecutable writes body at mode 0700 unless the file already holds exactly it.
func writeExecutable(path, body string) (bool, error) {
	if err := refuseSymlink(path); err != nil {
		return false, err
	}
	if cur, err := os.ReadFile(path); err == nil && string(cur) == body {
		return false, os.Chmod(path, 0o700)
	}
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		return false, err
	}
	return true, os.Chmod(path, 0o700)
}

func ensureGitignore(dir, line string) (bool, error) {
	p := filepath.Join(dir, ".gitignore")
	if err := refuseSymlink(p); err != nil {
		return false, err
	}
	cur, err := os.ReadFile(p)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	for _, l := range strings.Split(string(cur), "\n") {
		if strings.TrimSpace(l) == line || strings.TrimSpace(l) == strings.TrimSuffix(line, "/") {
			return false, nil
		}
	}
	add := line + "\n"
	if len(cur) > 0 && !strings.HasSuffix(string(cur), "\n") {
		add = "\n" + add
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return false, err
	}
	defer f.Close()
	_, err = f.WriteString(add)
	return true, err
}

type envLine struct{ name, value string }

// ensureEnvLines appends `NAME=value` for every name the file does not define yet (an empty definition
// counts as defined), creating the file at 0600 with header when absent, and tightens the mode to 0600.
// It never rewrites or removes a line, so an existing value is never touched.
func ensureEnvLines(path, header string, want []envLine) ([]string, error) {
	if err := refuseSymlink(path); err != nil {
		return nil, err
	}
	have := map[string]bool{}
	existed := true
	if err := scanEnv(path, func(name, _ string) { have[name] = true }); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		existed = false
	}
	var add strings.Builder
	var changed []string
	for _, w := range want {
		if !have[w.name] {
			q, err := quoteEnvValue(w.name, w.value)
			if err != nil {
				return nil, err
			}
			add.WriteString(w.name + "=" + q + "\n")
		}
	}
	if !existed {
		changed = append(changed, path)
	}
	if add.Len() > 0 || !existed {
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		out := add.String()
		if !existed {
			out = header + out
		} else if st, err := f.Stat(); err == nil && st.Size() > 0 {
			last := make([]byte, 1)
			if _, err := f.ReadAt(last, st.Size()-1); err == nil && last[0] != '\n' {
				out = "\n" + out
			}
		}
		if _, err := f.WriteString(out); err != nil {
			return nil, err
		}
		if existed {
			changed = append(changed, path)
		}
	}
	return changed, os.Chmod(path, 0o600)
}

// envSafe is the set of characters bash reads literally in an unquoted assignment value.
var envSafe = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

// quoteEnvValue renders v so that bash, sourcing the file, assigns exactly v: unchanged when it is made
// only of characters bash reads literally, else single-quoted with each embedded quote closed, escaped and reopened. The env file is
// SOURCED by the generated helper and wrapper, so an unquoted `$(...)`, backtick, `;`, `&` or space in a
// --control-plane or $ARGUS_CP_URL would run or split (review finding 2). A line break or NUL cannot be
// carried by a one-line file and is refused, naming the variable and never the value.
func quoteEnvValue(name, v string) (string, error) {
	if strings.ContainsAny(v, "\n\r\x00") {
		return "", fmt.Errorf("sessioninit: %s: the value contains a line break or NUL, which a one-line env file cannot hold", name)
	}
	if v == "" || envSafe.MatchString(v) {
		return v, nil
	}
	return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'", nil
}

// parseBashValue reads the right-hand side of NAME=<raw> the way bash does when it sources the line:
// single quotes are literal, double quotes honour only \$ \` \" \\, a backslash outside quotes escapes the
// next character, adjacent pieces concatenate, and the word ends at the first unquoted blank, after which
// only a `# comment` may follow.
//
// strict refuses what cannot be evaluated without running bash (an unescaped $ or backtick, a leading or
// :-following ~, an unquoted ; & | < > ( ), text after the word, an unterminated quote): the error names
// the problem and never the value. Lenient returns the best literal reading, which is enough for callers
// that only ask whether a value is empty.
func parseBashValue(raw string, strict bool) (string, error) {
	var b strings.Builder
	fail := func(why string) (string, error) {
		if strict {
			return "", errors.New(why)
		}
		return b.String(), nil
	}
	n := len(raw)
	for i := 0; i < n; {
		c := raw[i]
		switch {
		case c == '\'':
			j := strings.IndexByte(raw[i+1:], '\'')
			if j < 0 {
				return fail("an unterminated single quote")
			}
			b.WriteString(raw[i+1 : i+1+j])
			i += j + 2
		case c == '"':
			i++
			closed := false
			for i < n {
				d := raw[i]
				if d == '"' {
					closed = true
					i++
					break
				}
				if d == '\\' && i+1 < n && strings.IndexByte("$`\"\\", raw[i+1]) >= 0 {
					b.WriteByte(raw[i+1])
					i += 2
					continue
				}
				if (d == '$' || d == '`') && strict {
					return fail("a $ or backtick inside double quotes, which bash expands")
				}
				b.WriteByte(d)
				i++
			}
			if !closed {
				return fail("an unterminated double quote")
			}
		case c == '\\':
			if i+1 >= n {
				return fail("a trailing backslash, which bash reads as a line continuation")
			}
			b.WriteByte(raw[i+1])
			i += 2
		case c == ' ' || c == '\t':
			rest := strings.TrimLeft(raw[i:], " \t")
			if rest == "" || rest[0] == '#' {
				return b.String(), nil
			}
			return fail("text after the value, which bash runs as a command")
		case strings.IndexByte("$`;&|<>()", c) >= 0:
			if strict {
				return fail("an unquoted shell metacharacter, which bash expands or runs")
			}
			b.WriteByte(c)
			i++
		case c == '~' && (i == 0 || raw[i-1] == ':'):
			if strict {
				return fail("a tilde, which bash expands")
			}
			b.WriteByte(c)
			i++
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String(), nil
}

// scanEnv calls visit with each NAME and its (unquoted) value. The value is only ever handed to visit;
// callers that must not hold one (TokenSet) look at its length and drop it.
func scanEnv(path string, visit func(name, value string)) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return scanEnvReader(f, visit)
}

func scanEnvReader(r io.Reader, visit func(name, value string)) error {
	return scanEnvRaw(r, func(name, raw string) {
		val, _ := parseBashValue(raw, false)
		visit(name, val)
	})
}

// scanEnvRaw calls visit with each NAME and the raw text right of its `=`, unparsed.
func scanEnvRaw(r io.Reader, visit func(name, raw string)) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		name, val, ok := strings.Cut(line, "=")
		if !ok || strings.ContainsAny(name, " \t") {
			continue
		}
		visit(name, val)
	}
	return sc.Err()
}

// TokenSet reports whether name has a non-empty value in the env file. It keeps only that one bit.
func TokenSet(path, name string) (bool, error) {
	set := false
	err := scanEnv(path, func(n, v string) {
		if n == name {
			set = len(v) > 0
		}
	})
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return set, err
}

// ReadToken returns the value of name in the env file ("" when unset or empty; the last definition wins,
// as a sourced file's would). The caller must use it as a bearer and never print, log or return it in an error.
//
// It parses the value as bash does (see parseBashValue), because the helper that SENDS the token sources
// this file with bash: a reader that disagreed would verify one token and send another. A line whose value
// only bash could evaluate (`$HOME`, a backtick, text after the word) is an error naming the variable.
func ReadToken(path, name string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	raw, found := "", false
	if err := scanEnvRaw(f, func(n, r string) {
		if n == name {
			raw, found = r, true
		}
	}); err != nil {
		return "", err
	}
	if !found {
		return "", nil
	}
	val, perr := parseBashValue(raw, true)
	if perr != nil {
		return "", fmt.Errorf("sessioninit: %s in %s: the value has %s; write it as a plain or single-quoted value", name, path, perr)
	}
	return val, nil
}

// TokenVarNames lists the variable names in the env file that start with prefix, sorted — names only.
func TokenVarNames(path, prefix string) ([]string, error) {
	seen := map[string]bool{}
	if err := scanEnv(path, func(n, _ string) {
		if strings.HasPrefix(n, prefix) {
			seen[n] = true
		}
	}); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out, nil
}

// JudgeBuilderTools is the builder verification: a runner-scope token sees ONLY runner__* tools. It fails
// on an empty list (a token that sees nothing proves nothing) and on any tool outside the runner
// namespace, naming what it saw — an author tool means the token carries author scope, which would let the
// builder read the checks it is being tested against.
func JudgeBuilderTools(names []string) error {
	if len(names) == 0 {
		return errors.New("tools/list returned no tools: this is not a working runner-scope token")
	}
	var foreign []string
	for _, n := range names {
		if !strings.HasPrefix(n, "runner__") {
			foreign = append(foreign, n)
		}
	}
	if len(foreign) == 0 {
		return nil
	}
	sort.Strings(foreign)
	shown := foreign
	if len(shown) > 8 {
		shown = append(append([]string{}, shown[:8]...), fmt.Sprintf("... and %d more", len(foreign)-8))
	}
	return fmt.Errorf("this token sees %d non-runner tool(s): %s. A builder token must be runner scope only; "+
		"a token that lists author tools has author scope and would expose the tester's checks. "+
		"Revoke it and mint a runner-scope (builder) token", len(foreign), strings.Join(shown, ", "))
}

// CountAuthorTools counts the author_* tools in a tools/list answer.
func CountAuthorTools(names []string) int {
	n := 0
	for _, x := range names {
		if strings.HasPrefix(x, "author_") {
			n++
		}
	}
	return n
}

// Message is the human-readable result: what was written, and the ONE thing the human must do.
func (r *Result) Message() string {
	var b strings.Builder
	fmt.Fprintf(&b, "argus %s init %s\n", r.Kind, r.App)
	if len(r.Changed) == 0 {
		b.WriteString("  nothing to change: everything is already in place\n")
	}
	for _, c := range r.Changed {
		fmt.Fprintf(&b, "  wrote  %s\n", c)
	}
	fmt.Fprintf(&b, "  env file      %s (mode 600)\n  headersHelper %s\n  CLI wrapper   %s\n", r.EnvFile, r.Helper, r.Wrapper)
	if r.MCPJSON != "" {
		fmt.Fprintf(&b, "  .mcp.json     %s (entry %q)\n", r.MCPJSON, MCPKey)
	}
	return b.String()
}
