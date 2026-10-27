package k8srender

// executor_memory.go — the executor container's memory LIMIT as an onboarding setting.
//
// The executor Deployment has always rendered `requests.memory: 384Mi` and `limits.memory: 1536Mi`. On
// 2026-10-19 an executor was ended by the kernel (out-of-memory) at 1.0-1.16 GB of its own Go process while a
// heavy load run was in progress. The default stays 1536Mi so every existing instance renders byte-identically;
// `--executor-memory <quantity>` raises (or lowers) the limit. The request is NOT a setting: it is what the
// scheduler reserves, and it stays at 384Mi, so a limit below it is refused (the API server would refuse the
// Deployment anyway, but only after onboarding had written everything else).

import (
	"fmt"
	"math/big"
	"regexp"
	"strings"
)

const (
	// DefaultExecutorMemoryLimit is the limit the executor container carries when the setting is not given.
	DefaultExecutorMemoryLimit = "1536Mi"
	// ExecutorMemoryRequest is the executor container's memory request, and the floor of the limit.
	ExecutorMemoryRequest = "384Mi"
)

// quantityRe is the Kubernetes quantity grammar for the values memory takes: a non-negative decimal number
// followed by nothing, a binary suffix (Ki Mi Gi Ti Pi Ei), a decimal suffix (n u m k M G T P E) or a decimal
// exponent (e3, E6). A leading "+" is accepted as Kubernetes does; a negative number is not.
var quantityRe = regexp.MustCompile(`^\+?([0-9]+(?:\.[0-9]+)?|\.[0-9]+)(Ki|Mi|Gi|Ti|Pi|Ei|n|u|m|k|M|G|T|P|E|[eE][+-]?[0-9]+)?$`)

var quantityScale = map[string]*big.Rat{
	"":   big.NewRat(1, 1),
	"Ki": new(big.Rat).SetInt(new(big.Int).Lsh(big.NewInt(1), 10)),
	"Mi": new(big.Rat).SetInt(new(big.Int).Lsh(big.NewInt(1), 20)),
	"Gi": new(big.Rat).SetInt(new(big.Int).Lsh(big.NewInt(1), 30)),
	"Ti": new(big.Rat).SetInt(new(big.Int).Lsh(big.NewInt(1), 40)),
	"Pi": new(big.Rat).SetInt(new(big.Int).Lsh(big.NewInt(1), 50)),
	"Ei": new(big.Rat).SetInt(new(big.Int).Lsh(big.NewInt(1), 60)),
	"n":  big.NewRat(1, 1_000_000_000),
	"u":  big.NewRat(1, 1_000_000),
	"m":  big.NewRat(1, 1_000),
	"k":  big.NewRat(1_000, 1),
	"M":  big.NewRat(1_000_000, 1),
	"G":  big.NewRat(1_000_000_000, 1),
	"T":  big.NewRat(1_000_000_000_000, 1),
	"P":  big.NewRat(1_000_000_000_000_000, 1),
	"E":  big.NewRat(1_000_000_000_000_000_000, 1),
}

// parseMemoryQuantity reads a Kubernetes quantity as a number of bytes.
func parseMemoryQuantity(s string) (*big.Rat, error) {
	m := quantityRe.FindStringSubmatch(s)
	if m == nil {
		return nil, fmt.Errorf("%q is not a Kubernetes quantity (a number with an optional suffix, for example 3Gi, 3072Mi or 4G)", s)
	}
	num, ok := new(big.Rat).SetString(m[1])
	if !ok {
		return nil, fmt.Errorf("%q is not a Kubernetes quantity", s)
	}
	suffix := m[2]
	if suffix != "" && (suffix[0] == 'e' || suffix[0] == 'E') && len(suffix) > 1 {
		// a decimal exponent: 1e9, 5E6. Bounded so a typo cannot ask big.Int for 10^1000000.
		var exp int
		if _, err := fmt.Sscanf(suffix[1:], "%d", &exp); err != nil || exp < -9 || exp > 18 {
			return nil, fmt.Errorf("%q is not a Kubernetes quantity (the exponent must be between -9 and 18)", s)
		}
		scale := new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(absInt(exp))), nil))
		if exp < 0 {
			scale.Inv(scale)
		}
		return num.Mul(num, scale), nil
	}
	return num.Mul(num, quantityScale[suffix]), nil
}

func absInt(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// ValidateExecutorMemory checks a --executor-memory value: empty is allowed (the default applies); anything else
// must be a Kubernetes quantity of at least ExecutorMemoryRequest.
func ValidateExecutorMemory(v string) error {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	got, err := parseMemoryQuantity(v)
	if err != nil {
		return fmt.Errorf("--executor-memory: %w", err)
	}
	floor, _ := parseMemoryQuantity(ExecutorMemoryRequest)
	if got.Cmp(floor) < 0 {
		return fmt.Errorf("--executor-memory %s is below the executor's memory request (%s); the limit cannot be lower than the request. Use %s or more", v, ExecutorMemoryRequest, ExecutorMemoryRequest)
	}
	return nil
}

// executorMemoryMi is the executor's memory limit in MiB, rounded UP, for sizing the instance quota. A value
// that does not parse (validate refuses it before anything is written) counts as the default.
func (in Instance) executorMemoryMi() int {
	b, err := parseMemoryQuantity(in.executorMemoryLimit())
	if err != nil {
		b, _ = parseMemoryQuantity(DefaultExecutorMemoryLimit)
	}
	mi := new(big.Int).Lsh(big.NewInt(1), 20)
	q, r := new(big.Int).QuoRem(b.Num(), new(big.Int).Mul(b.Denom(), mi), new(big.Int))
	if r.Sign() != 0 {
		q.Add(q, big.NewInt(1))
	}
	return int(q.Int64())
}

// executorMemoryLimit is the limit the executor container renders: the setting, else the default.
func (in Instance) executorMemoryLimit() string {
	if v := strings.TrimSpace(in.ExecutorMemory); v != "" {
		return v
	}
	return DefaultExecutorMemoryLimit
}
