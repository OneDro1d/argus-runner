package updatecmd

import (
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/obsconfig"
)

// promtail_positions_test.go — AC-D35, compose sites 1 and 2: a promtail RECREATE must resume from
// where it left off.
//
// promtail's positions file tracks how far it has read each container's log. Two compose tiers used
// to lose that cursor on every recreate:
//   - the M3 per-instance overlay (docker-compose.byo-m3.yml) bind-mounted a single HOST FILE onto
//     /tmp/positions.yaml — promtail saves by rename() over its target, and a rename onto a
//     bind-mounted FILE fails EBUSY, so the save silently never happened;
//   - the base compose file (docker-compose.yaml, the plain/non-control-plane tier) mounted nothing
//     for it at all — /tmp/positions.yaml lived on the container's own writable layer, discarded by
//     any recreate.
//
// The fix: a NAMED volume `promtail-positions` mounted at the DIRECTORY /var/lib/promtail, in BOTH
// compose files, with the rendered config's `positions: filename:` pointed inside it — a rename()
// inside a real volume filesystem has no busy single-file target to fail against.
//
// ⛔ The ORIGINAL version of this test sliced the promtail block by the FIRST BLANK LINE after its
// header (`strings.Index(src[i+1:], "\n\n")`). A blank line anywhere inside a service's own
// comments — which this fix's comments add — truncates the slice long before the block's real end,
// so the assertions below would pass by reading past the end of what they mean to check. Fixed by
// slicing to the NEXT TWO-SPACE-INDENTED SERVICE KEY (or end of file) instead, which no blank line
// inside the block can move.
func promtailServiceBlock(t *testing.T, src string) string {
	t.Helper()
	start := strings.Index(src, "\n  promtail:\n")
	if start < 0 {
		t.Fatalf("no promtail: service found — re-point this test deliberately")
	}
	rest := src[start+1:] // begins with the "  promtail:\n" header line itself
	afterHeader := strings.Index(rest, "\n")
	if afterHeader < 0 {
		t.Fatalf("promtail: service has no body")
	}
	// The next KEY line indented by at most two spaces (a sibling service, or a top-level
	// networks:/volumes:/secrets:) ends the block. Comments, blank lines and any deeper-indented
	// content in between are part of the block, not a boundary. ⚠ Not `^\n  key:`: that matches only
	// a key PRECEDED BY A BLANK LINE, so a sibling service written without one would be read as part
	// of this block, and the assertions below could pass on another service's text.
	nextKeyRE := regexp.MustCompile(`(?m)^ {0,2}[A-Za-z0-9_.-]+:`)
	tail := rest[afterHeader:]
	loc := nextKeyRE.FindStringIndex(tail)
	end := len(rest)
	if loc != nil {
		end = afterHeader + loc[0]
	}
	return rest[:end]
}

// nonCommentLines drops every line whose trimmed content starts with "#" — so a substring check
// below cannot be tricked by a fix's own explanatory comment quoting the OLD, defective text for
// context (a grep for a defect can hit its own fix's comment describing that defect).
func nonCommentLines(block string) string {
	var b strings.Builder
	for _, l := range strings.Split(block, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "#") {
			continue
		}
		b.WriteString(l)
		b.WriteString("\n")
	}
	return b.String()
}

// assertPromtailPositionsFixed is the shared assertion for both compose files: no target mounts
// onto /tmp/positions.yaml, and the named `promtail-positions` volume is mounted at the DIRECTORY
// /var/lib/promtail (not any single file under it, which would reintroduce the EBUSY rename target).
func assertPromtailPositionsFixed(t *testing.T, file, block string) {
	t.Helper()
	code := nonCommentLines(block)
	if strings.Contains(code, ":/tmp/positions.yaml") {
		t.Errorf("[%s] the promtail service still mounts something onto /tmp/positions.yaml — the "+
			"container's writable layer, discarded by every recreate:\n%s", file, block)
	}
	if !strings.Contains(code, "promtail-positions:/var/lib/promtail") {
		t.Errorf("[%s] no line mounts the named `promtail-positions` volume at the directory "+
			"/var/lib/promtail (the fix — a rename() inside a real volume filesystem, unlike a "+
			"single-file bind, cannot be EBUSY):\n%s", file, block)
	}
	// The mount target must be exactly the directory, never a single file under it — a file target
	// would resurrect the same EBUSY-on-rename failure this fix exists to remove.
	if strings.Contains(code, "promtail-positions:/var/lib/promtail/positions.yaml") {
		t.Errorf("[%s] the named volume is mounted onto a FILE under /var/lib/promtail, not the "+
			"directory itself — that reintroduces the single-file rename() target:\n%s", file, block)
	}
}

// TestPromtailConfig_ObsconfigRendered_FilenameUnderVarLibPromtail covers the compose promtail
// config internal/obsconfig renders per-instance (what a real onboard actually writes).
func TestPromtailConfig_ObsconfigRendered_FilenameUnderVarLibPromtail(t *testing.T) {
	var c config.Config
	if err := yaml.Unmarshal([]byte("project: {name: memstore}\n"), &c); err != nil {
		t.Fatalf("unmarshal minimal argus-config: %v", err)
	}
	rendered := obsconfig.RenderPromtail(&c, "memstore-compose")
	assertPositionsFilenameUnderVarLibPromtail(t, "obsconfig.RenderPromtail", rendered)
}

// assertPositionsFilenameUnderVarLibPromtail parses out `positions: filename: <path>` and requires
// it to live inside /var/lib/promtail/ — the directory the named volume is mounted at, so the
// container's configured target and the compose mount actually agree.
func assertPositionsFilenameUnderVarLibPromtail(t *testing.T, label, yamlSrc string) {
	t.Helper()
	m := regexp.MustCompile(`(?m)^positions:\n(?:[ \t]*#.*\n)*[ \t]*filename:[ \t]*(\S+)[ \t]*$`).FindStringSubmatch(yamlSrc)
	if m == nil {
		t.Fatalf("[%s] could not find a `positions:\\n  filename: <path>` block:\n%s", label, yamlSrc)
	}
	path := m[1]
	if !strings.HasPrefix(path, "/var/lib/promtail/") {
		t.Errorf("[%s] positions filename is %q, want a path inside /var/lib/promtail/ (where the "+
			"named promtail-positions volume is mounted)", label, path)
	}
	if strings.Contains(path, "/tmp/") {
		t.Errorf("[%s] positions filename %q still points under /tmp — the container's own writable "+
			"layer, discarded by every recreate", label, path)
	}
}
