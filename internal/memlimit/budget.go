package memlimit

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// DefaultContainerBytes is the executor container's memory limit when --executor-memory is not given (1536Mi,
// internal/k8srender). The row caps in internal/argus and internal/amqpload are sized for this container, so
// they are their FLOORS: at this limit, below it, or when no limit can be read, a cap is exactly its floor.
const DefaultContainerBytes = int64(1536) << 20

// Budget is the memory the process is given, as a container size, and the words that say where it came from.
// The zero value is "unknown": every Scale of it is the floor.
type Budget struct {
	Bytes int64  // the container-equivalent size, bytes; 0 = no limit could be read
	Basis string // one phrase for a message: what the number was read from
}

// Read learns the memory the process is given, without touching the Go runtime (Apply does that).
//
// WHICH LIMIT: the row caps bound what the GO HEAP holds, so the right quantity is the heap
// the process is allowed. Apply gives the Go runtime 60% of the cgroup limit (Fraction); the cap is therefore
// scaled on the cgroup limit, the number `--executor-memory` sets and the kernel enforces. When the operator
// set GOMEMLIMIT, the runtime holds the heap to THAT (Apply leaves it alone), so a GOMEMLIMIT lower than the
// 60% share is the truer budget: it is read as the container it would be a 60% share of (GOMEMLIMIT /
// Fraction), and the smaller of the two wins. A GOMEMLIMIT above the cgroup limit never raises a cap (the
// kernel ends the process at the cgroup limit whatever the runtime thinks). GOMEMLIMIT with no readable
// cgroup limit (a laptop, a container with no limit) is the only information there is, and is used. Nothing
// readable, or "no limit" (cgroup v2 `max`, the v1 sentinel, GOMEMLIMIT=off): unknown, the floors.
func Read(o Options) Budget {
	if o.V2File == "" {
		o.V2File = DefaultV2File
	}
	if o.V1File == "" {
		o.V1File = DefaultV1File
	}
	if o.Getenv == nil {
		o.Getenv = os.Getenv
	}
	cg, src, err := containerLimit(o.V2File, o.V1File)
	cgKnown := err == nil && cg > 0
	raw := strings.TrimSpace(o.Getenv("GOMEMLIMIT"))
	gl, glOK := parseGoMemLimit(raw)
	var equiv int64
	if glOK {
		equiv = int64(float64(gl) / Fraction)
	}
	switch {
	case cgKnown && glOK && equiv < cg:
		return Budget{Bytes: equiv, Basis: fmt.Sprintf(
			"GOMEMLIMIT=%s, which the executor reads as a %d MiB container (it is %d%% of that); the container limit is %d MiB, read from %s",
			raw, equiv>>20, int(Fraction*100), cg>>20, src)}
	case cgKnown:
		return Budget{Bytes: cg, Basis: fmt.Sprintf("%d MiB container limit, read from %s", cg>>20, src)}
	case glOK:
		return Budget{Bytes: equiv, Basis: fmt.Sprintf(
			"GOMEMLIMIT=%s, which the executor reads as a %d MiB container (no container limit could be read)", raw, equiv>>20)}
	}
	return Budget{Basis: fmt.Sprintf("no memory limit could be read, so the %d MiB default is assumed", DefaultContainerBytes>>20)}
}

// Scale returns floor scaled by this budget relative to DefaultContainerBytes, never below floor: at the default
// container, below it, or with an unknown budget it is floor exactly. It rounds down.
func (b Budget) Scale(floor int) int {
	if b.Bytes <= DefaultContainerBytes {
		return floor
	}
	return int(float64(floor) * float64(b.Bytes) / float64(DefaultContainerBytes))
}

// parseGoMemLimit reads GOMEMLIMIT the way the Go runtime does: an integer with an optional unit
// (B, KiB, MiB, GiB, TiB). Empty, "off" or anything it cannot read is "no limit": ok is false.
func parseGoMemLimit(s string) (n int64, ok bool) {
	if s == "" || s == "off" {
		return 0, false
	}
	units := []struct {
		suf  string
		mult int64
	}{{"TiB", 1 << 40}, {"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10}, {"B", 1}}
	mult := int64(1)
	for _, u := range units {
		if strings.HasSuffix(s, u.suf) {
			s, mult = strings.TrimSuffix(s, u.suf), u.mult
			break
		}
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil || v <= 0 || v > (int64(1)<<62)/mult {
		return 0, false
	}
	return v * mult, true
}
