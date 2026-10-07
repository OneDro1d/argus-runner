package runner

import (
	"strings"
	"sync"
	"time"

	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/toolcore"
)

// SUTProbeEvery is the floor between two SUT dials (VR6-W1, SA §0.4).
//
// It is deliberately NOT the poll's cadence, because the poll does not have one. The executor long-polls:
// a poll returns when work arrives or after a ~25 s timeout, so on a busy instance "once per poll" would
// dial the SUT continuously. HeartbeatEvery (30 s) is the mid-run heartbeat — a different mechanism on a
// different trigger, and reusing it would couple the SUT's dial rate to whether a run happens to be in
// flight.
const SUTProbeEvery = 60 * time.Second

// sutProbeTimeout bounds each individual dial. ProbeTargets dials concurrently, so this bounds the whole
// call rather than the sum. Short on purpose: this runs on the path of the heartbeat.
const sutProbeTimeout = 2 * time.Second

// sutProbe answers "is this instance's SUT reachable?" on its own clock, and caches the answer between
// dials so any number of polls can carry it (VR6-W1).
//
// ⚠ BEST-EFFORT AND NON-FATAL, which is the load-bearing property. The poll IS the heartbeat (D-FED.4).
// If a flaky SUT could break the poll, a struggling SUT would make its own executor read as STALE — and
// it would do so through the very channel the operator needs in order to see the SUT problem. So Observe
// never returns an error, and a probe that panics is survived and reported as NOT MEASURED.
//
// `now` and `probe` are fields rather than direct calls so the cadence is testable without sleeping.
type sutProbe struct {
	every time.Duration
	now   func() time.Time
	probe func() *bool
	// logf matches the Executor's injectable Log — this package has no package-level logger. nil is
	// silent, which is what the tests want and what a probe with nothing to say should be.
	logf func(string, ...any)

	mu        sync.Mutex
	last      time.Time
	reachable *bool
	checkedAt time.Time
}

// newSUTProbe wires the real dialler. A nil config disables the probe entirely — reporting NOT MEASURED
// rather than guessing, which is the same answer ARGUS_VALIDATE_NO_PROBE produces.
func newSUTProbe(cfg *config.Config) *sutProbe {
	p := &sutProbe{every: SUTProbeEvery, now: time.Now}
	if cfg != nil {
		p.probe = func() *bool {
			return toolcore.SUTReachableTriState(toolcore.ProbeTargets(cfg, sutProbeTimeout))
		}
	}
	return p
}

// Observe returns the best current answer and WHEN it was measured, dialling first if the window has
// elapsed. Both returns are nil until a probe has actually run.
//
// ⚠ A CACHED ANSWER KEEPS ITS ORIGINAL TIMESTAMP. Restamping it as current would make a 20-hour-old
// reading indistinguishable from a fresh one, and the page's whole ability to grey out a stale green
// depends on that distinction.
func (p *sutProbe) Observe() (*bool, *time.Time) {
	if p == nil {
		return nil, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	every := p.every
	if every <= 0 {
		// Never "probe on every poll" — that is §0.4's failure mode arriving through a forgotten field
		// rather than through a decision.
		every = SUTProbeEvery
	}
	nowFn := p.now
	if nowFn == nil {
		nowFn = time.Now
	}
	now := nowFn()

	if p.probe != nil && (p.last.IsZero() || now.Sub(p.last) >= every) {
		p.last = now
		// A fresh unknown REPLACES a cached true. Holding a stale true because the current answer is "I
		// could not tell" is exactly how a dead SUT stays green — memstore-compose, two hours, zero error
		// lines.
		p.reachable = p.runOnce()
		p.checkedAt = now
	}
	if p.checkedAt.IsZero() {
		return nil, nil
	}
	at := p.checkedAt
	return p.reachable, &at
}

// runOnce isolates the panic boundary. A malformed SUT config must cost this instance its SUT colour,
// never its heartbeat.
func (p *sutProbe) runOnce() (out *bool) {
	defer func() {
		if r := recover(); r != nil {
			if p.logf != nil {
				p.logf("sut probe panicked; reporting NOT MEASURED rather than a verdict: %v", r)
			}
			out = nil
		}
	}()
	return p.probe()
}

// sutConfigFor loads the SUT's argus-config.yaml for DIALLING, or returns nil so the probe reports NOT
// MEASURED (VR6-W1).
//
// ⚠ config.Load, NOT config.ParseUnresolved. The deployment watcher a few lines away in runner.go
// deliberately uses ParseUnresolved, so an unexpanded ${VAR} elsewhere in the file cannot block it. The
// probe must not copy that choice: it DIALS what it reads, and an unresolved config yields the literal
// text "${DB_HOST}" as a hostname. That dial fails, and the page would then show the SUT UNREACHABLE on
// the strength of a missing environment variable on the EXECUTOR — a red verdict about the wrong subject
// entirely, which is the mistake this requirement exists to stop making in the other direction.
//
// Every failure returns nil rather than a partial config. NOT MEASURED is a state the page renders and
// explains; a half-read config is a source of confident wrong answers.
func sutConfigFor(path string) *config.Config {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	c, err := config.Load(path)
	if err != nil {
		return nil
	}
	return c
}
