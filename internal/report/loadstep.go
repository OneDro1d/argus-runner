package report

// loadstep.go -- the per-step record of an `AMQP Load` ramp (, design DESIGN-AMQP §4.1).
//
// ⛔ THE SHAPE IS FROZEN. PR-E builds the wire column, the ledger store, author_get_run_status and the
// Pushgateway families AGAINST these field names; renaming or re-typing one breaks a PR that is being
// written in parallel. New fields may only be ADDED, with `omitempty`.
//
// ⛔ CUSTODY (UI-REWORK DESIGN §0.1 / AMQP design §4.5). These are MEASUREMENTS of what the broker did:
// session counts, rates, microsecond quantiles, error classes by count, blocked windows. They carry NO
// threshold (the declared targets live in the scenario's `## LOAD`, which is holdout material), no
// credential, no broker address and no message content. They are author/product-hat readable like every
// other observed number, and they are NEVER copied into a builder-token projection (RelayedReport has
// no field for them; a test pins that).

// LoadStep status values. A step is `measured` when the ramp reached it and read it; the other three say
// why it holds no usable measurement.
const (
	LoadStepMeasured    = "measured"     // the step ran and its window was read
	LoadStepBlocked     = "blocked"      // the broker blocked the publisher (connection.blocked) during the step
	LoadStepSetupFailed = "setup_failed" // a session could not be set up (connect / declare / bind / consume)
	LoadStepNotRun      = "not_run"      // the ramp stopped before this step
)

// Quantiles are nearest-rank percentiles in MICROSECONDS (the JTL `elapsed` is whole milliseconds, too
// coarse for a sub-10ms p99; the sampler writes the real figure into responseMessage as `us=`).
type Quantiles struct {
	Min int64 `json:"min"`
	P50 int64 `json:"p50"`
	P75 int64 `json:"p75"`
	P95 int64 `json:"p95"`
	P99 int64 `json:"p99"`
	Max int64 `json:"max"`
}

// BlockedPeriod is one stretch during which the broker held the publisher blocked (a memory/disk alarm).
// UntilMs == 0 means the step ended while the broker was still blocking.
type BlockedPeriod struct {
	SinceMs int64  `json:"since_ms"`
	UntilMs int64  `json:"until_ms"`
	Reason  string `json:"reason,omitempty"`
}

// LoadRampEntry is one load scenario's ramp as it rides the wire and sits in the ledger
// (ResultsPush.load_ramp is a JSON ARRAY of these, one per load scenario: a run may carry a scenario per
// target, AMQP design 4.3). It is the SAME LoadStep record wrapped with the two
// identifiers a reader needs to say which ramp it is: the scenario id and the NAMED target (never its
// URL). Measurements only, so every custody rule on LoadStep applies unchanged.
type LoadRampEntry struct {
	ScenarioID string     `json:"scenario_id"`
	Target     string     `json:"target,omitempty"` // the named target, NOT its URL
	Driver     string     `json:"driver"`           // "amqp" | "http"
	Steps      []LoadStep `json:"steps"`
	// StoppedAtStep names the 1-based step at which an HTTP ramp STOPPED because that step was
	// not comfortable: the step that broke. 0 = the ramp did not stop for that reason (it ran every step, or an
	// AMQP ramp, which never sets it).
	StoppedAtStep int `json:"stopped_at_step,omitempty"`
}

// LoadStep is one rung of the ramp.
type LoadStep struct {
	Step             int             `json:"step"`           // 1-based
	Sessions         int             `json:"sessions"`       // JMeter threads = AMQP sessions
	Status           string          `json:"status"`         // measured | blocked | setup_failed | not_run
	WindowSeconds    float64         `json:"window_seconds"` // the steady window (ramp excluded)
	OfferedPerS      float64         `json:"offered_per_s"`  // publish ATTEMPTS / window
	SentPerS         float64         `json:"sent_per_s"`     // publishes handed to the broker without error
	ConfirmedPerS    float64         `json:"confirmed_per_s"`
	DeliveredPerS    float64         `json:"delivered_per_s"`
	DeliveredRatio   float64         `json:"delivered_ratio"`              // delivered / sent
	PublishConfirmUs *Quantiles      `json:"publish_confirm_us,omitempty"` // nil unless Confirm == each
	PublishDeliverUs *Quantiles      `json:"publish_deliver_us,omitempty"` // publish -> deliver
	Errors           map[string]int  `json:"errors,omitempty"`             // by class (closed set)
	Blocked          []BlockedPeriod `json:"blocked,omitempty"`            // see BlockedPeriod
	BlockedSeconds   float64         `json:"blocked_seconds,omitempty"`    //
	GeneratorLimited bool            `json:"generator_limited,omitempty"`  // offered < 90% of sessions*rate: the GENERATOR was the limit
	RestartsDelta    *int            `json:"restarts_delta,omitempty"`     // absent = not measured (no SUT read Role)
	NotReady         *int            `json:"not_ready,omitempty"`          // absent = not measured
	Comfortable      bool            `json:"comfortable"`
	// GeneratorCPUThrottledShare (HTTP Load, #615) is the share (0..1) of the executor's own CFS
	// periods in which its cgroup was CPU-throttled during this step's JMeter run (cpu.stat nr_throttled /
	// nr_periods, after minus before). nil = not measured; then GeneratorNotMeasured says so. A share above
	// httpload.GeneratorThrottleBound sets GeneratorLimited: the executor, not the target, was the limit.
	GeneratorCPUThrottledShare *float64 `json:"generator_cpu_throttled_share,omitempty"`
	GeneratorNotMeasured       bool     `json:"generator_not_measured,omitempty"` // cpu.stat could not be read or did not advance: no value is invented
	// ResponseUs (HTTP Load) is the request -> response time of every request of the steady
	// window, in microseconds (the JTL `elapsed` x 1000). nil on an AMQP step, whose latency is PublishDeliverUs.
	ResponseUs *Quantiles `json:"response_us,omitempty"`
	// ErrorReasons (HTTP Load) are the transport failures of the window grouped by reason, exactly the plain
	// `## LOAD` record's `errors` (report.LoadErrorsFrom: at most 8, URL-reduced and credential-scrubbed at the
	// source). Errors keeps the closed class counts; this says why. Absent when there were none.
	ErrorReasons []LoadError `json:"error_reasons,omitempty"`
}
