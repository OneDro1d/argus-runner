package runner

// ownwords_test.go — AC-F1: the rule itself (ownwords.go), and the places that classify a lower error as one that
// could hold the app's text (readReport, writeReport, the relay's report read). The marker test drives the whole
// executor; these pin each piece of the rule on its own, so a weakened rule fails here even where the marker test's
// app cannot reach it.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/argus"
	"github.com/OneDro1d/argus-runner/internal/artifactmeasure"
	"github.com/OneDro1d/argus-runner/internal/federation"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/toolcore"
)

func TestOwnWords_OnlyVouchedWordsLeave(t *testing.T) {
	appErr := errors.New(appMarker + " the app failed")
	for _, c := range []struct {
		name      string
		err       error
		wantWords string // what both fields carry
		fullHas   string // what the local log (Error()) must still carry
	}{
		{"a vouched error is sent as it is",
			ownWordsf("load config: %w", errors.New("parse /cfg.yaml: yaml: line 1")),
			"load config: parse /cfg.yaml: yaml: line 1", "line 1"},
		{"a withheld lower error sends Argus's sentence and keeps the detail local",
			withhold(reportNotDecodable, appErr), reportNotDecodable, appMarker},
		{"a vouched error around a withheld one keeps it withheld",
			ownWordsf("read report: %w", withhold(reportNotDecodable, appErr)), "read report: " + reportNotDecodable, appMarker},
		{"a plain wrapper around own words is not sent",
			fmt.Errorf("%s: %w", appMarker, ownWordsf("run: the step failed")), "run: the step failed", appMarker},
		{"own words beside a plain error (two %w) leave alone",
			fmt.Errorf("%w; %w", appErr, ownWordsf("materialize: disk full")), "materialize: disk full", appMarker},
		{"a joined error carries only the vouched part",
			errors.Join(appErr, ownWordsf("refused: sealed")), "refused: sealed", appMarker},
		{"a vouched format renders a plain-wrapped withheld argument by its words",
			ownWordsf("x: %v", fmt.Errorf("%s %w", appMarker, withhold("w", appErr))), "x: w", appMarker},
		{"a wrap that breaks the chain (%v) cannot carry a withheld detail into vouched words",
			ownWordsf("x: %w", fmt.Errorf("y: %v", withhold("w", appErr))), "x: y: w", "x: y: w"},
		{"a JSON codec error is withheld wherever it sits in the chain",
			ownWordsf("run: %w", withholdJSON(fmt.Errorf("write report: %w", jsonNumberError(t)))), "run: write report: " + jsonNotShown, appNumber},
		{"a JSON codec error NOT at the end of the text withholds the whole of it",
			ownWordsf("run: %w", withholdJSON(fmt.Errorf("decode %w then "+appMarker, jsonNumberError(t)))), "run: " + jsonNotShown, appNumber},
	} {
		t.Run(c.name, func(t *testing.T) {
			for name, got := range map[string]string{"failure reason": failureReason(c.err), "command error": commandErrorText(c.err)} {
				if got != c.wantWords {
					t.Errorf("%s = %q, want %q", name, got, c.wantWords)
				}
				if strings.Contains(got, appMarker) {
					t.Errorf("%s carries the app's words: %q", name, got)
				}
			}
			if !strings.Contains(localDetail(c.err), c.fullHas) {
				t.Errorf("the local text lost the detail %q: %q", c.fullHas, localDetail(c.err))
			}
			if strings.Contains(c.err.Error(), appMarker) && c.err.Error() != localDetail(c.err) {
				t.Errorf("Error() of an error that withholds must not carry the withheld text: %q", c.err.Error())
			}
		})
	}
}

// appNumber is a number the app answered, in a field Argus reads as an int: the JSON decoder quotes it whole.
const appNumber = "7310.4242"

