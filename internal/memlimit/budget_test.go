package memlimit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// read builds a Budget against fake cgroup files; v2 == "" means the file does not exist.
func read(t *testing.T, v2, gomemlimit string) Budget {
	t.Helper()
	dir := t.TempDir()
	o := Options{V2File: filepath.Join(dir, "v2-missing"), V1File: filepath.Join(dir, "v1-missing"),
		Getenv: func(k string) string {
			if k == "GOMEMLIMIT" {
				return gomemlimit
			}
			return ""
		}}
	if v2 != "" {
		o.V2File = filepath.Join(dir, "memory.max")
		if err := os.WriteFile(o.V2File, []byte(v2), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return Read(o)
}

func TestScale_IsTheFloorAtTheDefaultBelowItAndWhenUnknown(t *testing.T) {
	for name, b := range map[string]Budget{
		"unknown":            read(t, "", ""),
		"cgroup v2 max":      read(t, "max\n", ""),
		"1536Mi":             read(t, "1610612736\n", ""),
		"1Gi":                read(t, "1073741824\n", ""),
		"GOMEMLIMIT=off":     read(t, "", "off"),
		"GOMEMLIMIT garbage": read(t, "", "lots"),
	} {
		if got := b.Scale(1_000_000); got != 1_000_000 {
			t.Errorf("%s: Scale(1000000) = %d, want exactly 1000000 (%s)", name, got, b.Basis)
		}
		if got := b.Scale(500_000); got != 500_000 {
			t.Errorf("%s: Scale(500000) = %d, want exactly 500000", name, got)
		}
	}
}

func TestScale_IsProportionalToTheContainerLimit(t *testing.T) {
	b := read(t, "4294967296\n", "") // 4Gi
	if got := b.Scale(1_000_000); got != 2_666_666 {
		t.Errorf("4Gi: Scale(1000000) = %d, want 2666666", got)
	}
	if got := b.Scale(500_000); got != 1_333_333 {
		t.Errorf("4Gi: Scale(500000) = %d, want 1333333", got)
	}
	if got := read(t, "3221225472\n", "").Scale(1_000_000); got != 2_000_000 {
		t.Errorf("3Gi: Scale(1000000) = %d, want 2000000", got)
	}
	if !strings.Contains(b.Basis, "4096 MiB container limit") || !strings.Contains(b.Basis, "memory.max") {
		t.Errorf("the basis must name the limit and where it was read: %q", b.Basis)
	}
}

func TestRead_GOMEMLIMIT(t *testing.T) {
	// Shop's executor: limit 4Gi, GOMEMLIMIT 2GiB -> a 2GiB/0.6 = 3413 MiB container.
	b := read(t, "4294967296\n", "2GiB")
	if b.Bytes>>20 != 3413 || !strings.Contains(b.Basis, "GOMEMLIMIT=2GiB") || !strings.Contains(b.Basis, "4096 MiB") {
		t.Errorf("GOMEMLIMIT below the share: %d MiB, %q", b.Bytes>>20, b.Basis)
	}
	// above the cgroup limit it never raises the budget.
	if b := read(t, "4294967296\n", "64GiB"); b.Bytes != 4294967296 {
		t.Errorf("GOMEMLIMIT above the cgroup limit: %d, want the cgroup's", b.Bytes)
	}
	// no cgroup limit at all: GOMEMLIMIT is the only information there is.
	if b := read(t, "", "3GiB"); b.Bytes>>20 != 5120 || !strings.Contains(b.Basis, "no container limit could be read") {
		t.Errorf("GOMEMLIMIT with no cgroup: %d MiB, %q", b.Bytes>>20, b.Basis)
	}
}

func TestParseGoMemLimit(t *testing.T) {
	for in, want := range map[string]int64{"1024": 1024, "1KiB": 1024, "2MiB": 2 << 20, "2GiB": 2 << 30, "1TiB": 1 << 40, "7B": 7} {
		if got, ok := parseGoMemLimit(in); !ok || got != want {
			t.Errorf("parseGoMemLimit(%q) = %d, %v, want %d", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "off", "lots", "-1GiB", "0", "99999999999TiB", "1.5GiB"} {
		if got, ok := parseGoMemLimit(in); ok {
			t.Errorf("parseGoMemLimit(%q) = %d, want not ok", in, got)
		}
	}
}
