package amqpload

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/memlimit"
)

func writeJTL(t *testing.T, rows int) string {
	t.Helper()
	var b strings.Builder
	b.WriteString("timeStamp,elapsed,label,responseCode,responseMessage,success,failureMessage\n")
	for i := 0; i < rows; i++ {
		fmt.Fprintf(&b, "%d,%d,S-amqp-publish,200,OK us=%d,true,\n", 1760000000000+int64(i), 5+i%7, 1000+i)
	}
	p := filepath.Join(t.TempDir(), "step.jtl")
	if err := os.WriteFile(p, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// A file at the cap is read whole; one row more is refused WHOLE with the honest error - never a prefix that
// would be judged as if it were the step.
func TestReadJTL_AtTheCapIsReadOverTheCapIsRefused(t *testing.T) {
	rows, err := readJTL(writeJTL(t, 5), 5, "test basis")
	if err != nil || len(rows) != 5 {
		t.Fatalf("at the cap: rows=%d err=%v, want 5 rows", len(rows), err)
	}
	rows, err = readJTL(writeJTL(t, 6), 5, "test basis")
	if !errors.Is(err, ErrJTLTooLarge) {
		t.Fatalf("over the cap: err=%v, want ErrJTLTooLarge", err)
	}
	if rows != nil {
		t.Fatalf("over the cap: %d rows returned, want none (a prefix must never be judged)", len(rows))
	}
	var tl *TooLargeError
	if !errors.As(err, &tl) || tl.Cap != 5 || tl.Seen != 6 || !tl.SeenAll || tl.Basis != "test basis" {
		t.Fatalf("the error must carry the cap, the samples seen and the basis: %+v", tl)
	}
	for _, want := range []string{"wrote 6 samples", "cap of 5", "test basis", "NOT judged", "--executor-memory"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the message must say %q: %v", want, err)
		}
	}
}

// The production entry point uses the named constant.
func TestReadJTL_UsesMaxJTLRows(t *testing.T) {
	if JTLRowsFloor != 500_000 {
		t.Fatalf("JTLRowsFloor = %d: change the message, the decision record and the docs together", JTLRowsFloor)
	}
	rows, err := ReadJTL(writeJTL(t, 3), memlimit.Budget{})
	if err != nil || len(rows) != 3 || rows[2].Message != "OK us=1002" || !rows[0].Success || rows[0].Label != "S-amqp-publish" {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
}

func TestReadJTL_EmptyAndHeaderOnlyAreNoRows(t *testing.T) {
	p := filepath.Join(t.TempDir(), "e.jtl")
	if err := os.WriteFile(p, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if rows, err := ReadJTL(p, memlimit.Budget{}); rows != nil || err != nil {
		t.Fatalf("empty: %v %v", rows, err)
	}
	if rows, err := ReadJTL(writeJTL(t, 0), memlimit.Budget{}); rows != nil || err != nil {
		t.Fatalf("header only: %v %v", rows, err)
	}
}

// the per-step cap is its floor at the default container and with no readable limit, and grows
// with the memory the executor is given.
func TestJTLRowCap_IsTheFloorAtTheDefaultAndScalesAbove(t *testing.T) {
	const mi = int64(1) << 20
	for name, b := range map[string]memlimit.Budget{
		"unknown limit":     {},
		"1536Mi":            {Bytes: 1536 * mi},
		"below the default": {Bytes: 1024 * mi},
	} {
		if got := JTLRowCap(b); got != 500_000 {
			t.Errorf("%s: cap = %d, want exactly 500000", name, got)
		}
	}
	if got := JTLRowCap(memlimit.Budget{Bytes: 4096 * mi}); got != 1_333_333 {
		t.Errorf("4Gi: cap = %d, want 1333333 (500,000 x 4096/1536)", got)
	}
}

// 600,000 rows: refused at the default container, read whole at 4Gi (the production entry point).
func TestReadJTL_TheCapScalesWithTheBudget(t *testing.T) {
	const mi = int64(1) << 20
	p := writeJTL(t, 600_000)
	rows, err := ReadJTL(p, memlimit.Budget{Bytes: 1536 * mi, Basis: "1536 MiB container limit, read from x"})
	if rows != nil || !errors.Is(err, ErrJTLTooLarge) {
		t.Fatalf("1536Mi: rows=%d err=%v, want ErrJTLTooLarge", len(rows), err)
	}
	for _, want := range []string{"wrote 600000 samples", "cap of 500000", "1536 MiB container limit, read from x"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the message must say %q: %v", want, err)
		}
	}
	rows, err = ReadJTL(p, memlimit.Budget{Bytes: 4096 * mi})
	if err != nil || len(rows) != 600_000 {
		t.Fatalf("4Gi: rows=%d err=%v, want all 600000 rows", len(rows), err)
	}
}