// jsonNumberError is the decoder's own error for appNumber in an int field — it quotes the number.
func jsonNumberError(t *testing.T) error {
	t.Helper()
	var v struct {
		Passed int `json:"passed"`
	}
	err := json.Unmarshal([]byte(`{"passed":`+appNumber+`}`), &v)
	if err == nil || !strings.Contains(err.Error(), appNumber) {
		t.Fatalf("positive control: the JSON decoder no longer quotes the number, so these cases prove nothing: %v", err)
	}
	return err
}

// Every encoding/json error kind that can quote what it read is withheld, wherever it sits in the chain; any other
// error passes through as it is.
func TestWithholdJSON_EveryCodecErrorThatCanQuoteIsWithheld(t *testing.T) {
	var v struct {
		N int `json:"n"`
	}
	syntaxErr := json.Unmarshal([]byte(`{"n":`+appMarker+`}`), &v)
	_, marshalerErr := json.Marshal(struct{ R json.RawMessage }{R: json.RawMessage(appMarker)}) // wraps a SyntaxError
	_, customMarshalerErr := json.Marshal(appTextMarshaler{})                                   // wraps no other JSON error
	_, unsupportedErr := json.Marshal(math.Inf(1))
	for name, err := range map[string]error{
		"SyntaxError": syntaxErr, "UnmarshalTypeError": jsonNumberError(t), "MarshalerError": marshalerErr,
		"MarshalerError around a plain error": customMarshalerErr, "UnsupportedValueError": unsupportedErr,
	} {
		if err == nil {
			t.Fatalf("%s: the JSON codec raised no error, so this case proves nothing", name)
		}
		wrapped := fmt.Errorf("load: %w", err)
		got := failureReason(ownWordsf("run: %w", withholdJSON(wrapped)))
		if !strings.HasPrefix(got, "run: load: ") || !strings.HasSuffix(got, jsonNotShown) || strings.Contains(got, appMarker) || strings.Contains(got, appNumber) {
			t.Errorf("%s was not withheld: %q (the codec said %q)", name, got, err.Error())
		}
	}
	plain := errors.New("open /cfg.yaml: no such file")
	if withholdJSON(plain) != plain {
		t.Error("an error with no JSON codec error in it must pass through unchanged")
	}
}

// appTextMarshaler fails to encode with an error that carries the app's words — the encoder wraps it in a
// *json.MarshalerError and nothing else.
type appTextMarshaler struct{}

func (appTextMarshaler) MarshalJSON() ([]byte, error) { return nil, errors.New(appMarker) }

// The relay's two small classifiers: validate_config's (a JSON codec error is withheld, the config's own text stays)
// and get_output's (only argus.ReadOutputFile's three sentinels are vouched for, byte for byte).
func TestRelayClassifiers_ConfigAndOutput(t *testing.T) {
	if got := commandErrorText(configLoadErr(fmt.Errorf("load config: %w", jsonNumberError(t)))); got != "load config: "+jsonNotShown {
		t.Errorf("a JSON codec error under the config load was sent as %q", got)
	}
	cfgErr := errors.New(`load config: parse /cfg.yaml: yaml: line 1: did not find expected ',' or ']'`)
	if got := commandErrorText(configLoadErr(cfgErr)); got != cfgErr.Error() {
		t.Errorf("the config loader's own text must stay: %q", got)
	}
	for _, s := range []error{argus.ErrOutputNotFound, argus.ErrBadOutputRef, argus.ErrOutputTooLarge} {
		if got := commandErrorText(outputReadErr(s)); got != s.Error() {
			t.Errorf("sentinel %q sent as %q", s.Error(), got)
		}
	}
	other := fmt.Errorf("the stored output is not JSON: %s", appMarker)
	if got := commandErrorText(outputReadErr(other)); got != commandNotOwnWords {
		t.Errorf("an error get_output does not know must not be vouched for: %q", got)
	}
	// get_output's own refusal of a bad reference, through the relay, byte for byte
	cmd := NewCommandFunc(ExecConfig{ResultsRoot: t.TempDir()})
	for _, args := range []string{`not json`, `{"run_id":"r","scenario_id":"S","step":"s"}`,
		`{"run_id":"r","scenario_id":"S","step":"s","sample":0}`, `{"run_id":"../r","scenario_id":"S","step":"s","sample":1}`} {
		_, err := cmd(context.Background(), "get_output", json.RawMessage(args))
		if got := commandErrorText(err); got != argus.ErrBadOutputRef.Error() {
			t.Errorf("get_output %s answered %q, want the closed sentence byte for byte", args, got)
		}
	}
}

