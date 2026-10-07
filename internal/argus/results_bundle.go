package argus

import (
	"io/fs"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// ── WHAT MAY LEAVE THE RESULTS DIR ───────────────────────────────────────────────────────────────
//
// The results dir is JMeter's scratch space as much as the report's home: next to report.json and
// runs/<id>.json it holds every scenario's raw samples (<template>__<id>.jtl), JMeter's run logs
// (<template>__<id>.jtl.log — where a `-J` property used to be echoed, name and value), cleanup
// samples, and the durable results outbox. None of that is the product's evidence, and any step
// that bundles the directory for a reader outside the environment — a reveal package, a publish
// step, a support archive — must take the allow-list below and nothing else. It is an ALLOW-list on
// purpose: a new kind of file is excluded until someone decides it is publishable.

// PublishableResult reports whether the results-dir entry at rel (slash- or OS-separated, relative
// to the results dir) may leave the environment: exactly `report.json` and `runs/<id>.json`.
func PublishableResult(rel string) bool {
	rel = path.Clean(filepath.ToSlash(rel))
	if rel == "report.json" {
		return true
	}
	dir, base := path.Split(rel)
	return dir == "runs/" && strings.HasSuffix(base, ".json") && base != ".json"
}

// PublishableResults walks dir and returns, sorted, the relative (slash-separated) paths of the
// files that pass PublishableResult. Everything else on the walk — .jtl, .jtl.log, properties
// files, the outbox — is left behind.
func PublishableResults(dir string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(dir, p)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		if PublishableResult(rel) {
			out = append(out, rel)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}
