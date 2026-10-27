package runner

// ownwords.go — AC-F1 (#679): the executor sends the control plane only text Argus writes itself in two fields, a
// run's failure reason (ResultsPush.FailureReason) and a relayed command's error text (CommandResult.Error) — never
// the app's own words: not its answer, not its error message, not a value from its answer.
//
// THE RULE, AND WHY IT IS AN ALLOW-LIST
// The failure reason is sent when a run produces no results, and the error text when a relayed command fails; the
// control plane stores both as sent (it only cuts the length). Only the executor can keep the app's text out of them.
// "Only words Argus writes itself" is an allow-list, so the rule is one too:
//
//   - a site that builds an error VOUCHES for its text with ownWordsf — the text is Argus's own: its sentence, the
//     operator's config, a path, a value from the control plane, an image digest;
//   - a lower error that COULD hold the app's text is WITHHELD with withhold (or withholdJSON for a JSON codec error):
//     Argus's fixed sentence is sent, and the lower error stays in this executor's local log (localDetail);
//   - an error no Argus code vouched for is sent as Argus's fixed sentence (failureReason / commandErrorText). A new
//     error path inside this package that nobody classified loses its detail on the wire until someone does.
//
// WHERE THE TEXT COMES FROM ANOTHER PACKAGE (toolcore.Run, the config loader, toolcore.GetReport, the outputs store),
// the classifier vouches only the SHAPES it knows from reading that package at ba9420d7 — runCoreErr, configLoadErr,
// reportReadErr, outputReadErr — and withholds everything else; each known shape is pinned by a test.
//
// Error() of these errors is their WORDS, so a withheld detail cannot ride into a vouched text through a wrap that
// breaks the chain (%v, errors.New(err.Error())). The full text is for this executor's own log only: localDetail.
// errors.Is / errors.As see through both wrappers.
//
// ⚠ go vet treats ownWordsf as an Errorf wrapper (so %w is checked at every call) ONLY while it hands format and args
// to fmt.Errorf unchanged: never assign to them, never build the text with Sprintf.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"reflect"
	"strings"
	"syscall"

	"github.com/OneDro1d/argus-runner/internal/toolcore"
)

// The fixed sentences Argus sends when it cannot send an error's own words. Each says where the detail is, early
// enough that the web Runs list (it shows the first 60 characters) shows what failed.
const (
	reasonNotOwnWords  = "the run failed before it produced results; see this executor's log for why"
	commandNotOwnWords = "this executor could not answer the request; see its log for why"
	reportNotDecodable = "the report on disk could not be decoded; see this executor's log"
	reportNotRead      = "the report on disk could not be read; see this executor's log"
	runCoreNotShown    = "the run stopped with an error this executor does not send; see its log"
	reportNotEncodable = "the run's report could not be encoded; see this executor's log"
	jsonNotShown       = "a JSON read or write failed; its text is in this executor's log"
)

// ownWordsError is an error whose text Argus vouches for (words), kept apart from the full text under it (full,
// for this executor's local log only) and from the chain errors.Is / errors.As walk.
type ownWordsError struct {
	words string // the only text either field may carry; also Error()
	full  string // words with every withheld detail in place: localDetail, never the wire
	chain error  // what Unwrap returns
}

func (e *ownWordsError) Error() string {
	if e == nil {
		return "<nil>"
	}
	return e.words
}

func (e *ownWordsError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.chain
}

// nilErr is an error interface holding a nil pointer — (*fs.PathError)(nil). Its methods may read the receiver, so it
// is never inspected (errors.As would call its Unwrap); it is rendered the way fmt renders it.
func nilErr(err error) bool {
	v := reflect.ValueOf(err)
	return v.Kind() == reflect.Pointer && v.IsNil()
}

// ownWordsf builds an error whose text Argus vouches for: the call site asserts that every byte of the formatted text
// is Argus's own. An error argument is rendered by its own words when it carries some — so a withheld error inside
// stays withheld — and otherwise by its Error(), which this call site vouches for. %w wraps as in fmt.Errorf.
func ownWordsf(format string, args ...any) error {
	wordsArgs := make([]any, len(args))
	fullArgs := make([]any, len(args))
	for i, a := range args {
		wordsArgs[i], fullArgs[i] = a, a
		if err, ok := a.(error); ok && !nilErr(err) {
			if w, ok := wireWords(err); ok {
				wordsArgs[i] = errors.New(w)
			}
			// only an argument that really withholds something is replaced in the local text, so %T, %+v and a
			// Formatter still see the real error everywhere else
			if d := localDetail(err); d != err.Error() {
				fullArgs[i] = errors.New(d)
			}
		}
	}
	return &ownWordsError{
		words: fmt.Errorf(format, wordsArgs...).Error(),
		full:  fmt.Errorf(format, fullArgs...).Error(),
		chain: fmt.Errorf(format, args...),
	}
}

