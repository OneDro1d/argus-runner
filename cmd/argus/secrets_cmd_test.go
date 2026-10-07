package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// secrets_cmd_test.go — P1 #9. Fakes `kubectl` on PATH (the same shape harness_test.go/
// apply_exec_test.go use for docker/kubectl/curl: a script on a stubbed PATH entry, recording its
// own invocation) so these tests exercise the REAL subprocess call cmdSecretsSet/cmdSecretsList
// make — not an injected Go function.

// writeFakeKubectl writes a `kubectl` script to <dir>/bin that, for every invocation, appends one
// line to <dir>/calls.txt (its argv, exactly as received) and saves its stdin verbatim to
// <dir>/stdin-<n>.txt (n = call index, 0-based) — so a test can assert a value reached kubectl ONLY
// via stdin, never argv. `getJSON`, when non-empty, is what a `get secret ... -o json` call answers.
func writeFakeKubectl(t *testing.T, dir string, getJSON string) string {
	t.Helper()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	getJSONFile := filepath.Join(dir, "get-secret-response.json")
	if getJSON != "" {
		if err := os.WriteFile(getJSONFile, []byte(getJSON), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	script := "#!/usr/bin/env bash\n" +
		"set -e\n" +
		"n=0\n" +
		"while [ -f \"$CALLDIR/stdin-$n.txt\" ]; do n=$((n+1)); done\n" +
		"printf '%s\\n' \"$*\" >> \"$CALLDIR/calls.txt\"\n" +
		"cat > \"$CALLDIR/stdin-$n.txt\"\n" +
		"case \"$*\" in\n" +
		"  *\"get secret\"*\"-o json\"*)\n" +
		"    if [ -s \"$GETJSONFILE\" ]; then cat \"$GETJSONFILE\"; else echo '{}'; fi ;;\n" +
		"esac\n" +
		"exit 0\n"
	if err := os.WriteFile(filepath.Join(bin, "kubectl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CALLDIR", dir)
	t.Setenv("GETJSONFILE", getJSONFile)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return bin
}

// callsFile reads every recorded kubectl invocation (one argv string per line).
func callsFile(t *testing.T, dir string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "calls.txt"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var out []string
	for _, l := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

func stdinFile(t *testing.T, dir string, n int) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "stdin-"+strconv.Itoa(n)+".txt"))
	if err != nil {
		t.Fatalf("read stdin-%d.txt: %v", n, err)
	}
	return string(b)
}

// runSecretsWithStdin calls cmdSecretsSet/cmdSecretsList IN-PROCESS, feeding the given string as
// this process's os.Stdin (the ONLY channel `secrets set` reads its value from) — mirrors upRun in
// up_json_protocol_test.go.
func runSecretsWithStdin(t *testing.T, stdin string, fn func([]string) int, args []string) (out string, code int) {
	t.Helper()
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	prevStdout := os.Stdout
	os.Stdout = outW
	outCh := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(outR)
		outCh <- string(b)
	}()

	prevStdin := os.Stdin
	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdin = inR
	go func() {
		io.WriteString(inW, stdin)
		inW.Close()
	}()

	code = fn(args)

	outW.Close()
	os.Stdout = prevStdout
	os.Stdin = prevStdin
	inR.Close()
	return <-outCh, code
}

// TestSecretsSet_ValueReachesKubectlOnlyOnStdin is the PROMISE's central evidence: the fake kubectl
// records its argv verbatim, and the secret value must NEVER appear there — only on its stdin.
func TestSecretsSet_ValueReachesKubectlOnlyOnStdin(t *testing.T) {
	dir := t.TempDir()
	writeFakeKubectl(t, dir, "")

	const value = "s3kr3t-value-42"
	out, code := runSecretsWithStdin(t, value, cmdSecretsSet,
		[]string{"--key", "DB_PASSWORD", "--namespace", "argus-inst-i9"})
	if code != exitOK {
		t.Fatalf("cmdSecretsSet exit=%d output=%s", code, out)
	}

	calls := callsFile(t, dir)
	if len(calls) != 1 {
		t.Fatalf("expected exactly 1 kubectl call (no --restart), got %d: %v", len(calls), calls)
	}
	if strings.Contains(calls[0], value) {
		t.Fatalf("secret value leaked into kubectl argv: %q", calls[0])
	}
	wantArgv := "patch secret exec-tokens -n argus-inst-i9 --type=merge --patch-file=/dev/stdin"
	if calls[0] != wantArgv {
		t.Fatalf("kubectl argv = %q, want %q", calls[0], wantArgv)
	}

	stdin0 := stdinFile(t, dir, 0)
	if !strings.Contains(stdin0, value) {
		t.Fatalf("value did not reach kubectl's stdin: %q", stdin0)
	}
	var patch struct {
		StringData map[string]string `json:"stringData"`
	}
	if err := json.Unmarshal([]byte(stdin0), &patch); err != nil {
		t.Fatalf("kubectl stdin was not the expected JSON merge patch: %v (%q)", err, stdin0)
	}
	if patch.StringData["DB_PASSWORD"] != value {
		t.Fatalf("patch body = %+v, want DB_PASSWORD=%q", patch.StringData, value)
	}

	// The command's own OWN report never echoes the value either.
	if strings.Contains(out, value) {
		t.Fatalf("secrets set printed the value in its own output: %s", out)
	}
	if !strings.Contains(out, "kubectl rollout restart deployment/executor -n argus-inst-i9") {
		t.Fatalf("secrets set did not report the restart command needed: %s", out)
	}
}

