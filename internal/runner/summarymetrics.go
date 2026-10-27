package runner

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/federation"
)

// UI-7a (A'3) -- summary numbers from the environment's OWN metrics.
//
// The executor reads a DECLARED source (config.SummaryMetrics: a Prometheus API or a /metrics text
// endpoint) on its own clock and reports ONLY a handful of named numbers on its poll. The raw time series
// are read here, reduced to one number each, and dropped: nothing else of the source's body ever leaves
// this function. An unreadable source is reported as unreadable (Value nil + a reason), never as 0.
//
// AC-F1.1 (#694): the reason is Argus's own sentence, the HTTP status number Argus measured and
// words from fixed lists (promErrorTypes, promResultTypes, the seven network words of report.LoadErrorKinds) —
// never the source's own text and never Go's network error text. Every error here is built with ownWordsf or
// withhold (ownwords.go, AC-F1's rule); one nobody vouched for is sent as summaryReadingNotOwnWords. The full
// text goes only to this executor's own log (SummaryTicker.Cycle).
//
// Bounds: at most federation.SummaryMaxReadings readings, at most one read per `every` (>= 1 minute),
// each read bounded by summaryReadTimeout, each body by summaryBodyMax.

var summaryReadTimeout = 10 * time.Second

const (
	summaryBodyMax    = 8 << 20 // a /metrics page can be large; a Prometheus instant-query answer is tiny
	summaryLineMax    = 1 << 20
	summaryCycleEvery = 30 * time.Second // how often the ticker looks at the clock and the config
)

// ReadSummary reads every declared reading once (at most SummaryMaxReadings, in declared order). Never
// panics, never returns an error: a reading that cannot be read is returned with Value nil and Error set.
func ReadSummary(ctx context.Context, hc *http.Client, sm *config.SummaryMetrics, now time.Time) []federation.SummaryReading {
	out, _ := readSummary(ctx, hc, sm, now)
	return out
}

// readSummary is ReadSummary plus, for each reading, the text for this executor's own log: the full detail of why it
// could not be read ("" for a reading that was read). Only the readings leave; the details never do.
func readSummary(ctx context.Context, hc *http.Client, sm *config.SummaryMetrics, now time.Time) ([]federation.SummaryReading, []string) {
	if sm == nil {
		return nil, nil
	}
	if hc == nil {
		hc = &http.Client{}
	}
	decls := sm.Readings
	if len(decls) > federation.SummaryMaxReadings {
		decls = decls[:federation.SummaryMaxReadings]
	}
	secrets := []string{sm.Source.Credential}
	every := int(sm.EveryDuration() / time.Second)
	out := make([]federation.SummaryReading, 0, len(decls))
	details := make([]string, 0, len(decls))
	for _, d := range decls {
		r := federation.SummaryReading{
			Name: d.Name, Target: d.Target, Unit: d.Unit, ComfortableLimit: d.ComfortableLimit,
			EverySeconds: every, ObservedAt: now,
		}
		rctx, cancel := context.WithTimeout(ctx, summaryReadTimeout)
		var v float64
		var err error
		switch sm.Source.Type {
		case config.SummarySourcePrometheus:
			v, err = readPrometheus(rctx, hc, sm.Source, d.Query)
		case config.SummarySourceMetricsEndpoint:
			v, err = readMetricsText(rctx, hc, sm.Source, d.Query)
		default:
			err = ownWordsf("unknown source type %q", sm.Source.Type) // the operator's own config
		}
		cancel()
		detail := ""
		if err != nil {
			r.Error, detail = summaryErrorTexts(err, secrets)
		} else {
			r.Value = &v
		}
		out = append(out, r)
		details = append(details, detail)
	}
	return out, details
}

// summaryReadingNotOwnWords is sent for an error no code in this file vouched for (fail-closed, as AC-F1's rule).
const summaryReadingNotOwnWords = "the reading failed; its details are in the executor's log"

// summaryErrorTexts is THE rule for SummaryReading.Error: what is sent (the error's own words — Argus's — or the
// fixed sentence, scrubbed and cut to federation.SummaryErrorMax as before) and what this executor logs (the full
// text, with only the configured credential removed and every C0 control character or DEL made a space).
//
// The sent words are scrubbed WITHOUT the credential: they are Argus's own text, so the credential cannot be in them,
// and replacing a credential part that happens to be a word of Argus's sentence ("metrics", "refused", "data") would
// only tell the control plane that part — and break a word of the fixed lists.
func summaryErrorTexts(err error, secrets []string) (words, local string) {
	words = summaryReadingNotOwnWords
	if w, ok := wireWords(err); ok {
		words = w
	}
	words = federation.ScrubReadingError(words)
	if words == "" {
		words = summaryReadingNotOwnWords
	}
	return words, summaryLogText(localDetail(err), secrets)
}