// The classifiers for text from other packages send ONLY the shapes they know and withhold every other — including
// codec failures that reach us as types jsonCodecErr does not know, and a known piece with something said around it.
func TestClassifiers_KnownShapesSentEverythingElseWithheld(t *testing.T) {
	pathErr := &fs.PathError{Op: "mkdir", Path: "/results/local/runs", Err: syscall.ENOTDIR}
	_, marshalErr := json.Marshal(struct{ R json.RawMessage }{R: json.RawMessage(appMarker)}) // a MarshalerError around a SyntaxError
	var sv struct{ N int }
	syntaxErr := json.Unmarshal([]byte(`{"N":`+appMarker+`}`), &sv)
	if marshalErr == nil || syntaxErr == nil {
		t.Fatal("positive control: the JSON codec raised no error, so the cases built on it prove nothing")
	}
	var tagged struct {
		N int `json:"n,string"`
	}
	stringTagErr := json.Unmarshal([]byte(`{"n":"`+appMarker+`"}`), &tagged)
	var num struct{ N json.Number }
	numberErr := json.Unmarshal([]byte(`{"N":"`+appMarker+`"}`), &num)
	var addr struct{ A netip.Addr }
	textErr := json.Unmarshal([]byte(`{"A":"`+appMarker+`"}`), &addr)
	_, timeErr := time.Parse(time.RFC3339, appMarker)
	for name, e := range map[string]error{"string tag": stringTagErr, "json.Number": numberErr, "TextUnmarshaler": textErr, "time": timeErr} {
		if e == nil || !strings.Contains(e.Error(), appMarker) {
			t.Fatalf("positive control: the %s failure no longer quotes what it read, so its case proves nothing: %v", name, e)
		}
	}

	known := map[string]error{
		"config load":        fmt.Errorf("load config: %w", errors.New("parse /cfg.yaml: yaml: line 1: did not find expected ',' or ']'")),
		"no scenario source": fmt.Errorf("%w: cannot run — instance %q has no scenarios to run. There are none in %q", toolcore.ErrNoScenarioSource, "local", "/results/materialized/r1"),
		"results dir":        fmt.Errorf("run: %w", pathErr),
		"report write":       fmt.Errorf("write report: %w", pathErr),
		"run report write":   fmt.Errorf("write run report: %w", pathErr),
	}
	for name, e := range known {
		if got := failureReason(ownWordsf("run: %w", runCoreErr(e))); got != "run: "+e.Error() {
			t.Errorf("run core, known shape %q lost Argus's words: %q", name, got)
		}
	}
	// A JSON codec error inside a known shape: only the codec's text is withheld, the shape's words stay.
	for name, c := range map[string]struct {
		err   error
		words string
	}{
		"config load": {fmt.Errorf("load config: parse /cfg.yaml: %w", jsonNumberError(t)), "load config: parse /cfg.yaml: "},
		"no scenario source": {fmt.Errorf("%w: cannot run — %w", toolcore.ErrNoScenarioSource, jsonNumberError(t)),
			toolcore.ErrNoScenarioSource.Error() + ": cannot run — "},
		"results dir":                {fmt.Errorf("run: %w", jsonNumberError(t)), "run: "},
		"report write":               {fmt.Errorf("write report: %w", jsonNumberError(t)), "write report: "},
		"run report write":           {fmt.Errorf("write run report: %w", jsonNumberError(t)), "write run report: "},
		"report encode, step word":   {fmt.Errorf("write report: %w", marshalErr), "write report: "},
		"report encode, config load": {fmt.Errorf("load config: parse /cfg.yaml: %w", marshalErr), "load config: parse /cfg.yaml: "},
	} {
		if got := failureReason(ownWordsf("run: %w", runCoreErr(c.err))); got != "run: "+c.words+jsonNotShown {
			t.Errorf("run core, a JSON error inside the known shape %q was sent as %q", name, got)
		}
	}
	// Two JSON codec errors in one tree: neither can be cut out alone, so the whole text is withheld — also when the
	// first one in the tree is the one that ends the text (fmt orders the wrapped errors by argument, not by place).
	for name, e := range map[string]error{
		"joined":                   fmt.Errorf("load config: %w", errors.Join(jsonNumberError(t), syntaxErr)),
		"the first one at the end": fmt.Errorf("load config: %[2]w then %[1]w", jsonNumberError(t), syntaxErr),
	} {
		if got := failureReason(ownWordsf("run: %w", runCoreErr(e))); got != "run: "+jsonNotShown {
			t.Errorf("run core, two JSON errors under the config load (%s) were sent as %q", name, got)
		}
	}
	unknown := map[string]error{
		"app text inside a path error":                 fmt.Errorf("write report: %w", &fs.PathError{Op: "read", Path: "/r.json", Err: errors.New(appMarker)}),
		"a JSON error inside a path error":             fmt.Errorf("write report: %w", &fs.PathError{Op: "read", Path: "/r.json", Err: fmt.Errorf("decode the answer %s: %w", appMarker, jsonNumberError(t))}),
		"a rename error under a step word":             fmt.Errorf("write report: %w", &os.LinkError{Op: "rename", Old: "/r/a.tmp", New: "/r/a.json", Err: syscall.ENOENT}),
		"app text before a step word and a path error": fmt.Errorf("%s: write report: %w", appMarker, pathErr),
		"a step word, app text, a path error":          fmt.Errorf("write report: %s: %w", appMarker, pathErr),
		"app text before a step word and a JSON error": fmt.Errorf("decode the answer %s write report: %w", appMarker, jsonNumberError(t)),
		"two JSON errors under a step word":            fmt.Errorf("write report: %w", errors.Join(jsonNumberError(t), syntaxErr)),
		"the sentinel wrapped under other text":        fmt.Errorf("decode the answer %s: %w", appMarker, toolcore.ErrNoScenarioSource),
		"the sentinel's words without the sentinel":    fmt.Errorf("%v: %s", toolcore.ErrNoScenarioSource, appMarker),
		"load, but not the config":                     fmt.Errorf("load the answer: %s", appMarker),
		"the config words without their space":         errors.New("load config:" + appMarker),
		"app text above a JSON codec error":            fmt.Errorf("decode the answer %s: %w", appMarker, jsonNumberError(t)),
		"a step word, app text, a JSON error":          fmt.Errorf("write report: %s: %w", appMarker, jsonNumberError(t)),
		"a plain error":                                errors.New(appMarker + " the app failed"),
		"a path error with app text":                   fmt.Errorf("write report: %w (the app said %s)", pathErr, appMarker),
		"a string-tag decode failure":                  fmt.Errorf("write report: %w", stringTagErr),
		"a json.Number decode failure":                 fmt.Errorf("write report: %w", numberErr),
		"a TextUnmarshaler failure":                    fmt.Errorf("write report: %w", textErr),
		"a time parse failure":                         fmt.Errorf("write report: %w", timeErr),
		"a codec error behind a %v":                    fmt.Errorf("write report: %v", jsonNumberError(t)),
		"config words after other text":                fmt.Errorf("%s load config: %w", appMarker, errors.New("x")),
	}
	for name, e := range unknown {
		got := failureReason(ownWordsf("run: %w", runCoreErr(e)))
		if got != "run: "+runCoreNotShown {
			t.Errorf("run core, unknown shape %q was sent: %q", name, got)
		}
		if !strings.Contains(localDetail(ownWordsf("run: %w", runCoreErr(e))), e.Error()) {
			t.Errorf("run core, unknown shape %q lost its detail from the local log", name)
		}
	}

	exact := fmt.Errorf("read report: %w (run first?)", pathErr)
	if got := commandErrorText(reportReadErr(exact)); got != exact.Error() {
		t.Errorf("report read, the known shape lost Argus's words: %q", got)
	}
	for name, e := range map[string]error{
		"a path error with app text":      fmt.Errorf("read report: %w (run first?) %s", pathErr, appMarker),
		"a path error joined to a decode": errors.Join(pathErr, jsonNumberError(t)),
		"a string-tag decode failure":     fmt.Errorf("read report: %w (run first?)", stringTagErr),
		"app text inside a path error":    fmt.Errorf("read report: %w (run first?)", &fs.PathError{Op: "open", Path: "/r.json", Err: errors.New(appMarker)}),
	} {
		if got := commandErrorText(reportReadErr(e)); strings.Contains(got, appMarker) || strings.Contains(got, appNumber) ||
			!strings.HasPrefix(got, "read report: ") || got == e.Error() {
			t.Errorf("report read, %s was sent: %q", name, got)
		}
	}

	around := fmt.Errorf("%w: %s", argus.ErrOutputNotFound, appMarker)
	if got := commandErrorText(outputReadErr(around)); got != argus.ErrOutputNotFound.Error() || !errors.Is(outputReadErr(around), argus.ErrOutputNotFound) {
		t.Errorf("get_output must send only the sentinel's text, never what is said around it: %q", got)
	}
	if got := commandErrorText(configLoadErr(errors.New(appMarker))); got != commandNotOwnWords {
		t.Errorf("validate_config, an error that is not the config load was sent: %q", got)
	}
	for name, e := range map[string]error{
		"app text above a JSON codec error":  fmt.Errorf("validate %s: %w", appMarker, jsonNumberError(t)),
		"config words after other text":      fmt.Errorf("%s load config: %w", appMarker, errors.New("x")),
		"load, but not the config":           fmt.Errorf("load the answer: %s", appMarker),
		"the config words without the space": errors.New("load config:" + appMarker),
	} {
		if got := commandErrorText(configLoadErr(e)); got != commandNotOwnWords {
			t.Errorf("validate_config, %s was sent: %q", name, got)
		}
	}
}

