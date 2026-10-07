package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Spec 26 P1 (A1, observe only): the optional observability.openshell block. Every test here loads a
// real file through Load/ParseUnresolved, the way validate-config and a run do, so what is asserted is
// what an operator sees.

const osBlock = `observability:
  openshell:
    source: loki
    selector: '{job="openshell-gateway",source="sandbox-jsonl"}'
    sandbox: sb-0123
    window_pad: 5s
`

func TestOpenShellObs_ParsesFullBlock(t *testing.T) {
	c, err := loadCfgText(t, rlBase+osBlock)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	o := c.OpenShellEvidence()
	if o == nil {
		t.Fatal("observability.openshell was declared but OpenShellEvidence() is nil")
	}
	if o.Source != "loki" || o.Selector != `{job="openshell-gateway",source="sandbox-jsonl"}` || o.Sandbox != "sb-0123" || o.WindowPad != "5s" {
		t.Errorf("block read as %+v", *o)
	}
	if o.Pad() != 5*time.Second {
		t.Errorf("Pad() = %s, want 5s", o.Pad())
	}
}

// Invariant 1: no block, no feature. A nil accessor is what every later pass keys on.
func TestOpenShellObs_AbsentIsOff(t *testing.T) {
	c, err := loadCfgText(t, rlBase+"observability:\n  loki:\n    url: http://loki:3100\n")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if o := c.OpenShellEvidence(); o != nil {
		t.Fatalf("no observability.openshell block, but OpenShellEvidence() = %+v", *o)
	}
}

func TestOpenShellObs_UnknownSourceRefusedByName(t *testing.T) {
	body := strings.Replace(rlBase+osBlock, "source: loki", "source: gateway-jsonl", 1)
	_, err := loadCfgText(t, body)
	want := `observability.openshell.source: "gateway-jsonl" is not supported — accepted: loki`
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("want the refusal %q, got %v", want, err)
	}
}

func TestOpenShellObs_RequiredKeysRefusedByName(t *testing.T) {
	for _, key := range []string{"source", "selector", "sandbox", "window_pad"} {
		t.Run(key, func(t *testing.T) {
			var kept []string
			for _, line := range strings.Split(osBlock, "\n") {
				if strings.HasPrefix(strings.TrimSpace(line), key+":") {
					continue
				}
				kept = append(kept, line)
			}
			_, err := loadCfgText(t, rlBase+strings.Join(kept, "\n"))
			want := "observability.openshell." + key + " is required"
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("without %s: want an error containing %q, got %v", key, want, err)
			}
		})
	}
}

// Whitespace is not a value: a blank selector would query `{}`-less nonsense and a blank sandbox would
// match every line.
func TestOpenShellObs_BlankValuesAreMissing(t *testing.T) {
	body := strings.Replace(rlBase+osBlock, "sandbox: sb-0123", `sandbox: "   "`, 1)
	_, err := loadCfgText(t, body)
	if err == nil || !strings.Contains(err.Error(), "observability.openshell.sandbox is required") {
		t.Fatalf("a whitespace-only sandbox must be refused as missing, got %v", err)
	}
}

func TestOpenShellObs_SelectorMustBeAStreamSelector(t *testing.T) {
	body := strings.Replace(rlBase+osBlock, `selector: '{job="openshell-gateway",source="sandbox-jsonl"}'`, `selector: 'job="openshell-gateway"'`, 1)
	_, err := loadCfgText(t, body)
	want := `observability.openshell.selector must be a LogQL stream selector such as {job="openshell-gateway"}`
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("want %q, got %v", want, err)
	}
}

