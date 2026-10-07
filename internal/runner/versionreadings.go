package runner

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/OneDro1d/argus-runner/internal/artifactmeasure"
	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/federation"
	"github.com/OneDro1d/argus-runner/internal/scenario"
	"github.com/OneDro1d/argus-runner/internal/testtargets"
)

// versionreadings.go -- the executor half of which VERSION of the system under test is each
// run about to exercise, one reading per Kubernetes namespace the run touches, for EVERY run mode.
//
// ⛔ NOT EVIDENCE. The readings ride the push in their own field (federation.ResultsPush.VersionReadings)
// and are assigned AFTER everything bindMeasurement hashes: they are not in evidence_bundle_hash or
// scenario_evidence_root, they never call artifactmeasure.RefuseIfMismatch, and they leave the artifact
// measurement untouched. A build run's bundle hash is byte-identical to what it was before this file existed.

// maxVersionReadings caps the namespaces read in one run; the control plane accepts the same number.
const maxVersionReadings = 8

// versionReadBudget bounds ALL the readings of one run together (each reader call has its own, shorter
// timeout as well). A var only so a test can shorten it.
var versionReadBudget = 30 * time.Second

// maxVersionReasonLen bounds a reading's reason.
const maxVersionReasonLen = 200

// scenarioRef is what mapping a scenario to its test target needs of it: its id and tags.
type scenarioRef struct {
	ID   string
	Tags []string
}

// namespaceUse is one namespace a run touches and the target names that resolved to it.
type namespaceUse struct {
	Namespace string
	Targets   []string
}

// versionNamespaces is the SET of namespaces a run touches (chooseCaptureNamespace's sibling, which returns
// one). PURE. Each scenario is mapped to its target with testtargets.List.Map; a target that declares a
// namespace contributes it, and a scenario whose target declares none (or an instance with no test_targets)
// contributes envNS (ARGUS_SUT_NAMESPACE) when it is set. Sorted by namespace, unique, at most
// maxVersionReadings; target names sorted and unique. No scenario at all counts as one with no target.
func versionNamespaces(targets testtargets.List, scs []scenarioRef, envNS string) []namespaceUse {
	by := map[string]map[string]bool{}
	add := func(ns, target string) {
		if ns == "" {
			return
		}
		if by[ns] == nil {
			by[ns] = map[string]bool{}
		}
		if target != "" {
			by[ns][target] = true
		}
	}
	if len(scs) == 0 {
		add(envNS, "")
	}
	for _, sc := range scs {
		name := ""
		if len(targets) > 0 {
			name = targets.Map(sc.ID, sc.Tags)
		}
		if t, ok := targets.ByName(name); ok && t.Namespace != "" {
			add(t.Namespace, t.Name)
			continue
		}
		add(envNS, name)
	}
	names := make([]string, 0, len(by))
	for ns := range by {
		names = append(names, ns)
	}
	sort.Strings(names)
	if len(names) > maxVersionReadings {
		names = names[:maxVersionReadings]
	}
	out := make([]namespaceUse, 0, len(names))
	for _, ns := range names {
		var ts []string
		for tn := range by[ns] {
			ts = append(ts, tn)
		}
		sort.Strings(ts)
		out = append(out, namespaceUse{Namespace: ns, Targets: ts})
	}
	return out
}

// takeVersionReadings reads the namespaces versionNamespaces names and returns one reading each. nil when
// no reader is wired (an executor built without one sends nothing: not measured, never a guess). On the
// compose tier it is ONE reading with an empty namespace and no Kubernetes call. It never fails and never
// waits beyond versionReadBudget: a namespace that cannot be read is a reading with no key and a reason
// that names it.
func takeVersionReadings(ctx context.Context, cfg ExecConfig, targets testtargets.List, scs []scenarioRef, envNS string) []federation.VersionReading {
	if cfg.ReadNamespaceVersion == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, versionReadBudget)
	defer cancel()
	if cfg.Tier == "compose" {
		return []federation.VersionReading{readingOf(ctx, cfg.ReadNamespaceVersion, "", nil)}
	}
	var out []federation.VersionReading
	for _, u := range versionNamespaces(targets, scs, envNS) {
		out = append(out, readingOf(ctx, cfg.ReadNamespaceVersion, u.Namespace, u.Targets))
	}
	return out
}

func readingOf(ctx context.Context, read artifactmeasure.NamespaceFunc, ns string, targets []string) federation.VersionReading {
	r := federation.VersionReading{Namespace: ns, Targets: targets}
	var got artifactmeasure.Reading
	if ctx.Err() != nil {
		got.Reason = "not read: the time allowed for version readings ran out"
	} else {
		got = read(ctx, ns)
	}
	distinct := map[string]bool{}
	for _, d := range got.Digests {
		if n, ok := artifactmeasure.NormalizeDigest(d); ok {
			distinct[n] = true
		}
	}
	r.Unresolved = got.Unresolved
	if got.Reason == "" && len(distinct) > 0 {
		r.Key = artifactmeasure.VersionKey(got.Digests)
		r.Digests = len(distinct)
		return r
	}
	r.Reason = got.Reason
	if r.Reason == "" {
		r.Reason = "no running container reported an image digest (tag-only or locally built images name no version)"
	}
	if ns != "" && !strings.Contains(r.Reason, ns) {
		r.Reason = "namespace " + ns + ": " + r.Reason
	}
	r.Reason = strings.Join(strings.Fields(r.Reason), " ")
	if len(r.Reason) > maxVersionReasonLen {
		r.Reason = r.Reason[:maxVersionReasonLen]
	}
	return r
}

// versionReadingsRaw is the push field: omitted (nil) when there is nothing to say.
func versionReadingsRaw(rs []federation.VersionReading) json.RawMessage {
	if len(rs) == 0 {
		return nil
	}
	b, err := json.Marshal(rs)
	if err != nil {
		return nil
	}
	return b
}

// scenarioRefsIn lists the id and tags of every scenario materialised under dir (the set the run will
// execute, already opened if it arrived sealed). A file that does not parse to an id is skipped: it names no
// target, and the run itself reports it.
func scenarioRefsIn(dir string) []scenarioRef {
	var out []scenarioRef
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".md") {
			return nil
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return nil
		}
		if s := scenario.Parse(string(b)); s != nil && s.ID != "" {
			out = append(out, scenarioRef{ID: s.ID, Tags: s.Tags})
		}
		return nil
	})
	return out
}

// testTargetDecl is the instance's declared test_targets, read the way testTargetsFor reads them; nil on a
// missing or unreadable config (no declaration: every scenario then counts as one with no namespace of its own).
func testTargetDecl(configPath string) testtargets.List {
	if configPath == "" {
		return nil
	}
	c, err := config.ParseUnresolved(configPath)
	if err != nil {
		return nil
	}
	return c.TestTargets
}

// namespaceVersionReader is the reader Bootstrap installs: the executor's own tier and in-cluster service
// account, the same grant ReadRunningContainers already needs (get/list pods, SUTAccessRoleManifest).
func namespaceVersionReader(cfg ExecConfig) artifactmeasure.NamespaceFunc {
	return artifactmeasure.NamespaceReader(artifactmeasure.SystemConfig{
		Tier: cfg.Tier,
		ComposeProject: func() string {
			if cfg.ConfigPath == "" {
				return ""
			}
			c, err := config.ParseUnresolved(cfg.ConfigPath)
			if err != nil {
				return ""
			}
			return c.SUTProject()
		},
	})
}