// The run function vouches toolcore.Run's error as a whole — except a JSON codec error, which can quote what the run
// read: the core is stubbed to fail the way a report encode error would.
func TestNewRunFunc_AJSONErrorFromTheRunCoreIsWithheld(t *testing.T) {
	jerr := jsonNumberError(t)
	prev := toolcoreRun
	toolcoreRun = func(toolcore.Env, string, string, string, string) (any, bool, error) {
		return nil, false, fmt.Errorf("write report: %w", jerr)
	}
	t.Cleanup(func() { toolcoreRun = prev })
	_, err := NewRunFunc(ExecConfig{ResultsRoot: t.TempDir()})(context.Background(), &federation.RunAssignment{
		RunID: "run-json", Scope: "full", Scenarios: []federation.ScenarioPayload{{Path: "a/S1.md", Body: "# S1"}}})
	if err == nil {
		t.Fatal("the stubbed core's error was lost")
	}
	if got := failureReason(err); got != "run: write report: "+jsonNotShown {
		t.Errorf("failure reason = %q", got)
	}
	if !strings.Contains(localDetail(err), appNumber) {
		t.Errorf("the local log must keep the detail: %q", localDetail(err))
	}
}

// The helpers survive what fmt.Errorf survives: a nil pointer inside an error interface never panics them.
func TestOwnWords_ANilPointerErrorDoesNotPanic(t *testing.T) {
	var pe *fs.PathError
	var ow *ownWordsError
	for name, f := range map[string]func(){
		"ownWordsf with a nil *fs.PathError": func() { _ = ownWordsf("x: %w", error(pe)).Error() },
		"failureReason of a nil own-words":   func() { _ = failureReason(error(ow)) },
		"commandErrorText of a nil own-words": func() {
			_ = commandErrorText(error(ow))
		},
		"localDetail of a nil own-words": func() { _ = localDetail(error(ow)) },
		"runCoreErr of a nil *fs.PathError": func() {
			_ = failureReason(ownWordsf("run: %w", runCoreErr(error(pe))))
		},
		"reportReadErr of a nil *fs.PathError": func() { _ = commandErrorText(reportReadErr(error(pe))) },
	} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("%s panicked: %v", name, r)
				}
			}()
			f()
		}()
	}
}