// summaryLogMax is the most of one reading's log text kept (characters). The text is the source's own message and Go's
// error text in full; the bound is there against a flood (Go quotes a malformed header line, and allows 10 MiB of
// them; not measured as a real case). Above it the head and the tail are kept — a stack trace's root cause is at its end.
const summaryLogMax = 16384

// summaryLogText makes the full text safe for one log line: the configured credential becomes <redacted> — as written,
// as it appears inside a quoted text, and in the Basic form this executor sent (a source can echo its Authorization
// header) — and so does each of its ':' parts of 3 bytes or more (the rule ScrubReadingError applies); then a text over
// summaryLogMax is cut in its middle, and a C0 control character (below U+0020) or DEL becomes a space.
func summaryLogText(s string, secrets []string) string {
	for _, sec := range secrets {
		if sec == "" {
			continue
		}
		user, pass, _ := strings.Cut(sec, ":") // exactly as sourceGet sends it: SetBasicAuth(user, pass)
		s = strings.ReplaceAll(s, base64.StdEncoding.EncodeToString([]byte(user+":"+pass)), "<redacted>")
		for _, part := range append([]string{sec}, strings.Split(sec, ":")...) {
			if len(part) >= 3 {
				s = strings.ReplaceAll(s, part, "<redacted>")
				if q := strconv.Quote(part); q[1:len(q)-1] != part {
					s = strings.ReplaceAll(s, q[1:len(q)-1], "<redacted>")
				}
			}
		}
	}
	if r := []rune(s); len(r) > summaryLogMax {
		s = string(r[:summaryLogMax/2]) + fmt.Sprintf(" … (%d characters cut) … ", len(r)-summaryLogMax) +
			string(r[len(r)-summaryLogMax/2:])
	}
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
}

// The fixed lists. Only these words of a source's answer may leave the environment.
var (
	// promErrorTypes are the error types Prometheus itself defines (web/api/v1/api.go, errorType); any other word an
	// answer gives is dropped.
	promErrorTypes = map[string]bool{"timeout": true, "canceled": true, "execution": true, "bad_data": true,
		"internal": true, "unavailable": true, "not_found": true, "not_acceptable": true}
	// promResultTypes are Prometheus's four result types (promql/parser ValueType).
	promResultTypes = map[string]bool{"matrix": true, "vector": true, "scalar": true, "string": true}
)

// answerError is ways A and B: Argus's sentence, then the answer's error type only when it is on promErrorTypes. When the
// answer carried anything else of its own (an error text, an error type off the list), the sentence says the message
// is in the log, and only the log gets it.
func answerError(sentence string, errorType, message json.RawMessage) error {
	typ, typIsString := jsonText(errorType)
	msg, _ := jsonText(message)
	listed := typIsString && promErrorTypes[typ]
	words := sentence
	if listed {
		words += " (" + typ + ")"
	}
	// withheld: an error text, or an error type that is not on the list (a word off it, or a value that is no string)
	if msg == "" && (typ == "" || listed) {
		return ownWordsf("%s", words)
	}
	return withhold(words+"; its own message is in the executor's log",
		fmt.Errorf("the source's own message: errorType %s, error %s", sourceTextForLog(errorType), sourceTextForLog(message)))
}

// jsonText returns a JSON value's text: a string's value (and true), or any other value as written; "" for none or null.
func jsonText(raw json.RawMessage) (string, bool) {
	t := strings.TrimSpace(string(raw))
	if t == "" || t == "null" {
		return "", false
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, true
	}
	return t, false
}

// sourceTextForLog quotes one JSON value of the source's answer for the local log (quoteForLog).
func sourceTextForLog(raw json.RawMessage) string {
	s, _ := jsonText(raw)
	return quoteForLog(s)
}

// quoteForLog quotes a text of the source's for the local log, whole: quoted, so a line break or a control character in
// it cannot start a second log line (summaryLogText bounds the whole line).
func quoteForLog(s string) string {
	return strconv.Quote(s)
}

