package updatecmd

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// ⛔ — THE GENERATED SCRIPTS RUN AGAINST A KIT THAT IS NOT THE ONE THEY WERE WRITTEN WITH.
//
// `argus update` runs from the CURRENT image and generates apply.sh, which then SOURCES THE TARGET KIT's
// libraries (`. "$KIT/onboarding/lib/…"`). On an update the target is newer or equal; on a ROLLBACK it is
// OLDER, and a function the newer kit added is not there. #545 added grafana_instance_dashboard to
// grafana-refs.sh and called it from A-4: the forward update 0.3.60 -> 0.3.61 passed, the rollback back to
// 0.3.60 died with `grafana_instance_dashboard: command not found` — found by the release's own upgrade
// self-check, not by any test, because every fixture here copies the CURRENT libraries.
//
// ⛔ WHICH KITS: the three newest final release tags (git tags), not "the previous one". The newest tag can be
// a release that was never promoted (v0.3.61 was tagged and then failed its self-check; no instance ever ran
// it), and a check against it alone would have passed this very defect. Tags are immutable, so this
// mechanism cannot go stale the way a vendored list refreshed by a script can; what it can do is be ABSENT:
// GitHub CI checks out shallow with no tags. There it SKIPS, loudly and by name, and the merge gate (a normal
// clone) runs it. Setting ARGUS_REQUIRE_PREVIOUS_KIT=1 turns the skip into a failure.

var finalTagRE = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)

func gitOut(args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", filepath.Join("..", "..")}, args...)...)
	b, err := cmd.Output()
	return string(b), err
}

// previousKitTags returns the newest three final release tags, newest first. It skips (or fails) when the
// checkout carries none.
func previousKitTags(t *testing.T) []string {
	t.Helper()
	out, err := gitOut("tag", "-l", "v*")
	var tags []string
	if err == nil {
		for _, l := range strings.Fields(out) {
			if finalTagRE.MatchString(l) {
				tags = append(tags, l)
			}
		}
	}
	sort.Slice(tags, func(i, j int) bool { return SemverLess(tags[j][1:], tags[i][1:]) })
	if len(tags) > 3 {
		tags = tags[:3]
	}
	if len(tags) == 0 {
		msg := "THE PREVIOUS RELEASES' KITS WERE NOT CHECKED: this checkout has no release tags (a shallow CI clone?). " +
			"The merge gate runs this in a full clone. Set ARGUS_REQUIRE_PREVIOUS_KIT=1 to make this a failure."
		if os.Getenv("ARGUS_REQUIRE_PREVIOUS_KIT") == "1" {
			t.Fatal(msg)
		}
		t.Skip(msg)
	}
	return tags
}

// kitLibsAt reads every onboarding/lib/*.sh of a release, by file name.
func kitLibsAt(t *testing.T, tag string) map[string]string {
	t.Helper()
	list, err := gitOut("ls-tree", "--name-only", tag, "onboarding/lib/")
	if err != nil {
		t.Fatalf("git ls-tree %s onboarding/lib/: %v", tag, err)
	}
	libs := map[string]string{}
	for _, p := range strings.Fields(list) {
		if !strings.HasSuffix(p, ".sh") {
			continue
		}
		src, err := gitOut("show", tag+":"+p)
		if err != nil {
			t.Fatalf("git show %s:%s: %v", tag, p, err)
		}
		libs[filepath.Base(p)] = src
	}
	return libs
}

var (
	libFuncRE = regexp.MustCompile(`(?m)^([a-z_][a-z0-9_]*)\(\) *\{`)
	// A leading class of [A-Z_]: internal/scenario's TestOldIDGrammarIsGoneFromTheTree scans every file under
	// internal/ for the retired scenario-id pattern's spelling, and the plain uppercase form of this one is it.
	libVarRE    = regexp.MustCompile(`(?m)^([A-Z_][A-Z0-9_]{3,})=`)
	sourcedLib  = regexp.MustCompile(`\. "\$(?:KIT|STAGE/kit)/onboarding/lib/([a-z0-9-]+\.sh)"`)
	scriptsKind = []struct {
		name     string
		rollback bool
	}{{"update", false}, {"rollback", true}}
)

