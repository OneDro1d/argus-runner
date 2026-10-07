package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeCfg writes an argus-config and loads it, returning the load error verbatim.
func loadCfgText(t *testing.T, body string) (*Config, error) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "argus-config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return Load(p)
}

const rlBase = "project:\n  name: t\ntargets:\n  mcp:\n    base_url: http://sut:9000/mcp\n"

// VR10-R1-1: the block is TOP-LEVEL and carries requests / per / signature, and (optionally) a
// pause block with the three caps.
func TestRateLimit_ParsesTheDeclaredBlock(t *testing.T) {
	c, err := loadCfgText(t, rlBase+`rate_limit:
  requests: 120
  per: minute
  signature:
    body_contains: '"error":"rate_limited"'
    retry_after_field: retry_after
  pause:
    max_pauses: 1
    max_total_wait: 45s
    retry_once: false
`)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	rl := c.RateLimit
	if rl == nil {
		t.Fatal("rate_limit block was not read")
	}
	if rl.Requests != 120 || rl.Per != "minute" {
		t.Errorf("requests/per = %d/%q; want 120/minute", rl.Requests, rl.Per)
	}
	if rl.Signature.BodyContains != `"error":"rate_limited"` || rl.Signature.RetryAfterField != "retry_after" {
		t.Errorf("signature = %+v", rl.Signature)
	}
	if rl.MaxPauses() != 1 || rl.MaxTotalWait() != 45*time.Second || rl.RetryOnce() {
		t.Errorf("pause overrides not read: pauses=%d wait=%s retryOnce=%v", rl.MaxPauses(), rl.MaxTotalWait(), rl.RetryOnce())
	}
}

// VR10-R1-8: the caps have global defaults — 3 pauses / 180s / retry-once — and an instance that
// declares no `pause:` block gets exactly those.
func TestRateLimit_PauseDefaults(t *testing.T) {
	c, err := loadCfgText(t, rlBase+"rate_limit:\n  requests: 5\n  per: second\n  signature:\n    body_contains: slow down\n")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	rl := c.RateLimit
	if rl.MaxPauses() != 3 {
		t.Errorf("max_pauses default = %d; want 3", rl.MaxPauses())
	}
	if rl.MaxTotalWait() != 180*time.Second {
		t.Errorf("max_total_wait default = %s; want 180s", rl.MaxTotalWait())
	}
	if !rl.RetryOnce() {
		t.Error("retry_once default = false; want true")
	}
	// SA §0.14 R1-h: no retry_after from the SUT → pause for the declared window.
	if rl.Window() != time.Second {
		t.Errorf("per: second → window %s; want 1s", rl.Window())
	}
}

func TestRateLimit_MinuteWindow(t *testing.T) {
	c, err := loadCfgText(t, rlBase+"rate_limit:\n  requests: 120\n  per: minute\n  signature:\n    body_contains: x\n")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.RateLimit.Window() != time.Minute {
		t.Errorf("per: minute → window %s; want 1m", c.RateLimit.Window())
	}
}

// VR10-R1-1: `per` accepts minute|second and NOTHING else — refused by name at load.
func TestRateLimit_RefusesUnknownPer(t *testing.T) {
	_, err := loadCfgText(t, rlBase+"rate_limit:\n  requests: 10\n  per: hour\n  signature:\n    body_contains: x\n")
	if err == nil {
		t.Fatal("per: hour was accepted; it must be refused")
	}
	if !strings.Contains(err.Error(), "rate_limit.per") || !strings.Contains(err.Error(), "hour") ||
		!strings.Contains(err.Error(), "minute") || !strings.Contains(err.Error(), "second") {
		t.Errorf("the refusal must name the key, the bad value and the accepted ones: %v", err)
	}
}

// VR10-R1-1: a limit of zero (or a missing `requests`) is a malformed declaration, not a limit.
func TestRateLimit_RefusesNonPositiveRequests(t *testing.T) {
	_, err := loadCfgText(t, rlBase+"rate_limit:\n  per: minute\n  signature:\n    body_contains: x\n")
	if err == nil {
		t.Fatal("rate_limit with no `requests` was accepted")
	}
	if !strings.Contains(err.Error(), "rate_limit.requests") {
		t.Errorf("the refusal must name rate_limit.requests: %v", err)
	}
}

// VR10-R1-1 / V28-012: an unknown key under rate_limit (or under pause) is refused BY NAME with the
// accepted list — the same rule `targets` got, for the same reason: a silently-dropped key looks
// supported.
func TestRateLimit_RefusesUnknownKeyByName(t *testing.T) {
	for _, tc := range []struct{ name, body, key, place string }{
		{"under rate_limit", "rate_limit:\n  requests: 10\n  per: minute\n  retry_afterr: 5\n", "retry_afterr", "rate_limit"},
		{"under pause", "rate_limit:\n  requests: 10\n  per: minute\n  pause:\n    max_pause: 2\n", "max_pause", "rate_limit.pause"},
		{"under signature", "rate_limit:\n  requests: 10\n  per: minute\n  signature:\n    body_contain: x\n", "body_contain", "rate_limit.signature"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadCfgText(t, rlBase+tc.body)
			if err == nil {
				t.Fatalf("unknown key %q was accepted", tc.key)
			}
			if !strings.Contains(err.Error(), tc.key) || !strings.Contains(err.Error(), tc.place) {
				t.Errorf("refusal must name the key and the place (%s under %s): %v", tc.key, tc.place, err)
			}
		})
	}
}

// VR10-R1-3: with no rate_limit block nothing changes — the config loads and RateLimit is nil, so
// every downstream check is off.
func TestRateLimit_AbsentBlockIsNil(t *testing.T) {
	c, err := loadCfgText(t, rlBase)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.RateLimit != nil {
		t.Fatalf("no rate_limit declared, yet the config carries one: %+v", c.RateLimit)
	}
}
