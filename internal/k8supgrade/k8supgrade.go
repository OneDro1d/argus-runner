// Package k8supgrade is the pure half of `argus upgrade`: it parses a fresh render-k8s render into
// objects, compares each against what is live, formats the difference for a person, and builds the
// minimal patch. It never runs kubectl and never sees a Secret's contents — Secrets are identified
// (IsSecret) so the caller can skip them, and nothing here reads or prints `data`/`stringData`.
//
// The comparison is DESIRED ⊆ LIVE: every field the render sets must already hold in the cluster;
// fields that exist only live (server defaults, status, other controllers' annotations, anything a
// person added) are never differences. The price is stated in the command's help: upgrade adds and
// changes, it never removes.
package k8supgrade

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"reflect"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// ExecutorName is the executor Deployment, whose replica count belongs to the executor's own
// autoscaler (internal/runner/autoscale.go) and whose image belongs to the control plane's Update.
const ExecutorName = "executor"

// Object is one rendered Kubernetes object, JSON-normalised.
type Object struct {
	APIVersion, Kind, Name, Namespace string
	Doc                               map[string]any
}

// Change is one field that differs.
type Change struct {
	Path      string
	Live      any
	Desired   any
	Missing   bool // absent live (an addition), as opposed to present with another value
	Immutable bool // the API server refuses to change this field on an existing object
}

// Parse splits YAML documents into objects. Empty and comment-only documents are skipped.
func Parse(manifests ...string) ([]Object, error) {
	var out []Object
	for _, text := range manifests {
		dec := yaml.NewDecoder(strings.NewReader(text))
		for {
			var raw any
			if err := dec.Decode(&raw); err == io.EOF {
				break
			} else if err != nil {
				return nil, fmt.Errorf("parse rendered manifest: %w", err)
			}
			if raw == nil {
				continue
			}
			b, err := json.Marshal(raw)
			if err != nil {
				return nil, fmt.Errorf("normalise rendered manifest: %w", err)
			}
			var doc map[string]any
			if err := json.Unmarshal(b, &doc); err != nil {
				return nil, fmt.Errorf("normalise rendered manifest: %w", err)
			}
			o := Object{Doc: doc}
			o.APIVersion, _ = doc["apiVersion"].(string)
			o.Kind, _ = doc["kind"].(string)
			if meta, ok := doc["metadata"].(map[string]any); ok {
				o.Name, _ = meta["name"].(string)
				o.Namespace, _ = meta["namespace"].(string)
			}
			if o.Kind == "" || o.Name == "" {
				return nil, fmt.Errorf("rendered document without kind/name: %v", doc)
			}
			out = append(out, o)
		}
	}
	return out, nil
}

// IsSecret reports whether the object is a Secret: never compared, never read, never written.
func (o Object) IsSecret() bool { return o.Kind == "Secret" }

// Ref is the human name, Kind/name.
func (o Object) Ref() string { return o.Kind + "/" + o.Name }

func (o Object) group() string {
	if i := strings.Index(o.APIVersion, "/"); i >= 0 {
		return o.APIVersion[:i]
	}
	return ""
}

// Resource is the kubectl resource type, group-qualified so a custom resource is unambiguous.
func (o Object) Resource() string {
	r := strings.ToLower(o.Kind)
	if g := o.group(); g != "" {
		r += "." + g
	}
	return r
}

// deepCopy copies a JSON-shaped value.
func deepCopy(v any) any {
	switch t := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(t))
		for k, x := range t {
			m[k] = deepCopy(x)
		}
		return m
	case []any:
		s := make([]any, len(t))
		for i, x := range t {
			s[i] = deepCopy(x)
		}
		return s
	default:
		return v
	}
}