// TestSecretsSet_Restart_RunsRolloutRestart covers the --restart flag: a second kubectl call, no
// stdin needed, and the result says restarted:true.
func TestSecretsSet_Restart_RunsRolloutRestart(t *testing.T) {
	dir := t.TempDir()
	writeFakeKubectl(t, dir, "")

	out, code := runSecretsWithStdin(t, "another-value", cmdSecretsSet,
		[]string{"--key", "API_KEY", "--namespace", "argus-inst-i9", "--restart"})
	if code != exitOK {
		t.Fatalf("cmdSecretsSet exit=%d output=%s", code, out)
	}
	calls := callsFile(t, dir)
	if len(calls) != 2 {
		t.Fatalf("expected 2 kubectl calls (patch + restart), got %d: %v", len(calls), calls)
	}
	if !strings.Contains(calls[1], "rollout restart deployment/executor -n argus-inst-i9") {
		t.Fatalf("second kubectl call = %q, want a rollout restart", calls[1])
	}
	if !strings.Contains(out, `"restarted": true`) {
		t.Fatalf("output does not report restarted:true: %s", out)
	}
}

// TestSecretsSet_RefusesBadKey never reaches kubectl at all when --key is not an env-var NAME.
func TestSecretsSet_RefusesBadKey(t *testing.T) {
	dir := t.TempDir()
	writeFakeKubectl(t, dir, "")

	_, code := runSecretsWithStdin(t, "value", cmdSecretsSet,
		[]string{"--key", "not a var", "--namespace", "argus-inst-i9"})
	if code != exitUsage {
		t.Fatalf("exit = %d, want exitUsage", code)
	}
	if calls := callsFile(t, dir); len(calls) != 0 {
		t.Fatalf("kubectl was invoked despite a refused --key: %v", calls)
	}
}

// TestSecretsSet_RefusesEmptyStdin never patches on an accidentally-empty pipe.
func TestSecretsSet_RefusesEmptyStdin(t *testing.T) {
	dir := t.TempDir()
	writeFakeKubectl(t, dir, "")

	_, code := runSecretsWithStdin(t, "", cmdSecretsSet,
		[]string{"--key", "DB_PASSWORD", "--namespace", "argus-inst-i9"})
	if code != exitUsage {
		t.Fatalf("exit = %d, want exitUsage", code)
	}
	if calls := callsFile(t, dir); len(calls) != 0 {
		t.Fatalf("kubectl was invoked despite empty stdin: %v", calls)
	}
}

// TestSecretsList_NamesOnlyNeverValues proves `secrets list` prints keys and that this process's
// own output never contains a value from the (fake) Secret's data.
func TestSecretsList_NamesOnlyNeverValues(t *testing.T) {
	dir := t.TempDir()
	const fakeSecretValue = "cGFzc3dvcmQtdmFsdWUtc2VjcmV0" // base64, still a value — must never be printed
	getJSON := `{"apiVersion":"v1","kind":"Secret","data":{"DB_PASSWORD":"` + fakeSecretValue + `","ARGUS_RUNNER_TOKEN":"YWJj"}}`
	writeFakeKubectl(t, dir, getJSON)

	out, code := runSecretsWithStdin(t, "", cmdSecretsList,
		[]string{"--namespace", "argus-inst-i9"})
	if code != exitOK {
		t.Fatalf("cmdSecretsList exit=%d output=%s", code, out)
	}
	if strings.Contains(out, fakeSecretValue) || strings.Contains(out, "YWJj") {
		t.Fatalf("secrets list printed a value: %s", out)
	}
	var result struct {
		Keys []string `json:"keys"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("output was not JSON: %v (%s)", err, out)
	}
	want := []string{"ARGUS_RUNNER_TOKEN", "DB_PASSWORD"}
	if len(result.Keys) != len(want) || result.Keys[0] != want[0] || result.Keys[1] != want[1] {
		t.Fatalf("keys = %v, want %v", result.Keys, want)
	}
	calls := callsFile(t, dir)
	if len(calls) != 1 || !strings.Contains(calls[0], "-o json") {
		t.Fatalf("expected one `-o json` kubectl call, got %v", calls)
	}
	for _, c := range calls {
		if strings.Contains(c, "jsonpath") || strings.Contains(c, "go-template") {
			t.Fatalf("secrets list must never run jsonpath/go-template against a Secret (kubectl dumps raw data on a template error): %q", c)
		}
	}
}
