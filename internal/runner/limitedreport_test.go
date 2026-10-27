package runner

import (
	"bytes"
	"context"
	"encoding"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/report"
)

// AC-F2 — the classification test. Every JSON field a report.Report can serialise — through every report type,
// and through the types of other packages it embeds (envcapture.Capture, compare.OutputRecord) — must be listed
// in EXACTLY ONE of limitedSent and limitedNotSent (limitedreport.go). A field nobody classified fails here, so a
// new report field cannot reach author_get_report_limited by default, and cannot slip past without a decision.

var (
	jsonMarshalerType = reflect.TypeOf((*json.Marshaler)(nil)).Elem()
	textMarshalerType = reflect.TypeOf((*encoding.TextMarshaler)(nil)).Elem()
)

// marshalsItself reports whether t (or *t) writes its own JSON: its inner fields are not the report's shape, so
// the walk stops there and the field is one leaf (time.Time, json.RawMessage).
func marshalsItself(t reflect.Type) bool {
	return t.Implements(jsonMarshalerType) || reflect.PointerTo(t).Implements(jsonMarshalerType) ||
		t.Implements(textMarshalerType) || reflect.PointerTo(t).Implements(textMarshalerType)
}

// reportLeaf is one serialised leaf of the report's JSON form: its path (limitedreport.go's spelling) and the
// Go type that produces it.
type reportLeaf struct {
	path string
	typ  reflect.Type
}

// reportLeaves walks t the way encoding/json serialises it and returns every leaf path, plus every shape the
// walk refuses to classify (problems): a map whose values hold structs, or an interface-typed field, whose JSON
// shape the type does not fix.
func reportLeaves(t reflect.Type) (leaves []reportLeaf, problems []string) {
	var walk func(t reflect.Type, path string, depth int)
	walk = func(t reflect.Type, path string, depth int) {
		if depth > 40 {
			problems = append(problems, path+": the type nests deeper than 40 levels (a recursive type?)")
			return
		}
		for t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		if marshalsItself(t) {
			leaves = append(leaves, reportLeaf{path, t})
			return
		}
		switch t.Kind() {
		case reflect.Struct:
			for i := 0; i < t.NumField(); i++ {
				f := t.Field(i)
				tag := f.Tag.Get("json")
				name, _, _ := strings.Cut(tag, ",")
				if tag == "-" {
					continue // not serialised
				}
				if f.Anonymous && name == "" {
					ft := f.Type
					for ft.Kind() == reflect.Pointer {
						ft = ft.Elem()
					}
					if ft.Kind() == reflect.Struct {
						walk(ft, path, depth+1) // an embedded struct's fields are promoted into this object
						continue
					}
				}
				if !f.IsExported() {
					continue
				}
				if name == "" {
					name = f.Name
				}
				walk(f.Type, limitedJoin(path, name), depth+1)
			}
		case reflect.Slice, reflect.Array:
			walk(t.Elem(), path+"[]", depth+1)
		case reflect.Map:
			if holdsStruct(t.Elem()) {
				problems = append(problems, path+": a map whose values hold structs — its keys are data, so its fields cannot be listed by path")
				return
			}
			leaves = append(leaves, reportLeaf{path, t})
		case reflect.Interface:
			problems = append(problems, path+": an interface-typed field — its JSON shape is not fixed by the type")
		default:
			leaves = append(leaves, reportLeaf{path, t})
		}
	}
	walk(t, "", 0)
	return leaves, problems
}

func holdsStruct(t reflect.Type) bool {
	for {
		switch t.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Map:
			t = t.Elem()
			continue
		case reflect.Struct:
			return !marshalsItself(t)
		case reflect.Interface:
			return true
		}
		return false
	}
}