// promErrorFields pulls Prometheus's own {"errorType","error"} out of an answer, as raw JSON values (nil when absent or
// when the body is not a JSON object).
func promErrorFields(body []byte) (errorType, message json.RawMessage) {
	var e struct {
		ErrorType json.RawMessage `json:"errorType"`
		Error     json.RawMessage `json:"error"`
	}
	if json.Unmarshal(body, &e) != nil {
		return nil, nil
	}
	return e.ErrorType, e.Error
}

// sourceGet performs the GET for either source type and returns the (bounded) body. Every error it returns is Argus's
// own words (ownwords.go): a status the source answered is sent as its number and, from the answer, only a Prometheus
// error type on the list (way A); a transport failure as one network word (way D, transportError). The source's own
// message and Go's error text are withheld — they reach only this executor's log.
func sourceGet(ctx context.Context, hc *http.Client, src config.SummarySource, u string, q url.Values) ([]byte, error) {
	if q != nil {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, ownWordsf("the declared source url is not usable")
	}
	if cred := src.Credential; cred != "" {
		user, pass, _ := strings.Cut(cred, ":")
		req.SetBasicAuth(user, pass)
	}
	req.Header.Set("Accept", "application/json, text/plain;q=0.9")
	tr := &readTrace{base: hc.Transport}
	if tr.base == nil {
		tr.base = http.DefaultTransport
	}
	traced := *hc // the caller's client, with the same transport seen through readTrace
	traced.Transport = tr
	resp, err := traced.Do(req)
	if err != nil {
		s := tr.signals()
		// a non-nil answer with an error: the client refused to follow a redirect (CheckRedirect), its body closed
		s.redirectRefused = resp != nil
		return nil, transportError("could not reach the source", err, s)
	}
	defer resp.Body.Close()
	body, rerr := io.ReadAll(io.LimitReader(resp.Body, summaryBodyMax))
	if resp.StatusCode != http.StatusOK {
		errorType, message := promErrorFields(body)
		return nil, answerError(fmt.Sprintf("the source answered HTTP %d", resp.StatusCode), errorType, message)
	}
	if rerr != nil {
		return nil, transportError("could not read the source's answer", rerr, readSignals{answered: true})
	}
	return body, nil
}

// readSignals is what the LAST request of one read saw, beside its error — they choose the network word where Go's
// error has no type that says it (summarymetrics_neterr.go).
type readSignals struct {
	answered        bool // the first byte of the source's answer arrived: a failure after it is the answer's
	tlsFailed       bool // a TLS handshake failed (httptrace TLSHandshakeDone with an error)
	redirectRefused bool // the client refused to follow a redirect
}

// readTrace is the read's transport seen through httptrace. A redirect makes several requests, each through RoundTrip,
// and each starts a new record: the signals are those of the last request, not of an earlier hop.
type readTrace struct {
	base                http.RoundTripper
	answered, tlsFailed atomic.Bool
}

func (t *readTrace) RoundTrip(r *http.Request) (*http.Response, error) {
	t.answered.Store(false)
	t.tlsFailed.Store(false)
	ctx := httptrace.WithClientTrace(r.Context(), &httptrace.ClientTrace{
		GotFirstResponseByte: func() { t.answered.Store(true) },
		TLSHandshakeDone: func(_ tls.ConnectionState, err error) {
			if err != nil {
				t.tlsFailed.Store(true)
			}
		},
	})
	return t.base.RoundTrip(r.WithContext(ctx))
}

func (t *readTrace) signals() readSignals {
	return readSignals{answered: t.answered.Load(), tlsFailed: t.tlsFailed.Load()}
}

// transportError is way D: a client error becomes Argus's sentence with ONE network word (networkErrorWord), and the
// error itself — Go's text, which names hosts, addresses and certificates of the environment — is withheld for the
// local log. A timeout and a cancellation keep their own sentences. net/http wraps every error as `Get "<url>":
// <cause>`; only the cause is kept, even for the log. s: what the read's last request saw.
func transportError(sentence string, err error, s readSignals) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	var ne net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
		return withhold("timed out reading the source", err)
	}
	if errors.Is(err, context.Canceled) {
		return withhold("read cancelled", err)
	}
	return withhold(sentence+" ("+networkErrorWord(err, s)+"); the details are in the executor's log", err)
}