// The nil branches of the four classifiers and of the two field rules are part of the rule, so their words are pinned
// too, not only their not panicking: through them, a nil pointer inside an error interface is sent as Argus's
// sentence, never as fmt's "<nil>". (ownWordsf itself renders a nil-pointer argument the way fmt does, "x: <nil>".)
func TestOwnWords_ANilPointerErrorIsSentAsArgussSentence(t *testing.T) {
	var pe *fs.PathError
	var ow *ownWordsError
	for name, c := range map[string]struct{ got, want string }{
		"failureReason of a nil own-words":     {failureReason(error(ow)), reasonNotOwnWords},
		"commandErrorText of a nil own-words":  {commandErrorText(error(ow)), commandNotOwnWords},
		"runCoreErr of a nil *fs.PathError":    {failureReason(ownWordsf("run: %w", runCoreErr(error(pe)))), "run: " + runCoreNotShown},
		"reportReadErr of a nil *fs.PathError": {commandErrorText(reportReadErr(error(pe))), "read report: " + reportNotRead},
		"configLoadErr of a nil *fs.PathError": {commandErrorText(configLoadErr(error(pe))), commandNotOwnWords},
		"outputReadErr of a nil *fs.PathError": {commandErrorText(outputReadErr(error(pe))), commandNotOwnWords},
	} {
		if c.got != c.want {
			t.Errorf("%s: sent %q, want %q", name, c.got, c.want)
		}
	}
}