// TestLimitedReport_EveryReportFieldIsClassified is the issue's classification test (AC-F2): every field of the
// report's types is listed as sent or not sent; an unlisted field fails.
func TestLimitedReport_EveryReportFieldIsClassified(t *testing.T) {
	leaves, problems := reportLeaves(reflect.TypeOf(report.Report{}))
	for _, p := range problems {
		t.Errorf("report shape the classification cannot list: %s", p)
	}
	if len(leaves) < 100 {
		// a broken walk that found nothing would pass every check below
		t.Fatalf("the walk found only %d report fields; expected well over 100", len(leaves))
	}

	sent, notSent := map[string]bool{}, map[string]bool{}
	for _, p := range limitedSent {
		if sent[p] {
			t.Errorf("limitedSent lists %q twice", p)
		}
		sent[p] = true
	}
	for _, p := range limitedNotSent {
		if notSent[p] {
			t.Errorf("limitedNotSent lists %q twice", p)
		}
		notSent[p] = true
		if sent[p] {
			t.Errorf("%q is listed as BOTH sent and not sent", p)
		}
	}

	seen := map[string]bool{}
	var unclassified []string
	for _, l := range leaves {
		if seen[l.path] {
			t.Errorf("two report fields serialise to the same path %q", l.path)
		}
		seen[l.path] = true
		switch {
		case sent[l.path]:
			// A sent leaf must have a shape its type fixes: raw JSON can carry anything.
			if l.typ == reflect.TypeOf(json.RawMessage(nil)) {
				t.Errorf("%q is raw JSON (json.RawMessage) and is listed as sent: raw JSON can carry anything", l.path)
			}
		case notSent[l.path]:
		default:
			unclassified = append(unclassified, l.path+" ("+l.typ.String()+")")
		}
	}
	sort.Strings(unclassified)
	for _, u := range unclassified {
		t.Errorf("report field not classified for author_get_report_limited: %s — list it in limitedSent "+
			"(only after checking it can carry nothing the app returned) or in limitedNotSent (limitedreport.go)", u)
	}
	for _, list := range [][]string{limitedSent, limitedNotSent} {
		for _, p := range list {
			if !seen[p] {
				t.Errorf("%q is listed but no report field serialises to it (stale entry, or a container instead of a field)", p)
			}
		}
	}
}

// The marker the "app" put in every field that is not sent. A number field that is not sent carries
// limitedMarkerNum; a sent string field carries "sent:<its path>", so the test can see each one arrive.
const (
	limitedMarker    = "LIMITED-APP-MARKER-7f3a9c"
	limitedMarkerNum = 987654321
)

// fillEveryField sets every serialised field of v, the way the walk above lists them: slices get one element,
// maps one entry, pointers are allocated. A not-sent string gets limitedMarker, a not-sent number
// limitedMarkerNum, raw JSON (always not sent) an object holding limitedMarker.
func fillEveryField(t *testing.T, v reflect.Value, path string, notSent map[string]bool) {
	t.Helper()
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			v.Set(reflect.New(v.Type().Elem()))
		}
		fillEveryField(t, v.Elem(), path, notSent)
		return
	}
	typ := v.Type()
	switch typ {
	case reflect.TypeOf(time.Time{}):
		v.Set(reflect.ValueOf(time.Date(2026, 10, 14, 16, 0, 0, 0, time.UTC)))
		return
	case reflect.TypeOf(json.RawMessage(nil)):
		v.Set(reflect.ValueOf(json.RawMessage(`{"app":"` + limitedMarker + `"}`)))
		return
	}
	switch v.Kind() {
	case reflect.Struct:
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			tag := f.Tag.Get("json")
			name, _, _ := strings.Cut(tag, ",")
			if tag == "-" || !f.IsExported() {
				continue
			}
			if name == "" {
				name = f.Name
			}
			fillEveryField(t, v.Field(i), limitedJoin(path, name), notSent)
		}
	case reflect.Slice:
		s := reflect.MakeSlice(typ, 1, 1)
		fillEveryField(t, s.Index(0), path+"[]", notSent)
		v.Set(s)
	case reflect.Map:
		m := reflect.MakeMap(typ)
		key := reflect.New(typ.Key()).Elem()
		key.SetString("k")
		val := reflect.New(typ.Elem()).Elem()
		fillEveryField(t, val, path, notSent) // a map is one leaf: its value takes the map's own path
		m.SetMapIndex(key, val)
		v.Set(m)
	case reflect.String:
		if notSent[path] {
			v.SetString(limitedMarker)
		} else {
			v.SetString("sent:" + path)
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if notSent[path] {
			v.SetInt(limitedMarkerNum)
		} else {
			v.SetInt(7)
		}
	case reflect.Float32, reflect.Float64:
		if notSent[path] {
			v.SetFloat(limitedMarkerNum)
		} else {
			v.SetFloat(7)
		}
	case reflect.Bool:
		v.SetBool(true)
	default:
		t.Fatalf("fillEveryField: no value for %s at %q", typ, path)
	}
}

