package compare

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"
)

// The hash of an output (design 1.2.6). The domain strings are part of the algorithm: a part hash
// of the same bytes differs between status, headers and body, and the total is positional.
const (
	DomainStatus  = "argus-output/1/status"
	DomainHeaders = "argus-output/1/headers"
	DomainBody    = "argus-output/1/body"
	domainTotal   = "argus-output/1\n"
	domainRoot    = "argus-outputs-root/1\n"
	domainRules   = "argus-rules/1\n"
	domainCheck   = "argus-compare/1/check\n"
	domainSet     = "argus-compare/1/set\n"
	domainClaim   = "argus-compare/1/claim\n"
)

func sum(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// PartHash is sha256(domain "\n" bytes), lower-case hex.
func PartHash(domain string, b []byte) string {
	h := sha256.New()
	h.Write([]byte(domain))
	h.Write([]byte("\n"))
	h.Write(b)
	return hex.EncodeToString(h.Sum(nil))
}

// OutputHash is sha256("argus-output/1\n" status_part "\n" headers_part "\n" body_part) where each
// *_part is the hex part hash, or "" for a part the check does not compare.
func OutputHash(statusPart, headersPart, bodyPart string) string {
	return sum(domainTotal, statusPart, "\n", headersPart, "\n", bodyPart)
}

// OutputsRoot commits to every row of ResultsPush.outputs: scenario id, step, sample and hash, in
// sorted order, with an unambiguous field separator (a unit separator never occurs in an id).
//
// ARGUS-CMP-11 fix F5: for a row that carries them it ALSO commits to its tolerant values (rule index, path, canonical number) and
// its load numbers, because those numbers decide a cell and the root is what the ledger's reference and member versions anchor. They
// follow the row's own line, each on a line that starts with the record separator 0x1e (a control character, which a scenario id
// cannot contain, so no extra line can be read as a row line), values sorted by (rule, path, number) so the order they arrive in is not
// part of what is committed to. A row that carries NEITHER contributes exactly the bytes it always did: every root a released 0.3.57
// executor computed, and every root already stored, is still the root of its rows (golden tests pin it).
//
// ⛔ The function is linked by the executor (to build outputs_root) and by the control plane (to check it): one function, never two.
func OutputsRoot(rows []ScenarioOutput) string {
	cp := append([]ScenarioOutput(nil), rows...)
	sort.SliceStable(cp, func(i, j int) bool {
		a, b := cp[i], cp[j]
		if a.ScenarioID != b.ScenarioID {
			return a.ScenarioID < b.ScenarioID
		}
		if a.Step != b.Step {
			return a.Step < b.Step
		}
		return a.Sample < b.Sample
	})
	h := sha256.New()
	h.Write([]byte(domainRoot))
	for _, r := range cp {
		h.Write([]byte(r.ScenarioID + "\x1f" + r.Step + "\x1f" + strconv.Itoa(r.Sample) + "\x1f" + r.Hash + "\n"))
		for _, line := range numberLines(r) {
			h.Write([]byte(line))
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// canonNumber is the one text of a number the root commits to: the shortest decimal that parses back to the same float64.
func rootNumber(f float64) string { return strconv.FormatFloat(f, 'g', -1, 64) }

// numberLines are the lines a row's tolerant values and load numbers add to the root (none for a row that carries neither).
func numberLines(r ScenarioOutput) []string {
	var lines []string
	if len(r.Values) > 0 {
		vs := append([]ToleranceValue(nil), r.Values...)
		sort.SliceStable(vs, func(i, j int) bool {
			if vs[i].Rule != vs[j].Rule {
				return vs[i].Rule < vs[j].Rule
			}
			if vs[i].Path != vs[j].Path {
				return vs[i].Path < vs[j].Path
			}
			return rootNumber(vs[i].Value) < rootNumber(vs[j].Value)
		})
		for _, v := range vs {
			lines = append(lines, "\x1evalue\x1f"+strconv.Itoa(v.Rule)+"\x1f"+v.Path+"\x1f"+rootNumber(v.Value)+"\n")
		}
	}
	if l := r.Load; l != nil {
		lines = append(lines, "\x1eload\x1f"+strconv.Itoa(l.Samples)+"\x1f"+rootNumber(l.P50Ms)+"\x1f"+rootNumber(l.P95Ms)+"\x1f"+rootNumber(l.P99Ms)+"\x1f"+rootNumber(l.ErrorRate)+"\n")
	}
	return lines
}