// A nil pointer one wrap down is just as safe: the rule never calls a method on it (errors.Is / errors.As would call its
// Unwrap), and Argus's sentence is sent.
func TestOwnWords_AWrappedNilPointerErrorIsSentAsArgussSentence(t *testing.T) {
	var pe *fs.PathError
	for name, c := range map[string]struct {
		send func() string
		want string
	}{
		"failureReason":    {func() string { return failureReason(fmt.Errorf("x: %w", error(pe))) }, reasonNotOwnWords},
		"commandErrorText": {func() string { return commandErrorText(fmt.Errorf("x: %w", error(pe))) }, commandNotOwnWords},
		"runCoreErr": {func() string {
			return failureReason(ownWordsf("run: %w", runCoreErr(fmt.Errorf("run: %w", error(pe)))))
		}, "run: " + runCoreNotShown},
		"reportReadErr": {func() string {
			return commandErrorText(reportReadErr(fmt.Errorf("read report: %w (run first?)", error(pe))))
		}, "read report: " + reportNotRead},
		"configLoadErr": {func() string { return commandErrorText(configLoadErr(fmt.Errorf("load config: %w", error(pe)))) }, commandNotOwnWords},
		"outputReadErr": {func() string { return commandErrorText(outputReadErr(fmt.Errorf("x: %w", error(pe)))) }, commandNotOwnWords},
	} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("%s of a wrapped nil *fs.PathError panicked: %v", name, r)
				}
			}()
			if got := c.send(); got != c.want {
				t.Errorf("%s of a wrapped nil *fs.PathError: sent %q, want %q", name, got, c.want)
			}
		}()
	}
}

