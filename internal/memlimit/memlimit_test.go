package memlimit

import (
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
)

// run applies the limit against fake cgroup files; v2/v1 == "" means that file does not exist.
func run(t *testing.T, v2, v1 string, env map[string]string) (Result, []int64) {
	t.Helper()
	dir := t.TempDir()
	o := Options{V2File: filepath.Join(dir, "v2-missing"), V1File: filepath.Join(dir, "v1-missing")}
	if v2 != "" {
		o.V2File = filepath.Join(dir, "memory.max")
		if err := os.WriteFile(o.V2File, []byte(v2), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if v1 != "" {
		o.V1File = filepath.Join(dir, "memory.limit_in_bytes")
		if err := os.WriteFile(o.V1File, []byte(v1), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	o.Getenv = func(k string) string { return env[k] }
	var calls []int64
	o.Set = func(b int64) int64 { calls = append(calls, b); return 0 }
	return Apply(o), calls
}

const mib = int64(1) << 20

// share is what Apply must set for a container limit of n MiB.
func share(n int64) int64 { return int64(float64(n*mib) * Fraction) }

func TestApply_V2Number_SetsAFractionOfTheLimit(t *testing.T) {
	r, calls := run(t, "1610612736\n", "", nil) // 1536Mi, with the trailing newline the kernel prints
	want := share(1536)
	if len(calls) != 1 || calls[0] != want || !r.Applied || r.Limit != want {
		t.Fatalf("want one call with %d, got calls=%v result=%+v", want, calls, r)
	}
	if !strings.Contains(r.Reason, "1536 MiB") {
		t.Fatalf("the log line must name the container limit it read: %q", r.Reason)
	}
}

func TestApply_V2Max_SetsNothing(t *testing.T) {
	r, calls := run(t, "max\n", "", nil)
	if len(calls) != 0 || r.Applied {
		t.Fatalf("\"max\" means no limit: nothing may be set, got calls=%v result=%+v", calls, r)
	}
	if !strings.Contains(r.Reason, "no memory limit") {
		t.Fatalf("the reason must say why: %q", r.Reason)
	}
}

func TestApply_V1Number_SetsAFractionOfTheLimit(t *testing.T) {
	r, calls := run(t, "", "3221225472\n", nil) // 3Gi
	want := share(3072)
	if len(calls) != 1 || calls[0] != want {
		t.Fatalf("want one call with %d, got %v (%+v)", want, calls, r)
	}
}

func TestApply_V1NoLimitSentinel_SetsNothing(t *testing.T) {
	for _, s := range []string{"9223372036854771712\n", "9223372036854775807", "99999999999999999999999"} {
		r, calls := run(t, "", s, nil)
		if len(calls) != 0 || r.Applied {
			t.Fatalf("v1 sentinel %q is no limit: nothing may be set, got %v %+v", s, calls, r)
		}
	}
}

func TestApply_NoFiles_SetsNothingAndSaysSo(t *testing.T) {
	r, calls := run(t, "", "", nil)
	if len(calls) != 0 || r.Applied || !strings.Contains(r.Reason, "can be read") {
		t.Fatalf("missing cgroup files: got calls=%v result=%+v", calls, r)
	}
}

func TestApply_GOMEMLIMITSet_AlwaysWins(t *testing.T) {
	r, calls := run(t, "1610612736", "", map[string]string{"GOMEMLIMIT": "500MiB"})
	if len(calls) != 0 || r.Applied || !strings.Contains(r.Reason, "GOMEMLIMIT is set") {
		t.Fatalf("an explicit GOMEMLIMIT must win: calls=%v result=%+v", calls, r)
	}
}

func TestApply_UnreadableValue_SetsNothing(t *testing.T) {
	for _, c := range [][2]string{{"lots", ""}, {"-5", ""}, {"", "lots"}} {
		r, calls := run(t, c[0], c[1], nil)
		if len(calls) != 0 || r.Applied {
			t.Fatalf("%q/%q: an unreadable value must set nothing, got %v %+v", c[0], c[1], calls, r)
		}
	}
}

// The default Set really moves the runtime's limit (and the test puts it back).
func TestApply_DefaultSetChangesTheRuntimeLimit(t *testing.T) {
	prev := debug.SetMemoryLimit(-1)
	defer debug.SetMemoryLimit(prev)
	dir := t.TempDir()
	f := filepath.Join(dir, "memory.max")
	if err := os.WriteFile(f, []byte("2147483648"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := Apply(Options{V2File: f, V1File: filepath.Join(dir, "none"), Getenv: func(string) string { return "" }})
	if !r.Applied {
		t.Fatalf("not applied: %+v", r)
	}
	if got, want := debug.SetMemoryLimit(-1), share(2048); got != want {
		t.Fatalf("runtime limit is %d, want %d", got, want)
	}
}

func TestApply_V2WinsOverV1(t *testing.T) {
	_, calls := run(t, "1073741824", "3221225472", nil)
	want := share(1024)
	if len(calls) != 1 || calls[0] != want {
		t.Fatalf("v2 must be read first: want %d, got %v", want, calls)
	}
}