// readPrometheus runs one instant query and returns its single finite sample.
func readPrometheus(ctx context.Context, hc *http.Client, src config.SummarySource, query string) (float64, error) {
	body, err := sourceGet(ctx, hc, src, strings.TrimRight(src.URL, "/")+"/api/v1/query", url.Values{"query": {query}})
	if err != nil {
		return 0, err
	}
	var res struct {
		Status    string          `json:"status"`
		ErrorType json.RawMessage `json:"errorType"`
		Error     json.RawMessage `json:"error"`
		Data      struct {
			ResultType string          `json:"resultType"`
			Result     json.RawMessage `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &res); err != nil || res.Status == "" {
		return 0, ownWordsf("the source did not answer with a Prometheus query result")
	}
	if res.Status != "success" {
		return 0, answerError("the source reported an error", res.ErrorType, res.Error) // way B
	}
	var pair [2]json.RawMessage
	switch res.Data.ResultType {
	case "scalar":
		if err := json.Unmarshal(res.Data.Result, &pair); err != nil {
			return 0, ownWordsf("malformed scalar result")
		}
	case "vector":
		var vec []struct {
			Value [2]json.RawMessage `json:"value"`
		}
		if err := json.Unmarshal(res.Data.Result, &vec); err != nil {
			return 0, ownWordsf("malformed vector result")
		}
		switch len(vec) {
		case 0:
			return 0, ownWordsf("the query returned no data")
		case 1:
			pair = vec[0].Value
		default:
			return 0, ownWordsf("the query returned %d series; it must reduce to ONE number (wrap it in sum())", len(vec))
		}
	default:
		// way C: the answer's result type is quoted only when it is one of Prometheus's four
		if promResultTypes[res.Data.ResultType] {
			return 0, ownWordsf("the query returned a %q result; it must return one number", res.Data.ResultType)
		}
		return 0, withhold("the query returned a result type Argus does not know; it must return one number",
			fmt.Errorf("the source's result type: %s", quoteForLog(res.Data.ResultType)))
	}
	var s string
	if err := json.Unmarshal(pair[1], &s); err != nil {
		return 0, ownWordsf("malformed sample value")
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, ownWordsf("the sample value is not a number")
	}
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, ownWordsf("the sample value is not finite")
	}
	return v, nil
}

// ---- /metrics text exposition ----------------------------------------------------------------------

var metricNameRE = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*`)

// parseSeries parses `name` or `name{a="b",c="d"}` (a sample line's head, or a declared query) and returns
// the name, its labels and the text after it.
func parseSeries(s string) (name string, labels map[string]string, rest string, ok bool) {
	name = metricNameRE.FindString(s)
	if name == "" {
		return "", nil, "", false
	}
	rest = s[len(name):]
	labels = map[string]string{}
	if !strings.HasPrefix(rest, "{") {
		return name, labels, rest, true
	}
	i := 1
	for {
		for i < len(rest) && (rest[i] == ' ' || rest[i] == ',') {
			i++
		}
		if i >= len(rest) {
			return "", nil, "", false
		}
		if rest[i] == '}' {
			return name, labels, rest[i+1:], true
		}
		eq := strings.IndexByte(rest[i:], '=')
		if eq < 0 {
			return "", nil, "", false
		}
		key := strings.TrimSpace(rest[i : i+eq])
		i += eq + 1
		if i >= len(rest) || rest[i] != '"' {
			return "", nil, "", false
		}
		i++
		var val strings.Builder
		closed := false
		for i < len(rest) {
			c := rest[i]
			if c == '\\' && i+1 < len(rest) {
				switch rest[i+1] {
				case 'n':
					val.WriteByte('\n')
				default:
					val.WriteByte(rest[i+1])
				}
				i += 2
				continue
			}
			if c == '"' {
				closed = true
				i++
				break
			}
			val.WriteByte(c)
			i++
		}
		if !closed || key == "" {
			return "", nil, "", false
		}
		labels[key] = val.String()
	}
}

// readMetricsText fetches a /metrics text body and sums every sample of the declared series (name plus
// optional label matchers). It keeps one number; the body is discarded.
func readMetricsText(ctx context.Context, hc *http.Client, src config.SummarySource, query string) (float64, error) {
	body, err := sourceGet(ctx, hc, src, strings.TrimSpace(src.URL), nil)
	if err != nil {
		return 0, err
	}
	wantName, wantLabels, trail, ok := parseSeries(strings.TrimSpace(query))
	if !ok || strings.TrimSpace(trail) != "" {
		return 0, ownWordsf("the declared query is not a series name with an optional {label=\"value\"} matcher")
	}
	sc := bufio.NewScanner(strings.NewReader(string(body)))
	sc.Buffer(make([]byte, 0, 64<<10), summaryLineMax)
	var sum float64
	matched, parsed := 0, 0
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' {
			continue
		}
		name, labels, rest, ok := parseSeries(line)
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		v, perr := strconv.ParseFloat(fields[0], 64)
		if perr != nil {
			continue
		}
		parsed++
		if name != wantName {
			continue
		}
		hit := true
		for k, want := range wantLabels {
			if got, present := labels[k]; !present || got != want {
				hit = false
				break
			}
		}
		if !hit {
			continue
		}
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return 0, ownWordsf("a matching sample is not finite")
		}
		sum += v
		matched++
	}
	if sc.Err() != nil {
		return 0, ownWordsf("the source body could not be read as metrics text")
	}
	if parsed == 0 {
		return 0, ownWordsf("the source did not answer with metrics text")
	}
	if matched == 0 {
		return 0, ownWordsf("the series was not found in the source")
	}
	return sum, nil
}

