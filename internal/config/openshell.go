package config

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// ── observability.openshell (spec 26, A1 — observe only) ─────────────────────
//
// When the system under test is an AI agent running in an NVIDIA OpenShell sandbox, this block says
// where that sandbox's OCSF events can be read, so every scenario row of report.json can carry what
// the sandbox DENIED during the scenario's padded window (report.ScenarioResult.SandboxPolicy).
//
// ⛔ ABSENT = OFF. With no block there is no query, no wait, and report.json and every evidence hash
// stay byte-identical. OpenShellEvidence() returns nil, and every later pass keys on that.
//
// Unknown keys inside the block are refused BY NAME at load (strictTargetsDoc / targetPlaces), the
// rule targets / rate_limit / package follow: a mistyped key that parsed and reached nothing would
// look supported. Only this block is strict — every other key under `observability` stays lenient.

// OpenShellSourceLoki is the only evidence source in v1: the executor's own Loki (--loki), read with
// a LogQL stream selector. Anything else is refused by name.
const OpenShellSourceLoki = "loki"

// MaxOpenShellWindowPad bounds window_pad at load. The run sleeps once, until the last window's end
// plus one pad, and that window already reaches one pad past the scenario, so the wait is up to two
// pads, holding the run and the instance run lock; the sleep takes no context. A typo such as 87600h
// would hold them for years. 2m is far above the 5s default and Promtail's 1s batch wait (spec 26
// §9 G3); G3's measured delay may move the bound.
const MaxOpenShellWindowPad = 2 * time.Minute

// openShellStreamSelectorRe is a bare LogQL stream selector: one {...} of label matchers
// (label = | != | =~ | !~ "double-quoted value"), with nothing after the closing brace. A line
// filter (!= "Denied") or a pipeline stage (| json | ..., | line_format ...) behind the braces would let
// the config decide what the evidence says: drop every denial while the allowed lines keep the
// liveness rule satisfied, or rewrite every line into a forged OCSF record. The selector only picks
// the stream; the one filter Argus adds (|= "<sandbox id>", obsquery.SandboxLines) is its own.
var openShellStreamSelectorRe = regexp.MustCompile(
	`^\{\s*[A-Za-z_][A-Za-z0-9_]*\s*(=|!=|=~|!~)\s*"(?:[^"\\]|\\.)*"` +
		`(?:\s*,\s*[A-Za-z_][A-Za-z0-9_]*\s*(=|!=|=~|!~)\s*"(?:[^"\\]|\\.)*")*\s*\}$`)

// OpenShellObs is observability.openshell.
type OpenShellObs struct {
	// Source is where the events are read. "loki" only in v1.
	Source string `yaml:"source"`
	// Selector is a LogQL stream selector for the stream that carries the sandbox's OCSF JSONL, e.g.
	// {job="openshell-gateway",source="sandbox-jsonl"} (spec 26 §9 G1 picks the real label). Only
	// the bare selector is accepted, with no line filter or pipeline stage (openShellStreamSelectorRe).
	Selector string `yaml:"selector"`
	// Sandbox is the sandbox ID — OCSF container.uid, usually ${SUT_SANDBOX_UID} — never its name
	// (OpenShell docs/observability/ocsf-json-export.mdx:67; spec 26 C5). Re-creating a sandbox
	// changes the ID. It is resolved from the environment like the credential fields
	// (walkCredentialFields).
	Sandbox string `yaml:"sandbox"`
	// WindowPad is a positive Go duration. Each scenario's window is padded by it on both sides, and
	// the run waits one more pad past the last window before reading (spec 26 A4).
	WindowPad string `yaml:"window_pad"`
}

// validate names every problem in the block, joined with "; " (the betterstack precedent), or nil.
// It runs BEFORE ${VAR} expansion, so a "${SUT_SANDBOX_UID}" literal counts as a value here and an
// unset variable is refused afterwards by expandEnv, naming the field.
func (o *OpenShellObs) validate() error {
	var errs []string
	switch src := strings.TrimSpace(o.Source); {
	case src == "":
		errs = append(errs, "observability.openshell.source is required — accepted: "+OpenShellSourceLoki)
	case src != OpenShellSourceLoki:
		errs = append(errs, fmt.Sprintf("observability.openshell.source: %q is not supported — accepted: %s", o.Source, OpenShellSourceLoki))
	}
	switch sel := strings.TrimSpace(o.Selector); {
	case sel == "":
		errs = append(errs, "observability.openshell.selector is required")
	case !openShellStreamSelectorRe.MatchString(sel):
		errs = append(errs, `observability.openshell.selector must be a LogQL stream selector such as {job="openshell-gateway"}, with no line filter or pipeline stage`)
	}
	if strings.TrimSpace(o.Sandbox) == "" {
		errs = append(errs, "observability.openshell.sandbox is required")
	}
	switch pad := strings.TrimSpace(o.WindowPad); {
	case pad == "":
		errs = append(errs, "observability.openshell.window_pad is required")
	default:
		if d, err := time.ParseDuration(pad); err != nil || d <= 0 {
			errs = append(errs, fmt.Sprintf("observability.openshell.window_pad: %q is not a positive duration (e.g. 5s)", o.WindowPad))
		} else if d > MaxOpenShellWindowPad {
			errs = append(errs, fmt.Sprintf("observability.openshell.window_pad: %q exceeds the maximum of %s (the run waits up to two pads before reading)", o.WindowPad, MaxOpenShellWindowPad))
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return errors.New(strings.Join(errs, "; "))
}

// Pad is the parsed window_pad. Only meaningful on a block that passed validate (Load guarantees
// that); an unparseable value reads as 0.
func (o *OpenShellObs) Pad() time.Duration {
	if o == nil {
		return 0
	}
	d, err := time.ParseDuration(strings.TrimSpace(o.WindowPad))
	if err != nil || d < 0 {
		return 0
	}
	return d
}

// OpenShellEvidence returns the declared observability.openshell block, or nil when it is absent —
// nil means the feature is off and report.json says nothing about it.
func (c *Config) OpenShellEvidence() *OpenShellObs {
	return c.Observability.OpenShell
}
