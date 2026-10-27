package k8srender

// executor_cpu.go — the executor container's CPU LIMIT as an onboarding setting.
//
// The executor Deployment has always rendered `requests.cpu: 150m` and `limits.cpu: 1`. A 1-CPU JMeter client
// saturates under a heavy load step and skews the result, so a tester can ask for more: `--executor-cpu <quantity>`
// sets the LIMIT. The default stays "1" so every existing instance renders byte-identically. The request is NOT a
// setting (it is what the scheduler reserves), so a limit below it is refused. The instance ResourceQuota is sized
// from the same value (quotaReplicas x the limit + CpuQuotaHeadroomMilli), because a quota that assumes 1 CPU per
// executor refuses the executor's own second or third replica once the limit is raised.

import (
	"fmt"
	"strconv"
	"strings"
)

const (
	// DefaultExecutorCPULimit is the limit the executor container carries when the setting is not given.
	DefaultExecutorCPULimit = "1"
	// ExecutorCPURequest is the executor container's CPU request (the template's `requests: {cpu: "150m"`), and the
	// floor of the limit. TestExecutorCPU_RequestConstantMatchesTheTemplate holds it equal to what is rendered.
	ExecutorCPURequest = "150m"
	// quotaCPUHeadroomMilli is the CPU the instance quota holds beyond the executors: loki + pushgateway, a
	// promtail per node and the one-shot pods (see normalize).
	quotaCPUHeadroomMilli = 3000
)

// parseCPUQuantity reads a CPU quantity as millicores. It accepts what an operator types for a CPU limit: a plain
// number with up to three decimals ("2", "1.5", "0.5", ".5") or whole millicores ("1500m"). Kubernetes' finest CPU
// unit is 1m, so a finer value is not a CPU quantity here. Nothing else is accepted: no sign, no exponent, no other
// suffix.
func parseCPUQuantity(s string) (int64, error) {
	bad := fmt.Errorf("%q is not a Kubernetes CPU quantity (a number of CPUs such as 2, 1.5 or 0.5, or millicores such as 1500m)", s)
	if s == "" {
		return 0, bad
	}
	// The digit bounds (at most 6 digits of CPUs, 9 of millicores) keep every value far inside int64 and are the
	// same bounds onboard.sh's executor_cpu_check uses; a "CPU count" of a million is a typo, not a request.
	if strings.HasSuffix(s, "m") {
		d := strings.TrimSuffix(s, "m")
		if d == "" || len(d) > 9 || !allDigits(d) {
			return 0, bad
		}
		n, err := strconv.ParseInt(d, 10, 64)
		if err != nil {
			return 0, bad
		}
		return n, nil
	}
	whole, frac, hasDot := strings.Cut(s, ".")
	if whole == "" && !hasDot {
		return 0, bad
	}
	if (whole != "" && (len(whole) > 6 || !allDigits(whole))) || (hasDot && (frac == "" || len(frac) > 3 || !allDigits(frac))) {
		return 0, bad
	}
	var w int64
	if whole != "" {
		n, err := strconv.ParseInt(whole, 10, 64)
		if err != nil {
			return 0, bad
		}
		w = n
	}
	var f int64
	if hasDot {
		n, err := strconv.ParseInt((frac + "000")[:3], 10, 64)
		if err != nil {
			return 0, bad
		}
		f = n
	}
	return w*1000 + f, nil
}

func allDigits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return s != ""
}

// ValidateExecutorCPU checks a --executor-cpu value: empty is allowed (the default applies); anything else must be
// a CPU quantity of at least ExecutorCPURequest.
func ValidateExecutorCPU(v string) error {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	got, err := parseCPUQuantity(v)
	if err != nil {
		return fmt.Errorf("--executor-cpu: %w", err)
	}
	floor, _ := parseCPUQuantity(ExecutorCPURequest)
	if got < floor {
		return fmt.Errorf("--executor-cpu %s is below the executor's CPU request (%s); the limit cannot be lower than the request. Use %s or more", v, ExecutorCPURequest, ExecutorCPURequest)
	}
	return nil
}

// executorCPULimit is the limit the executor container renders: the setting, else the default.
func (in Instance) executorCPULimit() string {
	if v := strings.TrimSpace(in.ExecutorCPU); v != "" {
		return v
	}
	return DefaultExecutorCPULimit
}

// executorCPUMilli is the executor's CPU limit in millicores, for sizing the instance quota. A value that does not
// parse (validate refuses it before anything is written) counts as the default.
func (in Instance) executorCPUMilli() int64 {
	n, err := parseCPUQuantity(in.executorCPULimit())
	if err != nil {
		n, _ = parseCPUQuantity(DefaultExecutorCPULimit)
	}
	return n
}

// quotaCPU is the instance quota's limits.cpu: room for every executor at its limit plus the headroom. Whole CPUs
// render as a plain integer ("6", exactly as before), anything else in millicores ("7500m").
func (in Instance) quotaCPU() string {
	m := int64(quotaReplicas(in.Replicas))*in.executorCPUMilli() + quotaCPUHeadroomMilli
	if m%1000 == 0 {
		return strconv.FormatInt(m/1000, 10)
	}
	return strconv.FormatInt(m, 10) + "m"
}
