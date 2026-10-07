package compare

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
)

// VersionKey names the version a run measured one system at (design 4.4): the sha256 of the sorted, unique
// running image digests, a newline, and the environment fingerprint. It is "" when NEITHER was measured (no
// digest and no fingerprint), and the table then says "version not measured" instead of pretending two such
// runs ran the same version.
//
// Pure: it reads nothing but its arguments, so the executor, the control plane and a test agree. Digests are
// compared as given (the caller normalises); a blank entry is ignored.
func VersionKey(runningDigests []string, envFingerprint string) string {
	set := map[string]bool{}
	for _, d := range runningDigests {
		if d = strings.TrimSpace(d); d != "" {
			set[d] = true
		}
	}
	env := strings.TrimSpace(envFingerprint)
	if len(set) == 0 && env == "" {
		return ""
	}
	sorted := make([]string, 0, len(set))
	for d := range set {
		sorted = append(sorted, d)
	}
	sort.Strings(sorted)
	h := sha256.Sum256([]byte("argus-version/1\n" + strings.Join(sorted, ",") + "\n" + env))
	return hex.EncodeToString(h[:])
}