// Prepare returns a copy of the object's document with the runtime-owned fields removed, so they are
// neither compared nor patched: the executor Deployment's spec.replicas (the executor scales its own
// Deployment, idle→1 and run→3; re-applying the render's genesis 1 would undo a live scale).
// The executor container's `image` is removed too: the image belongs to the control plane's Update, and a
// patch that carried the value read a moment ago would revert an update that lands in between.
func Prepare(o Object) map[string]any {
	d := deepCopy(o.Doc).(map[string]any)
	if o.Kind == "Deployment" && o.Name == ExecutorName {
		spec, _ := d["spec"].(map[string]any)
		if spec != nil {
			delete(spec, "replicas")
		}
		tpl, _ := spec["template"].(map[string]any)
		ps, _ := tpl["spec"].(map[string]any)
		cs, _ := ps["containers"].([]any)
		for _, c := range cs {
			if cm, ok := c.(map[string]any); ok && cm["name"] == ExecutorName {
				delete(cm, "image")
			}
		}
	}
	return d
}

var immutablePrefixes = map[string][]string{
	"Deployment":            {"spec.selector"},
	"DaemonSet":             {"spec.selector"},
	"PersistentVolumeClaim": {"spec.accessModes", "spec.storageClassName", "spec.volumeName", "spec.volumeMode"},
	"RoleBinding":           {"roleRef"},
	"ClusterRoleBinding":    {"roleRef"},
}

// Diff lists every field desired sets that live does not already hold.
func Diff(o Object, desired, live map[string]any) []Change {
	var cs []Change
	// Identity is what selected the live object in the first place; it is not a difference.
	desired = deepCopy(desired).(map[string]any)
	delete(desired, "apiVersion")
	delete(desired, "kind")
	if meta, ok := desired["metadata"].(map[string]any); ok {
		delete(meta, "name")
		delete(meta, "namespace")
	}
	walk("", desired, live, &cs)
	for i := range cs {
		for _, p := range immutablePrefixes[o.Kind] {
			if cs[i].Path == p || strings.HasPrefix(cs[i].Path, p+".") || strings.HasPrefix(cs[i].Path, p+"[") {
				cs[i].Immutable = true
			}
		}
	}
	return cs
}

func isEmpty(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return t == ""
	case map[string]any:
		return len(t) == 0
	case []any:
		return len(t) == 0
	}
	return false
}

func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

// listKey is the field the API server merges the elements of the list at `path` on, when every
// element is a map carrying it. The server's keys are not all `name`: volumeMounts merge by
// mountPath, volumeDevices by devicePath, a container's ports by containerPort and a Service's ports
// by port. A list of another shape that has no merge key (NetworkPolicy ports, RBAC rules, subjects)
// is replaced whole by a patch, so it returns "" and is compared position by position.
func listKey(path string, l []any) string {
	if len(l) == 0 {
		return ""
	}
	last := path
	if i := strings.LastIndex(path, "."); i >= 0 {
		last = path[i+1:]
	}
	want := "name"
	switch {
	case last == "volumeMounts":
		want = "mountPath"
	case last == "volumeDevices":
		want = "devicePath"
	case last == "ports" && path == "spec.ports":
		want = "port"
	case last == "ports" && strings.Contains(strings.ToLower(path), "containers["):
		want = "containerPort"
	case last == "ports":
		return ""
	}
	for _, e := range l {
		m, ok := e.(map[string]any)
		if !ok || m[want] == nil {
			return ""
		}
	}
	return want
}

func keyStr(v any) string {
	if f, ok := v.(float64); ok {
		return fmt.Sprintf("%d", int64(f))
	}
	return fmt.Sprint(v)
}