// limitedFullReport is a report.Report with every field set (fillEveryField), run id run_lim1, mode build.
func limitedFullReport(t *testing.T) *report.Report {
	t.Helper()
	notSent := map[string]bool{}
	for _, p := range limitedNotSent {
		notSent[p] = true
	}
	rep := &report.Report{}
	fillEveryField(t, reflect.ValueOf(rep).Elem(), "", notSent)
	rep.RunID, rep.Mode = "run_lim1", "build"
	return rep
}

// limitedPaths walks a JSON answer and returns every object-key path in it (containers and leaves alike). It does
// not walk into a listed leaf: a map-typed leaf (faults[].params) is one field, its keys are data.
func limitedPaths(t *testing.T, b []byte) map[string]bool {
	t.Helper()
	var tree any
	if err := json.Unmarshal(b, &tree); err != nil {
		t.Fatalf("answer is not JSON: %v: %s", err, b)
	}
	leaf := limitedKeep().leaf
	out := map[string]bool{}
	var walk func(v any, path string)
	walk = func(v any, path string) {
		switch x := v.(type) {
		case map[string]any:
			for k, val := range x {
				p := limitedJoin(path, k)
				out[p] = true
				if !leaf[p] {
					walk(val, p)
				}
			}
		case []any:
			for _, el := range x {
				walk(el, path+"[]")
			}
		}
	}
	walk(tree, "")
	return out
}

// Done-when 1 and 2, executor half (AC-F2): every field the keep list does not name is left out of the
// get_limited_report answer, the marker the app put in each of them is nowhere in it, and every listed field is
// there. get_full_report on the same run file is the positive control: it still carries the marker.
func TestLimitedReport_EveryNotSentFieldIsLeftOut_EverySentFieldIsKept(t *testing.T) {
	root := writeRunFixture(t, limitedFullReport(t))
	cf := NewCommandFunc(ExecConfig{ToolInstance: "local", ResultsRoot: root})
	args := json.RawMessage(`{"run_id":"run_lim1"}`)

	full, err := cf(context.Background(), "get_full_report", args)
	if err != nil {
		t.Fatalf("get_full_report: %v", err)
	}
	for _, needle := range []string{limitedMarker, "987654321"} {
		if !bytes.Contains(full, []byte(needle)) {
			t.Fatalf("positive control: get_full_report does not carry %q, so the checks below could not fail", needle)
		}
	}

	lim, err := cf(context.Background(), "get_limited_report", args)
	if err != nil {
		t.Fatalf("get_limited_report: %v", err)
	}
	for _, needle := range []string{limitedMarker, "987654321"} {
		if bytes.Contains(lim, []byte(needle)) {
			t.Errorf("get_limited_report carries %q, which the app put only in fields that are not sent", needle)
		}
	}

	keep := limitedKeep()
	for p := range limitedPaths(t, lim) {
		if p == "limited_report" || keep.leaf[p] || keep.enter[p] {
			continue
		}
		t.Errorf("get_limited_report carries the path %q, which is neither a listed field nor on the way to one", p)
	}
	for _, p := range limitedSent {
		if strings.HasSuffix(p, "[]") {
			p = strings.TrimSuffix(p, "[]") // a slice of strings: its elements carry the slice's path
		}
		if !bytes.Contains(lim, []byte(`"sent:`+p)) && !limitedPaths(t, lim)[p] {
			t.Errorf("get_limited_report lost the listed field %q", p)
		}
	}
	var top map[string]any
	if err := json.Unmarshal(lim, &top); err != nil || top["limited_report"] != limitedReportNote {
		t.Errorf("get_limited_report must carry limited_report = the fixed note, got %v (%v)", top["limited_report"], err)
	}
}

