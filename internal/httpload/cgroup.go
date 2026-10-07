package httpload

// cgroup.go -- the executor's OWN CPU throttling around a step (, GitHub #615).
//
// A closed-model HTTP load generator that is CFS-throttled serves fewer requests than the target could: the
// "limit" it reads is the executor's. The reading is the cgroup's cpu.stat counters `nr_periods` and
// `nr_throttled` (cgroup v2: /sys/fs/cgroup/cpu.stat; v1: .../cpu/cpu.stat or .../cpu,cpuacct/cpu.stat), taken
// before and after a step's JMeter run. Pure parsing and arithmetic; the only I/O is ReadCPUStat's file read.
// Nothing here is ever invented: an unreadable file, a counter that went backwards or one that did not advance
// is "not measured", never a share.

import (
	"os"
	"strconv"
	"strings"
)

// GeneratorThrottleBound is the throttled share of CFS periods above which a step is generator_limited. A
// throttled period is one in which the cgroup used its whole quota and was stopped until the next period; a
// quarter of them means the generator was starved of CPU for long enough to cap what it could send.
const GeneratorThrottleBound = 0.25

// CPUStat is one reading of the cgroup's CFS counters.
type CPUStat struct {
	Periods, Throttled int64
}

// CPUStatPaths are tried in order; the first that holds both counters wins.
var CPUStatPaths = []string{
	"/sys/fs/cgroup/cpu.stat",
	"/sys/fs/cgroup/cpu/cpu.stat",
	"/sys/fs/cgroup/cpu,cpuacct/cpu.stat",
}

// ParseCPUStat reads `nr_periods` and `nr_throttled` out of a cpu.stat body (both cgroup versions use the
// same two keys). ok is false unless BOTH are present and numeric.
func ParseCPUStat(body string) (CPUStat, bool) {
	var st CPUStat
	var gotP, gotT bool
	for _, line := range strings.Split(body, "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		n, err := strconv.ParseInt(f[1], 10, 64)
		if err != nil || n < 0 {
			continue
		}
		switch f[0] {
		case "nr_periods":
			st.Periods, gotP = n, true
		case "nr_throttled":
			st.Throttled, gotT = n, true
		}
	}
	return st, gotP && gotT
}

// ReadCPUStat reads the executor's own cgroup counters from the first path that has them. ok is false when none
// can be read (no cgroup filesystem, a non-Linux host, a cgroup without the cpu controller).
func ReadCPUStat() (CPUStat, bool) { return ReadCPUStatFrom(CPUStatPaths, os.ReadFile) }

// ReadCPUStatFrom is ReadCPUStat over named paths and a reader (a test passes a fake filesystem).
func ReadCPUStatFrom(paths []string, read func(string) ([]byte, error)) (CPUStat, bool) {
	for _, p := range paths {
		b, err := read(p)
		if err != nil {
			continue
		}
		if st, ok := ParseCPUStat(string(b)); ok {
			return st, true
		}
	}
	return CPUStat{}, false
}

// ThrottledShare is the share of CFS periods that were throttled between two readings. ok is false when either
// reading is missing, a counter went backwards (a restarted cgroup) or no period elapsed (no quota to throttle
// against, or a read too short to count one): the share is then NOT MEASURED.
func ThrottledShare(before, after CPUStat, haveBefore, haveAfter bool) (float64, bool) {
	if !haveBefore || !haveAfter {
		return 0, false
	}
	dp, dt := after.Periods-before.Periods, after.Throttled-before.Throttled
	if dp <= 0 || dt < 0 {
		return 0, false
	}
	if dt > dp {
		dt = dp
	}
	return float64(dt) / float64(dp), true
}