// withhold is a lower error that could hold the app's text: only words — Argus's fixed sentence — may be sent.
func withhold(words string, cause error) error {
	return &ownWordsError{words: words, full: words + ": " + localDetail(cause), chain: cause}
}

// withholdJSON withholds err when its tree holds one of the four typed encoding/json errors (jsonCodecErrs): a JSON
// decoder or encoder can quote a character or a whole number of what it read, and what the executor reads (its
// report, an answer) can hold the app's words. With exactly one codec error whose text ENDS err's text, only the
// codec's part is withheld and the words above it (the steps that wrapped it — "load config: parse <path>: ") stay;
// otherwise — the codec text not at the end, or more than one codec error — the whole of err is withheld. Any other
// err is returned as it is. The classifiers call this only inside a shape they know.
func withholdJSON(err error) error {
	codecs := jsonCodecErrs(err)
	switch {
	case len(codecs) == 0:
		return err
	case len(codecs) > 1:
		return withhold(jsonNotShown, err)
	}
	full, part := err.Error(), codecs[0].Error()
	if strings.HasSuffix(full, part) {
		return withhold(strings.TrimSuffix(full, part)+jsonNotShown, err)
	}
	return withhold(jsonNotShown, err)
}

// jsonCodecErrs returns the encoding/json errors in err's tree that can quote what they read, outermost first, without
// looking inside one (a MarshalerError around a SyntaxError is ONE error). It knows exactly four TYPED ones
// (SyntaxError, UnmarshalTypeError, MarshalerError, UnsupportedValueError). encoding/json also fails with plain errors
// that quote the data ("json: unknown field …", a ",string" tag misuse, an invalid json.Number) and a codec failure
// can reach us behind a %v: none of those is recognised HERE. Under a step word and in the report read the
// classifiers withhold such an error whole; under the two prefix shapes (the config load, no scenario source) it is
// sent with the rest of the text. Like localDetail it checks each level for a nil pointer before calling its Unwrap.
func jsonCodecErrs(err error) []error {
	var found []error
	var walk func(error)
	walk = func(e error) {
		if e == nil || nilErr(e) {
			return
		}
		switch e.(type) {
		case *json.SyntaxError, *json.UnmarshalTypeError, *json.MarshalerError, *json.UnsupportedValueError:
			found = append(found, e)
			return
		}
		switch u := e.(type) {
		case interface{ Unwrap() []error }:
			for _, c := range u.Unwrap() {
				walk(c)
			}
		case interface{ Unwrap() error }:
			walk(u.Unwrap())
		}
	}
	walk(err)
	return found
}

// jsonCodecErr returns the outermost codec error in err's tree (jsonCodecErrs), or nil.
func jsonCodecErr(err error) error {
	if codecs := jsonCodecErrs(err); len(codecs) > 0 {
		return codecs[0]
	}
	return nil
}

// nilInTree reports whether err's tree holds a nil pointer at any level (nilErr). Such a level's methods may read the
// receiver, so errors.Is / errors.As over the tree could panic: a classifier gives such an error Argus's fixed sentence
// before it asks errors.Is / errors.As anything. (Building its text for the local log still calls Error(); a level
// that formats a nil pointer only when asked, such as errors.Join, can still panic there.) Checks each level before
// calling its Unwrap.
func nilInTree(err error) bool {
	if err == nil {
		return false
	}
	if nilErr(err) {
		return true
	}
	switch u := err.(type) {
	case interface{ Unwrap() []error }:
		for _, c := range u.Unwrap() {
			if nilInTree(c) {
				return true
			}
		}
	case interface{ Unwrap() error }:
		return nilInTree(u.Unwrap())
	}
	return false
}

// osError reports whether e is exactly an operating-system error: a syscall.Errno, one of io/fs's sentinels, or
// os.ErrDeadlineExceeded — the text the OS (or Go) wrote, never text a caller put into the error.
func osError(e error) bool {
	if _, ok := e.(syscall.Errno); ok {
		return true
	}
	for _, s := range []error{fs.ErrInvalid, fs.ErrPermission, fs.ErrExist, fs.ErrNotExist, fs.ErrClosed, os.ErrDeadlineExceeded} {
		if e == s {
			return true
		}
	}
	return false
}