func walk(path string, d, l any, out *[]Change) {
	switch dv := d.(type) {
	case map[string]any:
		lm, ok := l.(map[string]any)
		if !ok {
			*out = append(*out, Change{Path: path, Live: l, Desired: d, Missing: l == nil})
			return
		}
		keys := make([]string, 0, len(dv))
		for k := range dv {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			lv, present := lm[k]
			if !present {
				if !isEmpty(dv[k]) {
					*out = append(*out, Change{Path: join(path, k), Desired: dv[k], Missing: true})
				}
				continue
			}
			walk(join(path, k), dv[k], lv, out)
		}
	case []any:
		ll, ok := l.([]any)
		if !ok {
			if !isEmpty(d) {
				*out = append(*out, Change{Path: path, Live: l, Desired: d, Missing: l == nil})
			}
			return
		}
		if key := listKey(path, dv); key != "" {
			for _, de := range dv {
				dm := de.(map[string]any)
				label := fmt.Sprintf("%s[%s]", path, keyStr(dm[key]))
				var found any
				for _, le := range ll {
					if lm, ok := le.(map[string]any); ok && lm[key] != nil && keyStr(lm[key]) == keyStr(dm[key]) {
						found = lm
						break
					}
				}
				if found == nil {
					*out = append(*out, Change{Path: label, Desired: de, Missing: true})
					continue
				}
				walk(label, de, found, out)
			}
			return
		}
		if len(dv) != len(ll) {
			*out = append(*out, Change{Path: path, Live: l, Desired: d})
			return
		}
		for i := range dv {
			walk(fmt.Sprintf("%s[%d]", path, i), dv[i], ll[i], out)
		}
	default:
		if !scalarEqual(path, d, l) {
			*out = append(*out, Change{Path: path, Live: l, Desired: d})
		}
	}
}

func scalarEqual(path string, d, l any) bool {
	if reflect.DeepEqual(d, l) {
		return true
	}
	ds, dok := d.(string)
	ls, lok := l.(string)
	if dok && lok && quantityPath(path) {
		a, aok := parseQuantity(ds)
		b, bok := parseQuantity(ls)
		return aok && bok && a.Cmp(b) == 0
	}
	if dok && ds == "" && l == nil {
		return true
	}
	return false
}

func quantityPath(p string) bool {
	return strings.Contains(p, "resources") || strings.Contains(p, "spec.hard") ||
		strings.Contains(p, "requests") || strings.Contains(p, "limits")
}

var quantityRe = regexp.MustCompile(`^([+-]?[0-9]*\.?[0-9]+)(Ki|Mi|Gi|Ti|Pi|Ei|n|u|m|k|M|G|T|P|E)?$`)

var quantityScale = map[string]*big.Rat{
	"":   big.NewRat(1, 1),
	"n":  big.NewRat(1, 1_000_000_000),
	"u":  big.NewRat(1, 1_000_000),
	"m":  big.NewRat(1, 1000),
	"k":  big.NewRat(1000, 1),
	"M":  big.NewRat(1_000_000, 1),
	"G":  big.NewRat(1_000_000_000, 1),
	"T":  big.NewRat(1_000_000_000_000, 1),
	"P":  big.NewRat(1_000_000_000_000_000, 1),
	"E":  big.NewRat(1_000_000_000_000_000_000, 1),
	"Ki": big.NewRat(1<<10, 1),
	"Mi": big.NewRat(1<<20, 1),
	"Gi": big.NewRat(1<<30, 1),
	"Ti": big.NewRat(1<<40, 1),
	"Pi": big.NewRat(1<<50, 1),
	"Ei": big.NewRat(1<<60, 1),
}

func parseQuantity(s string) (*big.Rat, bool) {
	m := quantityRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return nil, false
	}
	r, ok := new(big.Rat).SetString(m[1])
	if !ok {
		return nil, false
	}
	return r.Mul(r, quantityScale[m[2]]), true
}

// ---- formatting ---------------------------------------------------------------------------------

var sensitiveName = regexp.MustCompile(`(?i)token|secret|passw|credential|api[_-]?key|private|auth`)

// redact returns a copy of v in which the literal `value` of any env-shaped map (a map with a
// sensitive `name`) is masked. A secretKeyRef carries only a Secret NAME and key, never a value, and
// is shown as is.
func redact(v any) any {
	switch t := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(t))
		name, _ := t["name"].(string)
		for k, x := range t {
			if k == "value" && name != "" && sensitiveName.MatchString(name) {
				m[k] = "<redacted>"
				continue
			}
			m[k] = redact(x)
		}
		return m
	case []any:
		s := make([]any, len(t))
		for i, x := range t {
			s[i] = redact(x)
		}
		return s
	}
	return v
}