// kitNamesUsed returns, for one generated script, the names (functions and variables) that the CURRENT kit's
// libraries define and the script uses without defining them itself.
func kitNamesUsed(t *testing.T, script string) (funcs, vars []string, sourced []string) {
	t.Helper()
	for _, m := range sourcedLib.FindAllStringSubmatch(script, -1) {
		sourced = append(sourced, m[1])
	}
	seenF, seenV := map[string]bool{}, map[string]bool{}
	for _, lib := range sourced {
		src := repoFile(t, "onboarding/lib/"+lib)
		for _, m := range libFuncRE.FindAllStringSubmatch(src, -1) {
			n := m[1]
			if seenF[n] || regexp.MustCompile(`(?m)^\s*`+n+`\(\) *\{`).MatchString(script) {
				continue
			}
			if regexp.MustCompile(`\b` + n + `\b`).MatchString(script) {
				seenF[n] = true
				funcs = append(funcs, n)
			}
		}
		for _, m := range libVarRE.FindAllStringSubmatch(src, -1) {
			n := m[1]
			if seenV[n] || regexp.MustCompile(`(?m)^\s*(local\s+)?`+n+`=`).MatchString(script) {
				continue
			}
			if regexp.MustCompile(`\$\{?` + n + `\b`).MatchString(script) {
				seenV[n] = true
				vars = append(vars, n)
			}
		}
	}
	sort.Strings(funcs)
	sort.Strings(vars)
	return
}

func generatedScripts(t *testing.T) map[string]string {
	t.Helper()
	all := map[string]string{}
	for _, tier := range []string{"compose", "k3d", "managed"} {
		for _, k := range scriptsKind {
			in := fixtureInput(tier)
			if k.rollback {
				in.RollbackTo, in.Version = "0.3.31", "0.3.31"
				in.Installed = Manifest{InstanceID: "i1", Tier: tier, Version: "0.3.32",
					Previous: &Previous{Version: "0.3.31", Image: in.ImageDigest}}
			}
			p, err := BuildPlan(in)
			if err != nil {
				t.Fatalf("%s %s: %v", tier, k.name, err)
			}
			all[tier+"/"+k.name+"/preflight"] = RenderPreflight(p, in)
			all[tier+"/"+k.name+"/apply"] = RenderApply(p, in)
		}
	}
	return all
}

// ⛔ EVERY KIT NAME A GENERATED SCRIPT USES IS EITHER IN THE PREVIOUS RELEASES' KITS OR GUARDED.
func TestGeneratedScripts_EveryKitNameExistsInThePreviousReleasesKitsOrIsGuarded(t *testing.T) {
	tags := previousKitTags(t)
	scripts := generatedScripts(t)
	checked := 0
	for name, script := range scripts {
		funcs, vars, sourced := kitNamesUsed(t, script)
		for _, tag := range tags {
			libs := kitLibsAt(t, tag)
			var all strings.Builder
			for _, lib := range sourced {
				src, ok := libs[lib]
				if !ok {
					t.Errorf("%s sources onboarding/lib/%s, which the kit at %s does not ship — a rollback to it dies at the `.` line", name, lib, tag)
					continue
				}
				all.WriteString(src + "\n")
			}
			for _, fn := range funcs {
				checked++
				if regexp.MustCompile(`(?m)^` + fn + `\(\) *\{`).MatchString(all.String()) {
					continue
				}
				if strings.Contains(script, "declare -F "+fn+" ") {
					continue
				}
				t.Errorf("%s calls %s(), which the kit at %s does not define and the script does not guard "+
					"(`declare -F %s`) — a rollback to that release dies `%s: command not found`", name, fn, tag, fn, fn)
			}
			for _, v := range vars {
				checked++
				if !regexp.MustCompile(`(?m)^` + v + `=`).MatchString(all.String()) {
					t.Errorf("%s reads $%s, which the kit at %s does not set", name, v, tag)
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no kit function or variable was found in any generated script — the detector is broken, and a test that checks nothing passes")
	}
}

// the library's own function, cut out: the kit as it was before #545 added it
func withoutFunction(t *testing.T, src, fn string) string {
	t.Helper()
	re := regexp.MustCompile(`(?ms)^` + fn + `\(\) *\{.*?^\}\n`)
	if !re.MatchString(src) {
		t.Fatalf("%s is not defined in the library — re-point this test", fn)
	}
	return re.ReplaceAllString(src, "")
}

func readDashboardBody(t *testing.T, root string) (uid string, argusInstanceCurrent map[string]interface{}, raw string) {
	t.Helper()
	raw = readFile(t, filepath.Join(root, "dashboard-body.json"))
	var body struct {
		Dashboard struct {
			UID        string `json:"uid"`
			Templating struct {
				List []struct {
					Name    string                 `json:"name"`
					Current map[string]interface{} `json:"current"`
				} `json:"list"`
			} `json:"templating"`
		} `json:"dashboard"`
	}
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("the POSTed dashboard body is not JSON (%v):\n%s", err, raw)
	}
	for _, v := range body.Dashboard.Templating.List {
		if v.Name == "argus_instance" {
			argusInstanceCurrent = v.Current
		}
	}
	return body.Dashboard.UID, argusInstanceCurrent, raw
}