// The local log keeps EVERY withheld detail in a tree, and %T of an error argument still names its real type.
func TestLocalDetail_KeepsEveryWithheldDetailAndTheArgumentsType(t *testing.T) {
	a := withhold("first sentence", errors.New(appMarker+"-A"))
	b := withhold("second sentence", errors.New(appMarker+"-B"))
	for name, e := range map[string]error{"errors.Join": errors.Join(a, b), "two %w": fmt.Errorf("%w; %w", a, b)} {
		got := localDetail(e)
		if !strings.Contains(got, appMarker+"-A") || !strings.Contains(got, appMarker+"-B") {
			t.Errorf("%s: the local detail lost a withheld part: %q", name, got)
		}
		if w := commandErrorText(e); strings.Contains(w, appMarker) {
			t.Errorf("%s: the wire carries a withheld part: %q", name, w)
		}
	}
	pe := &fs.PathError{Op: "open", Path: "/x", Err: errors.New("no such file")}
	if got := localDetail(ownWordsf("got %T", pe)); got != "got *fs.PathError" {
		t.Errorf("%%T of an error argument = %q", got)
	}
}

func TestOwnWords_AnErrorNobodyVouchedForIsSentAsArgussSentence(t *testing.T) {
	plain := fmt.Errorf("check failed: %s", appMarker)
	if got := failureReason(plain); got != reasonNotOwnWords {
		t.Errorf("failure reason = %q, want Argus's sentence", got)
	}
	if got := commandErrorText(plain); got != commandNotOwnWords {
		t.Errorf("command error = %q, want Argus's sentence", got)
	}
	if failureReason(nil) != "" || commandErrorText(nil) != "" {
		t.Error("no error must be no text")
	}
}

func TestOwnWords_IsAndAsSeeThroughBothWrappers(t *testing.T) {
	nf := ownWordsf("%w", argus.ErrOutputNotFound)
	if !errors.Is(nf, argus.ErrOutputNotFound) || nf.Error() != argus.ErrOutputNotFound.Error() || commandErrorText(nf) != argus.ErrOutputNotFound.Error() {
		t.Errorf("a vouched sentinel must keep its identity and its exact text: %q / %q", nf.Error(), commandErrorText(nf))
	}
	mm := ownWordsf("%w", &artifactmeasure.MismatchError{Declared: "sha256:a", Running: []string{"sha256:b"}})
	var me *artifactmeasure.MismatchError
	if !errors.As(mm, &me) || me.Declared != "sha256:a" {
		t.Error("errors.As must still find the refusal inside")
	}
	cause := errors.New("cause")
	if !errors.Is(withhold("w", cause), cause) {
		t.Error("errors.Is must still find the withheld cause")
	}
}