var urlUserinfo = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.\-]*://)[^/\s@"'\\]+@`)

// MaskCreds hides the userinfo of any URL in s (scheme://user:pw@host becomes scheme://<redacted>@host).
// It is applied to every value upgrade prints, whatever the field is called: the name-based masking
// below cannot know that an innocuously named variable holds a URL with a password in it.
func MaskCreds(s string) string { return urlUserinfo.ReplaceAllString(s, "${1}<redacted>@") }

// hasUserinfo reports whether a value holds a URL with userinfo (what MaskCreds would rewrite).
func hasUserinfo(v any) bool {
	if s, ok := v.(string); ok {
		return urlUserinfo.MatchString(s)
	}
	b, err := json.Marshal(v)
	return err == nil && urlUserinfo.Match(b)
}

var envValuePath = regexp.MustCompile(`env\[([^\]]*)\]\.value$`)

func show(v any) string {
	switch t := v.(type) {
	case nil:
		return "<absent>"
	case string:
		return fmt.Sprintf("%q", MaskCreds(t))
	}
	b, err := json.Marshal(redact(v))
	if err != nil {
		return "<unprintable>"
	}
	s := MaskCreds(string(b))
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

// hiddenCredentialNote is printed in place of a difference the credential mask would otherwise hide.
// It names no value: the field or key is in front of it.
const hiddenCredentialNote = "a credential inside a URL changed and is not shown"

// FormatChanges renders changes as indented lines a person can read.
func FormatChanges(cs []Change) []string {
	var out []string
	for _, c := range cs {
		mark := "~"
		if c.Missing {
			mark = "+"
		}
		suffix := ""
		if c.Immutable {
			suffix = "   [immutable: the API server will not change this on an existing object]"
		}
		ds, dIsStr := c.Desired.(string)
		ls, lIsStr := c.Live.(string)
		if m := envValuePath.FindStringSubmatch(c.Path); m != nil && sensitiveName.MatchString(m[1]) {
			out = append(out, fmt.Sprintf("  %s %s: <redacted> -> <redacted> (value withheld: sensitive name)%s", mark, c.Path, suffix))
			continue
		}
		if dIsStr && strings.Contains(ds, "\n") && (lIsStr || c.Live == nil) {
			shown := lineDiff(MaskCreds(ls), MaskCreds(ds))
			// Masking runs before the diff, so a line whose ONLY change is inside a URL's userinfo looks
			// unchanged to it. The raw diff (never printed) is longer than the masked one exactly then.
			hidden := len(lineDiff(ls, ds)) > len(shown)
			if hidden && len(shown) == 0 {
				out = append(out, fmt.Sprintf("  %s %s: %s%s", mark, c.Path, hiddenCredentialNote, suffix))
				continue
			}
			out = append(out, fmt.Sprintf("  %s %s:%s", mark, c.Path, suffix))
			for _, l := range shown {
				out = append(out, "      "+l)
			}
			if hidden {
				out = append(out, "      ("+hiddenCredentialNote+")")
			}
			continue
		}
		if !c.Missing && show(c.Live) == show(c.Desired) && !reflect.DeepEqual(c.Live, c.Desired) &&
			(hasUserinfo(c.Live) || hasUserinfo(c.Desired)) {
			// Both sides print the same once a URL's userinfo is masked: say what happened, not two identical values.
			out = append(out, fmt.Sprintf("  ~ %s: %s%s", c.Path, hiddenCredentialNote, suffix))
			continue
		}
		if c.Missing {
			out = append(out, fmt.Sprintf("  + %s: %s%s", c.Path, show(c.Desired), suffix))
		} else {
			out = append(out, fmt.Sprintf("  ~ %s: %s -> %s%s", c.Path, show(c.Live), show(c.Desired), suffix))
		}
	}
	return out
}

// lineDiff returns the removed (-) and added (+) lines between two texts, in order, by longest
// common subsequence; unchanged lines are not shown.
func lineDiff(a, b string) []string {
	x := strings.Split(strings.TrimSuffix(a, "\n"), "\n")
	y := strings.Split(strings.TrimSuffix(b, "\n"), "\n")
	if a == "" {
		x = nil
	}
	n, m := len(x), len(y)
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if x[i] == y[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else if lcs[i+1][j] >= lcs[i][j+1] {
				lcs[i][j] = lcs[i+1][j]
			} else {
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}
	var out []string
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case x[i] == y[j]:
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			out = append(out, "- "+x[i])
			i++
		default:
			out = append(out, "+ "+y[j])
			j++
		}
	}
	for ; i < n; i++ {
		out = append(out, "- "+x[i])
	}
	for ; j < m; j++ {
		out = append(out, "+ "+y[j])
	}
	return out
}