// ---- the ticker ------------------------------------------------------------------------------------

// SummaryTicker reads the declared summary readings on its own clock (NOT the poll's: a poll can return
// immediately on a busy instance, and reading must never happen more often than `every`) and hands the
// latest batch to the poll exactly once. It reloads the config on every look so an edit reaches the
// control plane without a restart, like money_handling does.
type SummaryTicker struct {
	path string
	logf func(format string, args ...any)
	hc   *http.Client

	mu       sync.Mutex
	batch    []federation.SummaryReading
	gen      int // bumped on every new batch
	sentGen  int // the gen last handed to a poll
	lastRead time.Time
	lastErr  string
}

// NewSummaryTicker builds a ticker over the SUT's argus-config.yaml path.
func NewSummaryTicker(configPath string, logf func(format string, args ...any)) *SummaryTicker {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &SummaryTicker{path: configPath, logf: logf, hc: &http.Client{}}
}

// Cycle is one look: reload the config; if a summary_metrics block is declared and `every` has elapsed
// since the last read, read every reading and keep the batch for the next poll.
func (t *SummaryTicker) Cycle(ctx context.Context, now time.Time) {
	if t.path == "" {
		return
	}
	cfg, err := config.Load(t.path)
	if err != nil {
		// A config that does not load this tick says nothing new; the last batch stays (and ages to stale
		// on the control plane's side). Logged once per distinct message.
		if msg := federation.ScrubReadingError(err.Error()); msg != t.lastErr {
			t.lastErr = msg
			t.logf("summary_metrics: config not loadable this tick (%s) -- no new readings", msg)
		}
		return
	}
	sm := cfg.SummaryMetricsDecl()
	t.mu.Lock()
	if sm == nil {
		t.batch = nil
		t.mu.Unlock()
		return
	}
	if !t.lastRead.IsZero() && now.Sub(t.lastRead) < sm.EveryDuration() {
		t.mu.Unlock()
		return
	}
	t.lastRead = now
	t.mu.Unlock()

	batch, details := readSummary(ctx, t.hc, sm, now)
	for i, r := range batch {
		if r.Value == nil {
			// the full text, for whoever runs the source: the sent r.Error is only Argus's words (AC-F1.1)
			t.logf("summary_metrics: %s unreadable: %s", r.Name, details[i])
		}
	}
	t.mu.Lock()
	t.batch = batch
	t.gen++
	t.mu.Unlock()
}

// Run drives Cycle until ctx ends. A panic in a read is contained: the poll IS the heartbeat and this
// goroutine must never be able to take the executor down.
func (t *SummaryTicker) Run(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			t.logf("summary_metrics: reader stopped after a panic: %v", r)
		}
	}()
	t.Cycle(ctx, time.Now())
	tk := time.NewTicker(summaryCycleEvery)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tk.C:
			t.Cycle(ctx, now)
		}
	}
}

// Take returns the latest batch if it has not been handed to a poll yet, else nil.
func (t *SummaryTicker) Take() []federation.SummaryReading {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.batch) == 0 || t.sentGen == t.gen {
		return nil
	}
	t.sentGen = t.gen
	return append([]federation.SummaryReading(nil), t.batch...)
}

// Unsend re-arms the batch the last Take returned, after the poll that carried it failed.
func (t *SummaryTicker) Unsend() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.gen > 0 && t.sentGen == t.gen {
		t.sentGen = t.gen - 1
	}
}
