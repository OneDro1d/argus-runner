package runner

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/federation"
)

// UI-7a (A'3) -- summary numbers from the environment's OWN metrics.
//
// The executor reads a DECLARED source (config.SummaryMetrics: a Prometheus API or a /metrics text
// endpoint) on its own clock and reports ONLY a handful of named numbers on its poll. The raw time series
// are read here, reduced to one number each, and dropped: nothing else of the source's body ever leaves
// this function. An unreadable source is reported as unreadable (Value nil + a scrubbed reason), never as 0.
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
	if sm == nil {
		return nil
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
			err = fmt.Errorf("unknown source type %q", sm.Source.Type)
		}
		cancel()
		if err != nil {
			r.Error = federation.ScrubReadingError(err.Error(), secrets...)
			if r.Error == "" {
				r.Error = "unreadable"
			}
		} else {
			r.Value = &v
		}
		out = append(out, r)
	}
	return out
}

// sourceGet performs the GET for either source type and returns the (bounded) body. Every error it
// returns is already free of the URL: transport errors are reduced to their cause, and the caller scrubs
// once more before anything is sent or logged.
func sourceGet(ctx context.Context, hc *http.Client, src config.SummarySource, u string, q url.Values) ([]byte, error) {
	if q != nil {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, errors.New("the declared source url is not usable")
	}
	if cred := src.Credential; cred != "" {
		user, pass, _ := strings.Cut(cred, ":")
		req.SetBasicAuth(user, pass)
	}
	req.Header.Set("Accept", "application/json, text/plain;q=0.9")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, transportError(err)
	}
	defer resp.Body.Close()
	body, rerr := io.ReadAll(io.LimitReader(resp.Body, summaryBodyMax))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("source answered HTTP %d%s", resp.StatusCode, promErrorHint(body))
	}
	if rerr != nil {
		return nil, transportError(rerr)
	}
	return body, nil
}

// transportError reduces a client error to its cause without the request URL (net/http wraps every error
// as `Get "<url>": <cause>`).
func transportError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	var ne net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
		return errors.New("timed out reading the source")
	}
	if errors.Is(err, context.Canceled) {
		return errors.New("read cancelled")
	}
	return fmt.Errorf("could not reach the source: %v", err)
}

// promErrorHint pulls Prometheus's own {"errorType","error"} out of a non-200 body, if it is one.
func promErrorHint(body []byte) string {
	var e struct {
		ErrorType string `json:"errorType"`
		Error     string `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil && (e.ErrorType != "" || e.Error != "") {
		return " (" + e.ErrorType + ": " + e.Error + ")"
	}
	return ""
}

// readPrometheus runs one instant query and returns its single finite sample.
func readPrometheus(ctx context.Context, hc *http.Client, src config.SummarySource, query string) (float64, error) {
	body, err := sourceGet(ctx, hc, src, strings.TrimRight(src.URL, "/")+"/api/v1/query", url.Values{"query": {query}})
	if err != nil {
		return 0, err
	}
	var res struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Data   struct {
			ResultType string          `json:"resultType"`
			Result     json.RawMessage `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &res); err != nil || res.Status == "" {
		return 0, errors.New("the source did not answer with a Prometheus query result")
	}
	if res.Status != "success" {
		return 0, fmt.Errorf("the source reported an error: %s", res.Error)
	}
	var pair [2]json.RawMessage
	switch res.Data.ResultType {
	case "scalar":
		if err := json.Unmarshal(res.Data.Result, &pair); err != nil {
			return 0, errors.New("malformed scalar result")
		}
	case "vector":
		var vec []struct {
			Value [2]json.RawMessage `json:"value"`
		}
		if err := json.Unmarshal(res.Data.Result, &vec); err != nil {
			return 0, errors.New("malformed vector result")
		}
		switch len(vec) {
		case 0:
			return 0, errors.New("the query returned no data")
		case 1:
			pair = vec[0].Value
		default:
			return 0, fmt.Errorf("the query returned %d series; it must reduce to ONE number (wrap it in sum())", len(vec))
		}
	default:
		return 0, fmt.Errorf("the query returned a %q result; it must return one number", res.Data.ResultType)
	}
	var s string
	if err := json.Unmarshal(pair[1], &s); err != nil {
		return 0, errors.New("malformed sample value")
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, errors.New("the sample value is not a number")
	}
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, errors.New("the sample value is not finite")
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
		return 0, errors.New("the declared query is not a series name with an optional {label=\"value\"} matcher")
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
			return 0, errors.New("a matching sample is not finite")
		}
		sum += v
		matched++
	}
	if sc.Err() != nil {
		return 0, errors.New("the source body could not be read as metrics text")
	}
	if parsed == 0 {
		return 0, errors.New("the source did not answer with metrics text")
	}
	if matched == 0 {
		return 0, errors.New("the series was not found in the source")
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

	batch := ReadSummary(ctx, t.hc, sm, now)
	for _, r := range batch {
		if r.Value == nil {
			t.logf("summary_metrics: %s unreadable: %s", r.Name, r.Error) // Error is already scrubbed
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
