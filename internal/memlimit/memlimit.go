// Package memlimit gives the Go runtime a soft memory limit taken from the container it runs in.
//
// WHY (2026-10-19, measured on Kubernetes): an executor container with a 1536Mi limit was OOM-killed
// twice mid-run by the kernel, the `argus` Go process itself at about 1.0 and 1.16 GB of anonymous
// memory (once beside its JMeter child at about 0.4 GB). The Go runtime does not know it lives in a
// cgroup: with no GOMEMLIMIT it lets the heap grow to twice the live data before it collects, so a
// process whose live data is 500 MB can reach the container limit without ever being "out of memory"
// in its own eyes. A soft limit makes the collector work harder as the process nears the limit
// instead of waiting for the kernel to end it.
//
// WHAT IT IS NOT: a cap. The limit is soft (the runtime may exceed it rather than stall), and it
// governs the Go runtime only, never the JMeter JVM child. The unbounded buffers themselves are bounded
// where they are held (see); this is the net under them.
package memlimit

import (
	"fmt"
	"math"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
)

// Fraction is the share of the container limit the Go runtime is asked to stay under.
//
// 0.6 and not higher because the cgroup limit is shared with everything else in the container: the
// JMeter JVM child (measured about 0.4 GB beside a 1.0 GB argus on a 1536Mi container, and allowed to
// grow for a heavier plan), the Go runtime's non-heap memory (stacks, GC metadata) which counts toward
// the soft limit only partly, and file pages the kernel charges to the same cgroup. At 1536Mi that is
// 921Mi for Go and 615Mi for the rest; at 3Gi it is 1.8Gi and 1.2Gi.
const Fraction = 0.6

// noLimitFloor: a cgroup v1 "no limit" is a number just under 2^63 (page-aligned, for example
// 9223372036854771712), and some kernels print other very large values. Anything at or above 2^60
// bytes (one exbibyte) is not a container limit anyone set.
const noLimitFloor = int64(1) << 60

// Default cgroup file locations, injectable for tests.
const (
	DefaultV2File = "/sys/fs/cgroup/memory.max"
	DefaultV1File = "/sys/fs/cgroup/memory/memory.limit_in_bytes"
)

// Options are the inputs of Apply. The zero value reads the real container.
type Options struct {
	V2File string // default DefaultV2File
	V1File string // default DefaultV1File
	// Getenv reads GOMEMLIMIT (default os.Getenv).
	Getenv func(string) string
	// Set applies the limit (default debug.SetMemoryLimit); tests record the call instead.
	Set func(bytes int64) int64
}

// Result says what Apply did and why, in one sentence a log line can carry.
type Result struct {
	Applied bool
	Limit   int64 // the limit set, bytes; 0 when none was set
	Reason  string
}

// Apply sets the Go memory limit from the container's cgroup when GOMEMLIMIT is not set.
//
// An explicit GOMEMLIMIT always wins (the runtime has already honoured it at start-up; Apply touches
// nothing). No limit in the cgroup, an unreadable file or a value it cannot read sets nothing, and the
// Result says which.
func Apply(o Options) Result {
	if o.V2File == "" {
		o.V2File = DefaultV2File
	}
	if o.V1File == "" {
		o.V1File = DefaultV1File
	}
	if o.Getenv == nil {
		o.Getenv = os.Getenv
	}
	if o.Set == nil {
		o.Set = debug.SetMemoryLimit
	}
	if v := o.Getenv("GOMEMLIMIT"); v != "" {
		return Result{Reason: "GOMEMLIMIT is set in the environment (" + v + "); it is left as it is"}
	}
	limit, src, err := containerLimit(o.V2File, o.V1File)
	if err != nil {
		return Result{Reason: "no Go memory limit set: " + err.Error()}
	}
	if limit <= 0 {
		return Result{Reason: "no Go memory limit set: " + src + " says the container has no memory limit"}
	}
	want := int64(math.Floor(float64(limit) * Fraction))
	if want < 1 {
		return Result{Reason: "no Go memory limit set: the container limit in " + src + " is too small to take a share of"}
	}
	o.Set(want)
	return Result{Applied: true, Limit: want, Reason: fmt.Sprintf(
		"Go memory limit set to %d MiB (%d%% of the %d MiB container limit read from %s)",
		want>>20, int(Fraction*100), limit>>20, src)}
}

// containerLimit returns the container's memory limit in bytes (0 = none), and the file it came from.
// An error means no limit could be learned (no cgroup file, or one that does not parse).
func containerLimit(v2, v1 string) (limit int64, src string, err error) {
	if b, rerr := os.ReadFile(v2); rerr == nil {
		s := strings.TrimSpace(string(b))
		if s == "max" {
			return 0, v2, nil // cgroup v2 spells "no limit" as the word max
		}
		n, perr := strconv.ParseInt(s, 10, 64)
		if perr != nil || n < 0 {
			return 0, v2, fmt.Errorf("%s holds %q, which is neither a number nor \"max\"", v2, s)
		}
		return n, v2, nil
	}
	if b, rerr := os.ReadFile(v1); rerr == nil {
		s := strings.TrimSpace(string(b))
		n, perr := strconv.ParseInt(s, 10, 64)
		if perr != nil {
			// A value that overflows int64 is the v1 "no limit" sentinel on some kernels.
			if ne, ok := perr.(*strconv.NumError); ok && ne.Err == strconv.ErrRange {
				return 0, v1, nil
			}
			return 0, v1, fmt.Errorf("%s holds %q, which is not a number", v1, s)
		}
		if n < 0 || n >= noLimitFloor {
			return 0, v1, nil
		}
		return n, v1, nil
	}
	return 0, "", fmt.Errorf("neither %s nor %s can be read (not running in a cgroup, or not allowed to read it)", v2, v1)
}