// The projection drops a value whose SHAPE the keep list does not expect: a scalar (or anything but an object or an
// array) where the list only enters a container on the way to a leaf. json.Marshal of report.Report cannot produce
// such a tree today; the test pins that a future type change could not turn a container into a sent leaf by accident.
func TestLimitedProject_AValueOfAnUnexpectedShapeIsDropped(t *testing.T) {
	const appText = "APP-TEXT-AT-A-CONTAINER-PATH-6f0d"
	tree := map[string]any{
		"summary": appText, // summary is entered (summary.total …), never a leaf
		"layers": []any{map[string]any{"scenarios": []any{map[string]any{
			"id":      "S-1",
			"failure": appText,                                        // entered for failure.expected
			"steps":   []any{appText},                                 // entered for steps[].name …: an element that is not an object
			"load":    map[string]any{"p95_ms": 7, "errors": appText}, // load.errors is entered for errors[].reason
		}}}},
	}
	out, ok := limitedProject(tree, "", limitedKeep())
	b, _ := json.Marshal(out)
	if !ok || strings.Contains(string(b), appText) {
		t.Fatalf("a value of an unexpected shape was kept: %s", b)
	}
	if !strings.Contains(string(b), `"S-1"`) || !strings.Contains(string(b), `"p95_ms":7`) {
		t.Fatalf("the listed leaves next to it must stay: %s", b)
	}
}

// limitedRefusedAnswer asserts that a get_limited_report answer is a REFUSAL: a status answer (no error), in the shape
// of toolcore.GetReport's not_found map — status "refused", the given reason, a fixed note — and returns it.
// A refusal is never an error on purpose: a command's error text is rewritten by rules about error text (lane b1's
// AC-F1 sends any error it was not built for as one generic sentence), a command's result is not.
func limitedRefusedAnswer(t *testing.T, got json.RawMessage, err error, reason string) map[string]any {
	t.Helper()
	if err != nil {
		t.Fatalf("a refusal must be a status answer, not an error: got error %v", err)
	}
	var m map[string]any
	if uerr := json.Unmarshal(got, &m); uerr != nil {
		t.Fatalf("a refusal must be a JSON object: %v: %s", uerr, got)
	}
	note, _ := m["note"].(string)
	if m["status"] != "refused" || m["reason"] != reason || note == "" {
		t.Fatalf("want status refused, reason %q and a note, got %s", reason, got)
	}
	for k := range m {
		if k != "status" && k != "reason" && k != "note" && k != "run_id" {
			t.Errorf("a refusal carries only status, reason, note and run_id; it has %q: %s", k, got)
		}
	}
	return m
}

// The note on every limited report names what was not sent, the load error's message and a load target's blocked
// reason among them, and what the load fields carry instead (AC-F2.2, Done-when 5): each load error as a word from the
// closed list and a count, a broker pause as memory, disk or other and its times.
func TestLimitedReportNote_SaysWhatWasNotSentAndWhatTheLoadErrorsCarry(t *testing.T) {
	for _, want := range []string{"observed values", "response excerpts", "MCP answers", "sandbox events",
		"recorded comparison outputs", "cleanup output", "Retry-After", "a load error's message",
		"a load target's blocked reason", "build-record hub's reply",
		// AC-F2.1 (Done-when 5): a chain step's code is sent, in its own field
		"a chain http step's status code (steps[].observed_status) and a chain db step's SQLSTATE (steps[].observed_sqlstate",
		"never the database's message) are sent",
		// AC-F2.2 (Done-when 5): words and counts
		"each load error is sent as a word (refused, timeout, reset, unknown_host, tls, protocol or other) and a count",
		"a broker pause as memory, disk or other and its times", "never the load tool's message or the broker's words",
		"argus get-report"} {
		if !strings.Contains(limitedReportNote, want) {
			t.Errorf("limited_report does not say %q: %q", want, limitedReportNote)
		}
	}
	// AC-F2.2: the AC-F2 sentences that said the load error texts are sent are gone
	for _, gone := range []string{"network error texts are sent", "quote a short part of a malformed answer", "160 characters"} {
		if strings.Contains(limitedReportNote, gone) {
			t.Errorf("limited_report still says %q: %q", gone, limitedReportNote)
		}
	}
	// the words in the note are the closed list itself
	for _, w := range report.LoadErrorKinds {
		if !strings.Contains(limitedReportNote, w) {
			t.Errorf("limited_report does not name the word %q", w)
		}
	}
	// AC-F2.1: the AC-F2 sentence that said a chain step's status is not sent is gone
	if strings.Contains(limitedReportNote, "answered status is only in fields") {
		t.Errorf("limited_report still says a chain step's status is not sent: %q", limitedReportNote)
	}
}