// ---- patch ----------------------------------------------------------------------------------------

var builtInGroups = map[string]bool{
	"": true, "apps": true, "batch": true, "policy": true, "autoscaling": true,
	"rbac.authorization.k8s.io": true, "networking.k8s.io": true, "storage.k8s.io": true,
}

// Patch builds the patch for one object: the prepared desired document. Built-in kinds get a
// strategic merge patch (lists merge by name, so a Service keeps its allocated nodePort); custom
// resources get a JSON merge patch. Volumes and the Deployment strategy carry $retainKeys, so a
// volume whose source type changes is replaced, not merged into a second source.
// Patch uses `kubectl patch` and never `apply`.
func Patch(o Object, desired map[string]any) (string, []byte, error) {
	return PatchFor(o, desired, nil)
}

// PatchFor is Patch for an object whose live state is known. A patch never changes what Diff called
// unchanged: the prepared document is first aligned to live (alignToLive), so every keyed list it carries
// keeps the live order and every equal value is live's own. Why this mechanism: Diff compares keyed lists
// by key, the API server orders a merged list by the PATCH, so the only way to be sure an "unchanged" list
// is not re-ordered (which changes the pod-template hash and rolls the Deployment) is to send it in the
// order live holds it, for every list listKey knows and not only env. With live nil the document is sent as is.
func PatchFor(o Object, desired, live map[string]any) (string, []byte, error) {
	typ := "merge"
	if builtInGroups[o.group()] {
		typ = "strategic"
	}
	d := deepCopy(desired).(map[string]any)
	if live != nil {
		d = alignToLive("", d, live).(map[string]any)
	}
	if typ == "strategic" {
		addRetainKeys(d)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(d); err != nil {
		return "", nil, err
	}
	return typ, bytes.TrimSpace(buf.Bytes()), nil
}

// TemplateChanges lists the paths, under a workload's pod template (spec.template), that the patch would
// change. Because a patch never changes what Diff called unchanged (PatchFor), these are exactly the
// differences Diff reported there. Nil for anything that is not a workload.
func TemplateChanges(o Object, cs []Change) []string {
	if !o.IsWorkload() {
		return nil
	}
	var out []string
	for _, c := range cs {
		if c.Path == "spec.template" || strings.HasPrefix(c.Path, "spec.template.") || strings.HasPrefix(c.Path, "spec.template[") {
			out = append(out, c.Path)
		}
	}
	return out
}

// RestartNote is the line that says applying will replace the workload's pods, or "" when nothing in the
// pod template changes. It names what changes (paths only, never a value).
func RestartNote(o Object, cs []Change, apply bool) string {
	paths := TemplateChanges(o, cs)
	if len(paths) == 0 {
		return ""
	}
	shown := paths
	more := ""
	if len(shown) > 6 {
		more = fmt.Sprintf(" and %d more", len(shown)-6)
		shown = shown[:6]
	}
	verb := "would REPLACE"
	if apply {
		verb = "will REPLACE"
	}
	tail := ""
	if o.Kind == "Deployment" && o.Name == ExecutorName {
		tail = "; with one replica (maxSurge 0) the executor has no pod while it rolls, and a run in flight is cut"
	}
	return fmt.Sprintf("  => POD RESTART: applying this %s the pods of %s (its pod template changes: %s%s)%s", verb, o.Ref(), strings.Join(shown, ", "), more, tail)
}

// PortsNeedAWholeListPatch reports whether o is a Service one of whose differences lies in spec.ports.
// The API server merges a Service's ports by `port`, so a strategic patch that carries a port whose
// NUMBER changed while its NAME stayed adds a second entry with the old name, and the server refuses
// the result (duplicate port name). Such a Service is patched with ServicePortsPatch instead.
func PortsNeedAWholeListPatch(o Object, cs []Change) bool {
	if o.Kind != "Service" || o.group() != "" {
		return false
	}
	for _, c := range cs {
		if c.Path == "spec.ports" || strings.HasPrefix(c.Path, "spec.ports[") {
			return true
		}
	}
	return false
}

// ServicePortsPatch is the patch for a Service whose spec.ports differ: a JSON MERGE patch, which
// replaces an array whole, carrying the desired ports. Each desired port keeps the nodePort the server
// allocated for the live port it replaces (matched by name, then by number), so a NodePort or
// LoadBalancer Service does not have its port re-allocated; a ClusterIP Service has none to keep.
func ServicePortsPatch(o Object, desired, live map[string]any) (string, []byte, error) {
	d := deepCopy(desired).(map[string]any)
	spec, _ := d["spec"].(map[string]any)
	liveSpec, _ := live["spec"].(map[string]any)
	if spec != nil {
		typ, _ := spec["type"].(string)
		if typ == "" {
			typ, _ = liveSpec["type"].(string)
		}
		livePorts, _ := liveSpec["ports"].([]any)
		if ports, ok := spec["ports"].([]any); ok && (typ == "NodePort" || typ == "LoadBalancer") {
			for _, p := range ports {
				pm, ok := p.(map[string]any)
				if !ok || pm["nodePort"] != nil {
					continue
				}
				if lp := matchLivePort(pm, livePorts); lp != nil && lp["nodePort"] != nil {
					pm["nodePort"] = lp["nodePort"]
				}
			}
		}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(d); err != nil {
		return "", nil, err
	}
	return "merge", bytes.TrimSpace(buf.Bytes()), nil
}

// matchLivePort finds the live port a desired one replaces: the same name when it has one, else the
// same number.
func matchLivePort(want map[string]any, live []any) map[string]any {
	if name, _ := want["name"].(string); name != "" {
		for _, l := range live {
			if lm, ok := l.(map[string]any); ok && lm["name"] == name {
				return lm
			}
		}
	}
	if want["port"] != nil {
		for _, l := range live {
			if lm, ok := l.(map[string]any); ok && lm["port"] != nil && keyStr(lm["port"]) == keyStr(want["port"]) {
				return lm
			}
		}
	}
	return nil
}

func retain(m map[string]any) []any {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]any, len(keys))
	for i, k := range keys {
		out[i] = k
	}
	return out
}

func addRetainKeys(d map[string]any) {
	spec, _ := d["spec"].(map[string]any)
	if spec == nil {
		return
	}
	if st, ok := spec["strategy"].(map[string]any); ok {
		st["$retainKeys"] = retain(st)
	}
	tpl, _ := spec["template"].(map[string]any)
	if tpl == nil {
		return
	}
	ps, _ := tpl["spec"].(map[string]any)
	if ps == nil {
		return
	}
	if vols, ok := ps["volumes"].([]any); ok {
		for _, v := range vols {
			if vm, ok := v.(map[string]any); ok {
				delete(vm, "$retainKeys")
				vm["$retainKeys"] = retain(vm)
			}
		}
	}
}