// A selector with a line filter or a pipeline stage behind the braces would let a config decide what
// the evidence says: `!= "Denied"` drops every denial and keeps the allowed lines the liveness rule
// needs (so the block reads `complete` with nothing denied), and `line_format` rewrites every line into
// whatever OCSF record it likes. Only a bare stream selector is accepted, refused by name otherwise.
func TestOpenShellObs_SelectorWithPipelineRefused(t *testing.T) {
	for _, sel := range []string{
		`{job="openshell-gateway"} != "Denied"`,
		`{job="openshell-gateway"} | json | action_id != "2"`,
		`{job="openshell-gateway"} | line_format "{\"class_uid\":4001}"`,
		`{job="openshell-gateway"} |= "sb-1"`,
		`{job="openshell-gateway"}{job="x"}`,
		`{}`,
		`{job=openshell-gateway}`,
		`sum(count_over_time({job="openshell-gateway"}[1m]))`,
	} {
		t.Run(sel, func(t *testing.T) {
			body := strings.Replace(rlBase+osBlock, `selector: '{job="openshell-gateway",source="sandbox-jsonl"}'`, `selector: '`+sel+`'`, 1)
			_, err := loadCfgText(t, body)
			want := `observability.openshell.selector must be a LogQL stream selector such as {job="openshell-gateway"}, with no line filter or pipeline stage`
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("selector %s: want %q, got %v", sel, want, err)
			}
		})
	}
}

// Guard (green before and after): the stream selectors an operator really writes still load — every
// matcher operator, spaces around the parts, and a quoted value holding `}`, `|`, `,` or an escaped quote.
func TestOpenShellObs_StreamSelectorsAccepted(t *testing.T) {
	for _, sel := range []string{
		`{job="openshell-gateway"}`,
		`{job="openshell-gateway",source="sandbox-jsonl"}`,
		`{ job = "openshell-gateway" , source =~ "sandbox-.*" }`,
		`{job="openshell-gateway",source!="gateway-stream",host!~"ci-.*"}`,
		`{job="a|b}, c",note="say \"hi\""}`,
	} {
		t.Run(sel, func(t *testing.T) {
			body := strings.Replace(rlBase+osBlock, `selector: '{job="openshell-gateway",source="sandbox-jsonl"}'`, `selector: '`+sel+`'`, 1)
			c, err := loadCfgText(t, body)
			if err != nil {
				t.Fatalf("selector %s must load: %v", sel, err)
			}
			if got := c.OpenShellEvidence().Selector; got != sel {
				t.Fatalf("selector read as %q, want %q", got, sel)
			}
		})
	}
}

func TestOpenShellObs_WindowPadMustBePositive(t *testing.T) {
	for _, v := range []string{"0s", "-1s", "5", "soon"} {
		t.Run(v, func(t *testing.T) {
			body := strings.Replace(rlBase+osBlock, "window_pad: 5s", "window_pad: "+v, 1)
			_, err := loadCfgText(t, body)
			want := `observability.openshell.window_pad: "` + v + `" is not a positive duration (e.g. 5s)`
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("want %q, got %v", want, err)
			}
		})
	}
}

// The run sleeps until the last window's end plus one pad, and every window reaches one more pad past
// it, so a typo such as 87600h would hold the run (and the instance run lock) for years. The pad is
// capped at load, by name, until G3 measures the real delay.
func TestOpenShellObs_WindowPadAboveCapRefused(t *testing.T) {
	for _, v := range []string{"87600h", "2m1s", "10m"} {
		t.Run(v, func(t *testing.T) {
			body := strings.Replace(rlBase+osBlock, "window_pad: 5s", "window_pad: "+v, 1)
			_, err := loadCfgText(t, body)
			want := `observability.openshell.window_pad: "` + v + `" exceeds the maximum of 2m0s`
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("want %q, got %v", want, err)
			}
		})
	}
	// The cap itself is accepted.
	c, err := loadCfgText(t, strings.Replace(rlBase+osBlock, "window_pad: 5s", "window_pad: 2m", 1))
	if err != nil {
		t.Fatalf("window_pad 2m (the cap) must load: %v", err)
	}
	if c.OpenShellEvidence().Pad() != 2*time.Minute {
		t.Fatalf("Pad() = %s, want 2m", c.OpenShellEvidence().Pad())
	}
}

// Every problem is named in one refusal, not the first one only (the betterstack precedent).
func TestOpenShellObs_AllProblemsNamedAtOnce(t *testing.T) {
	_, err := loadCfgText(t, rlBase+"observability:\n  openshell:\n    source: syslog\n    window_pad: soon\n")
	if err == nil {
		t.Fatal("a block with four problems loaded")
	}
	for _, want := range []string{"openshell.source", "openshell.selector", "openshell.sandbox", "openshell.window_pad"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must name %s, got %v", want, err)
		}
	}
}