// runCoreErr classifies toolcore.Run's error for the run's failure reason. The run core is another package, so its
// error is Argus's only in the shapes this executor KNOWS (read from toolcore at ba9420d7, each pinned by a test):
//
//   - the config load, "load config: …" — the operator's own files: the config's path, lines and keys, and the text of
//     a file a message_schemas entry names (the Avro parser can quote it whole);
//   - no scenario source, toolcore.ErrNoScenarioSource with toolcore's own sentence (an instance label, a path);
//   - a file-system failure directly under one of toolcore's step words: "run: " (the results directory), "write
//     report: ", "write run report: " — the whole text being exactly that step word and a *fs.PathError whose own
//     error is exactly an OS error (osError);
//   - a JSON codec error directly under one of those step words — the whole text being exactly that step word and the
//     codec error; the step word is sent, the codec's text is not.
//
// Inside the first two shapes a JSON codec error is withheld (withholdJSON): only the codec's text, so the shape's
// words stay; the rest of those two texts is vouched by how it starts. The JSON rule never vouches the words above a
// codec error by itself — those words are sent only when the whole text is a known shape. ANY other shape is withheld
// whole: what the app answers becomes a ROW of the report, never this error, and a new error kind that one day says
// something else is not sent until it is added here. An error with a nil pointer anywhere in its tree is withheld
// before errors.Is / errors.As is asked of it. The fail-closed cost is the detail on the wire, never the detail in the
// local log.
func runCoreErr(err error) error {
	if nilInTree(err) {
		return withhold(runCoreNotShown, err)
	}
	msg := err.Error()
	if strings.HasPrefix(msg, "load config: ") {
		return withholdJSON(err)
	}
	if errors.Is(err, toolcore.ErrNoScenarioSource) && strings.HasPrefix(msg, toolcore.ErrNoScenarioSource.Error()+": ") {
		return withholdJSON(err)
	}
	inner := errors.Unwrap(err)
	for _, step := range []string{"run: ", "write report: ", "write run report: "} {
		if inner == nil || msg != step+inner.Error() {
			continue
		}
		if pe, ok := inner.(*fs.PathError); ok && osError(pe.Err) {
			return err
		}
		if codecs := jsonCodecErrs(err); len(codecs) == 1 && codecs[0] == inner {
			return withhold(step+jsonNotShown, err)
		}
	}
	return withhold(runCoreNotShown, err)
}

// ownWordsOf sends only piece's text — a closed sentinel found inside whole — and keeps whole for the local log and
// for errors.Is / errors.As: whatever else whole says is not vouched for.
func ownWordsOf(piece, whole error) error {
	return &ownWordsError{words: piece.Error(), full: localDetail(whole), chain: whole}
}

// wireWords returns the words of the outermost own-words error in err's tree, and whether there is one. Text outside
// it — a plain wrapper around it — is dropped: nobody vouched for that.
func wireWords(err error) (string, bool) {
	if o := outermostOwnWords(err); o != nil {
		return o.words, true
	}
	return "", false
}

// outermostOwnWords finds the first own-words error in err's tree, in the order errors.As walks it, without calling a
// method on a nil pointer (a level that is one ends that branch of the walk).
func outermostOwnWords(err error) *ownWordsError {
	if err == nil || nilErr(err) {
		return nil
	}
	if o, ok := err.(*ownWordsError); ok {
		return o
	}
	switch u := err.(type) {
	case interface{ Unwrap() []error }:
		for _, c := range u.Unwrap() {
			if o := outermostOwnWords(c); o != nil {
				return o
			}
		}
	case interface{ Unwrap() error }:
		return outermostOwnWords(u.Unwrap())
	}
	return nil
}

// localDetail is the whole of err for THIS executor's own log — every withheld detail included, however many sit in
// its tree (errors.Join, two %w). Never the wire.
func localDetail(err error) string {
	if err == nil || nilErr(err) {
		return fmt.Sprint(err)
	}
	if o, ok := err.(*ownWordsError); ok {
		return o.full
	}
	var extra []string
	var walk func(error)
	walk = func(e error) {
		if e == nil || nilErr(e) {
			return
		}
		if o, ok := e.(*ownWordsError); ok {
			if o.full != o.words {
				extra = append(extra, o.full) // its full text already holds everything under it
			}
			return
		}
		switch u := e.(type) {
		case interface{ Unwrap() []error }:
			for _, c := range u.Unwrap() {
				walk(c)
			}
		case interface{ Unwrap() error }:
			walk(u.Unwrap())
		}
	}
	walk(err)
	if len(extra) == 0 {
		return err.Error()
	}
	return err.Error() + " (in full: " + strings.Join(extra, "; ") + ")"
}

// failureReason is THE rule for ResultsPush.FailureReason: the error's own words, or Argus's fixed sentence.
func failureReason(err error) string {
	if err == nil {
		return ""
	}
	if w, ok := wireWords(err); ok {
		return w
	}
	return reasonNotOwnWords
}

// commandErrorText is THE rule for a relayed command's error text (CommandResult.Error): the same, for a command.
func commandErrorText(err error) string {
	if err == nil {
		return ""
	}
	if w, ok := wireWords(err); ok {
		return w
	}
	return commandNotOwnWords
}

// errNotWired answers every relayed command when this executor has no command function.
var errNotWired = ownWordsf("this executor is not wired to answer relayed commands")