// readReport decodes a file that holds the app's answers. Its decode error can quote a whole number literal from
// it, so a decode failure is withheld; a file-system failure (a path) is Argus's text and stays.
func TestReadReport_ADecodeErrorIsWithheldAPathErrorIsNot(t *testing.T) {
	dir := t.TempDir()
	damaged := filepath.Join(dir, "report.json")
	if err := os.WriteFile(damaged, []byte(`{"summary":{"passed":`+appNumber+`}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := readReport(damaged)
	if err == nil {
		t.Fatal("the damaged report decoded")
	}
	if !strings.Contains(localDetail(err), appNumber) {
		t.Fatalf("positive control: the decoder's own text no longer quotes the number, so this case proves nothing: %v", localDetail(err))
	}
	if got := failureReason(ownWordsf("read report: %w", err)); got != "read report: "+reportNotDecodable {
		t.Errorf("failure reason = %q", got)
	}
	_, err = readReport(filepath.Join(dir, "missing.json"))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a missing file must stay a file-system error: %v", err)
	}
	if got := failureReason(ownWordsf("read report: %w", err)); !strings.Contains(got, filepath.Join(dir, "missing.json")) {
		t.Errorf("a missing file must keep Argus's words with the path: %q", got)
	}
}

// writeReport encodes a report that holds the app's answers; an encode error is withheld.
func TestWriteReport_AnEncodeErrorIsWithheld(t *testing.T) {
	rep := &report.Report{Layers: []report.Layer{{Scenarios: []report.ScenarioResult{{ID: "S",
		MCPEnvelope: json.RawMessage(appMarker + " is not JSON")}}}}}
	err := writeReport(filepath.Join(t.TempDir(), "report.json"), rep)
	if err == nil {
		t.Fatal("a report holding invalid JSON encoded")
	}
	if got := failureReason(ownWordsf("write report: %w", err)); got != "write report: "+reportNotEncodable {
		t.Errorf("failure reason = %q", got)
	}
}

// The relayed get_report / get_full_report read the same file: a decode error is withheld, a missing file keeps
// toolcore's own words and the path.
func TestRelay_GetReportDecodeErrorIsWithheld(t *testing.T) {
	root := t.TempDir()
	cmd := NewCommandFunc(ExecConfig{ResultsRoot: root})
	for _, verb := range []string{"get_report", "get_full_report"} {
		_, err := cmd(context.Background(), verb, json.RawMessage(`{}`))
		if !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("%s with no report must stay a file-system error: %v", verb, err)
		}
		if got := commandErrorText(err); !strings.Contains(got, "read report") || !strings.Contains(got, filepath.Join(root, "local", "report.json")) || !strings.Contains(got, "(run first?)") {
			t.Errorf("%s with no report must keep toolcore's own words and the path: %q", verb, got)
		}
	}
	if err := os.MkdirAll(filepath.Join(root, "local"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "local", "report.json"), []byte(`{"summary":{"passed":`+appNumber+`}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, verb := range []string{"get_report", "get_full_report"} {
		_, err := cmd(context.Background(), verb, json.RawMessage(`{}`))
		if err == nil || !strings.Contains(localDetail(err), appNumber) {
			t.Fatalf("positive control: %s's local error does not quote the number, so this case proves nothing: %v", verb, localDetail(err))
		}
		if got := commandErrorText(err); strings.Contains(got, appNumber) || !strings.Contains(got, reportNotDecodable) {
			t.Errorf("%s over a damaged report: %q", verb, got)
		}
	}
}

// CommandResult takes the error, not a string, and sends only its words.
func TestCommandResult_SendsOnlyTheWords(t *testing.T) {
	var got []federation.CommandResult
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var res federation.CommandResult
		_ = json.NewDecoder(r.Body).Decode(&res)
		got = append(got, res)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	c := tallyTestClient(t, srv.URL)
	ctx := context.Background()
	_ = c.CommandResult(ctx, "c1", nil, withhold(reportNotDecodable, errors.New(appMarker)))
	_ = c.CommandResult(ctx, "c2", nil, fmt.Errorf("plain %s", appMarker))
	_ = c.CommandResult(ctx, "c3", json.RawMessage(`{"ok":true}`), nil)
	_ = c.CommandResult(ctx, "c4", nil, errNotWired)
	want := []string{reportNotDecodable, commandNotOwnWords, "", "this executor is not wired to answer relayed commands"}
	if len(got) != len(want) {
		t.Fatalf("got %d answers", len(got))
	}
	for i, w := range want {
		if got[i].Error != w || strings.Contains(got[i].Error, appMarker) {
			t.Errorf("%s: error %q, want %q", got[i].CommandID, got[i].Error, w)
		}
	}
	if string(got[2].Result) != `{"ok":true}` {
		t.Errorf("the result must pass unchanged: %s", got[2].Result)
	}
}