// the evidence read goes to the executor's --loki. A BetterStack instance has
// no Loki, so the block would say `unavailable` on every row of every run. Refused at load instead.
func TestOpenShellObs_RefusedBesideBetterStack(t *testing.T) {
	body := rlBase + `observability:
  betterstack:
    query_url: https://eu.example.test
    credential: ${BS_CRED}
    team_id: "123"
    sources:
      api: api
  openshell:
    source: loki
    selector: '{job="openshell-gateway"}'
    sandbox: sb-0123
    window_pad: 5s
`
	t.Setenv("BS_CRED", "u:p")
	_, err := loadCfgText(t, body)
	want := "observability.openshell reads Loki; this config selects observability.betterstack, which has no Loki to read"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("want %q, got %v", want, err)
	}
}

// Finding A1 (spec 26 §11): unknown keys under observability.openshell are refused BY NAME, with the
// line and the accepted keys — the rule targets / rate_limit / package already follow.
func TestOpenShellObs_UnknownKeyRefusedByName(t *testing.T) {
	body := strings.Replace(rlBase+osBlock, "source: loki", "sorce: loki\n    source: loki", 1)
	_, err := loadCfgText(t, body)
	want := `unknown key "sorce" under observability.openshell — accepted: source, selector, sandbox, window_pad`
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("want %q, got %v", want, err)
	}
	if !strings.Contains(err.Error(), "line 8:") {
		t.Errorf("the refusal must carry the line of the bad key (line 8), got %v", err)
	}
}

// Guard (green before and after): strictness is widened to observability.openshell ONLY. Every other
// key under observability stays lenient, exactly as cleanup_enabled_leftover_test.go pins for
// `scenarios`. If this goes red, the HOW-TO strictness list is wrong.
func TestOpenShellObs_OtherObservabilityKeysStayLenient(t *testing.T) {
	_, err := loadCfgText(t, rlBase+`observability:
  loki:
    url: http://loki:3100
    not_a_loki_key: 1
  future_block:
    anything: goes
`)
	if err != nil {
		t.Fatalf("unknown keys outside observability.openshell must still load: %v", err)
	}
}

// Finding A2 (spec 26 §11): ${VAR} expands only in the fields walkCredentialFields lists. Without an
// entry the literal "${SUT_SANDBOX_UID}" would be the sandbox id and match nothing.
func TestOpenShellObs_SandboxVarResolved(t *testing.T) {
	t.Setenv("SUT_SANDBOX_UID", "sb-123")
	c, err := loadCfgText(t, rlBase+strings.Replace(osBlock, "sandbox: sb-0123", "sandbox: ${SUT_SANDBOX_UID}", 1))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := c.OpenShellEvidence().Sandbox; got != "sb-123" {
		t.Fatalf("sandbox = %q, want the resolved sb-123", got)
	}
}

func TestOpenShellObs_SandboxVarUnsetFailsLoud(t *testing.T) {
	t.Setenv("SUT_SANDBOX_UID", "") // set-but-empty is missing too (expandEnv)
	_, err := loadCfgText(t, rlBase+strings.Replace(osBlock, "sandbox: sb-0123", "sandbox: ${SUT_SANDBOX_UID}", 1))
	if err == nil || !strings.Contains(err.Error(), "SUT_SANDBOX_UID (referenced by observability.openshell.sandbox)") {
		t.Fatalf("an unset ${SUT_SANDBOX_UID} must fail the load naming the variable and the field, got %v", err)
	}
}

// The onboarding preflight (secrets-scan, doctor) learns which names to load from EnvRefs.
func TestOpenShellObs_EnvRefsListsSandboxVar(t *testing.T) {
	p := filepath.Join(t.TempDir(), "argus-config.yaml")
	if err := os.WriteFile(p, []byte(rlBase+strings.Replace(osBlock, "sandbox: sb-0123", "sandbox: ${SUT_SANDBOX_UID}", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := ParseUnresolved(p)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, r := range c.EnvRefs() {
		if r.Name == "SUT_SANDBOX_UID" && r.Field == "observability.openshell.sandbox" {
			return
		}
	}
	t.Fatalf("EnvRefs() = %+v, want SUT_SANDBOX_UID under observability.openshell.sandbox", c.EnvRefs())
}