// The run id is required and must be one file-name component: a missing, empty or path-shaped one is refused, before
// anything is read, with a status answer that does not echo the refused id.
func TestLimitedReport_RunIDIsRequiredAndChecked(t *testing.T) {
	root := writeRunFixture(t, limitedFullReport(t))
	cf := NewCommandFunc(ExecConfig{ToolInstance: "local", ResultsRoot: root})
	for _, args := range []string{`{}`, `{"run_id":""}`, `{"run_id":"../local/report"}`, `{"run_id":"a/b"}`,
		`{"run_id":"a\\b"}`, `{"run_id":".."}`, `{"run_id":"x..y"}`, `{"run_id":".hidden"}`, `{"run_id":"C:x"}`,
		`{"run_id":7}`, `not json`} {
		got, err := cf(context.Background(), "get_limited_report", json.RawMessage(args))
		m := limitedRefusedAnswer(t, got, err, limitedRefusedRunID)
		if _, echoed := m["run_id"]; echoed {
			t.Errorf("args %s: a refused run id must not be echoed: %s", args, got)
		}
	}
}

// A run id with no report on disk answers the not_found / pending status map, as get_full_report does; a report
// file that cannot be decoded never puts its decoder's words (which can quote the file) into the answer.
func TestLimitedReport_NoReportOrUndecodableReportAnswersArgusWordsOnly(t *testing.T) {
	root := t.TempDir()
	cf := NewCommandFunc(ExecConfig{ToolInstance: "local", ResultsRoot: root})
	got, err := cf(context.Background(), "get_limited_report", json.RawMessage(`{"run_id":"nope"}`))
	if err != nil || !bytes.Contains(got, []byte(`"not_found"`)) {
		t.Fatalf("no report on disk: want the not_found map, got %s, %v", got, err)
	}

	bad := writeRunFixture(t, limitedFullReport(t))
	for _, f := range []string{"local/report.json", "local/runs/run_lim1.json"} {
		if err := os.WriteFile(filepath.Join(bad, filepath.FromSlash(f)), []byte(`{"summary": "`+limitedMarker+`", "layers": 5}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cf = NewCommandFunc(ExecConfig{ToolInstance: "local", ResultsRoot: bad})
	got, err = cf(context.Background(), "get_limited_report", json.RawMessage(`{"run_id":"run_lim1"}`))
	if bytes.Contains(got, []byte(limitedMarker)) || (err != nil && strings.Contains(err.Error(), limitedMarker)) {
		t.Errorf("an undecodable report leaked its content: %s, %v", got, err)
	}
	if err != nil && err != errLimitedUnreadable {
		t.Errorf("an undecodable report must answer a fixed sentence, got %v", err)
	}
}

// A relayed command's error reaches the control plane only as words an Argus site vouched for (AC-F1, ownwords.go);
// anything else becomes one generic sentence. The executor's own failures of get_limited_report are fixed sentences
// of Argus's, so they must reach the author whole and still name the verb.
func TestLimitedReport_ExecutorFailuresReachTheWireAsWritten(t *testing.T) {
	for _, e := range []error{errLimitedUnreadable, errLimitedUnencodable} {
		if got := commandErrorText(e); got != e.Error() {
			t.Errorf("%q is sent to the control plane as %q", e.Error(), got)
		}
		if !strings.HasPrefix(e.Error(), "get_limited_report: ") {
			t.Errorf("the error does not name the verb: %q", e.Error())
		}
	}
}

// The failure sentence tells the author to read the executor's log, so the cause must be in what the executor logs
// (localDetail, which executor.go's runCommands writes when it differs from what is sent), while the wire carries only
// the sentence. A NaN cannot be encoded as JSON: it stands for any encode failure, none of which a report read from
// disk can reach today.
func TestLimitedReport_AnEncodeFailureKeepsItsCauseForTheExecutorsLog(t *testing.T) {
	rep := &report.Report{RunID: "run_nan", ClaimsAsWritten: true, Layers: []report.Layer{{Layer: "Load",
		Scenarios: []report.ScenarioResult{{ID: "L-1", Status: "failed", Load: &report.LoadStats{ErrorRate: math.NaN()}}}}}}
	_, err := limitedReport(rep, "run_nan")
	if err == nil {
		t.Fatal("positive control: a NaN in the report must fail the encode")
	}
	if got := commandErrorText(err); got != errLimitedUnencodable.Error() {
		t.Errorf("the wire must carry only the fixed sentence, got %q", got)
	}
	if d := localDetail(err); d == err.Error() || !strings.Contains(d, "NaN") {
		t.Errorf("the executor's log must hold the cause, but localDetail is %q", d)
	}
}

// limitedOldExecutorReport is a run file shaped like an older executor's (v0.3.71 and before): no ClaimsAsWritten
// stamp, and an mcp chain step whose claim list printed the value saved from the app — on a passed step and on the
// failed step, whose list failure.expected copies.
func limitedOldExecutorReport(stamped bool) *report.Report {
	saved := "SAVED-BY-AN-OLDER-EXECUTOR-3e8a"
	exp := "body has email equals " + saved
	return &report.Report{
		RunID: "run_old", Mode: "build", ClaimsAsWritten: stamped,
		Summary: report.Summary{Total: 1, Failed: 1},
		Layers: []report.Layer{{Layer: "Permissions", Scenarios: []report.ScenarioResult{{
			ID: "OLD-1", Status: "failed",
			Failure: &report.Failure{Expected: &exp, Observed: "step 'read' failed"},
			Steps: []report.StepResult{
				{Name: "write", Status: "passed", AssertionsEnforced: []string{"body has email equals " + saved}, AssertionsEnforcedCount: 1},
				{Name: "read", Status: "failed", AssertionsEnforced: []string{exp}, AssertionsEnforcedCount: 1},
			},
		}}}},
	}
}

// The design review's blocker: a run file an older executor left on the results volume can hold a saved value in
// the very claim fields the keep list sends, and the report no longer knows the value, so it cannot be cut out.
// Such a report is refused whole, with a status answer that holds nothing of it but its (checked) run id; the same
// report WITH the stamp is sent (the positive control: the stamp, and nothing else, decides).
func TestLimitedReport_AReportAnOlderExecutorWroteIsRefusedWhole(t *testing.T) {
	const saved = "SAVED-BY-AN-OLDER-EXECUTOR-3e8a"
	cf := NewCommandFunc(ExecConfig{ToolInstance: "local", ResultsRoot: writeRunFixture(t, limitedOldExecutorReport(false))})
	got, err := cf(context.Background(), "get_limited_report", json.RawMessage(`{"run_id":"run_old"}`))
	m := limitedRefusedAnswer(t, got, err, limitedRefusedOlderExecutor)
	if m["run_id"] != "run_old" {
		t.Errorf("the refusal names the run it refused: %s", got)
	}
	for _, leak := range []string{saved, "OLD-1", "body has email"} {
		if bytes.Contains(got, []byte(leak)) {
			t.Errorf("the refusal carries %q from the report it refused: %s", leak, got)
		}
	}

	cf = NewCommandFunc(ExecConfig{ToolInstance: "local", ResultsRoot: writeRunFixture(t, limitedOldExecutorReport(true))})
	got, err = cf(context.Background(), "get_limited_report", json.RawMessage(`{"run_id":"run_old"}`))
	if err != nil || !bytes.Contains(got, []byte(saved)) || !bytes.Contains(got, []byte(`"claims_as_written":true`)) {
		t.Fatalf("positive control: the same report WITH the stamp must be sent with its claims, got %s, %v", got, err)
	}
}
