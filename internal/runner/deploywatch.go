package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// FingerprintFunc returns the SUT's CURRENT deployment fingerprint — an opaque string that stays stable
// across a deployment and CHANGES when the SUT is redeployed (a build id / commit / version). The HTTP
// probe (HTTPFingerprint) is the default source; tests inject a stub.
type FingerprintFunc func(ctx context.Context) (string, error)

// DeploymentWatcher implements UC073: it periodically reads the SUT's deployment fingerprint and, when it
// CHANGES, produces a deployment marker for the executor to push. It never emits on the first observation
// (that only establishes the baseline) nor on an unchanged fingerprint — a spurious marker would falsely
// reset the Current SUT State. The baseline is optionally persisted to a cache file so a redeploy that
// happens while the executor is DOWN is still detected on restart.
//
// It is socket-free by construction (D-FED.4): the fingerprint comes from an HTTP GET to a SUT endpoint the
// executor can already reach, NOT from the docker/k8s API. If the SUT declares no probe, the watcher is
// simply not created and `argus mark-deployment` (UC074) remains the manual fallback.
type DeploymentWatcher struct {
	Fingerprint FingerprintFunc  // how to read the SUT's current fingerprint
	MinInterval time.Duration    // minimum gap between probes (default 15s); 0 = probe every Check
	Now         func() time.Time // injectable clock (default time.Now)

	mu        sync.Mutex
	lastSeen  string
	have      bool // whether lastSeen holds a real baseline
	lastProbe time.Time
	cachePath string // optional persistence of the baseline across restarts
}

// NewDeploymentWatcher builds a watcher over fp. When cachePath is non-empty it loads a persisted baseline
// (so a redeploy during downtime is detected on the first Check) and persists every observed fingerprint.
func NewDeploymentWatcher(fp FingerprintFunc, cachePath string) *DeploymentWatcher {
	w := &DeploymentWatcher{Fingerprint: fp, MinInterval: 15 * time.Second, Now: time.Now, cachePath: cachePath}
	if cachePath != "" {
		if b, err := os.ReadFile(cachePath); err == nil {
			if s := strings.TrimSpace(string(b)); s != "" {
				w.lastSeen, w.have = s, true
			}
		}
	}
	return w
}

// Check reads the current fingerprint (respecting MinInterval) and returns a marker + changed=true iff the
// SUT was redeployed since the last observed fingerprint. A probe error returns (nil,false,err) and leaves
// the baseline intact (a transient failure must not look like a redeploy). An empty/unusable fingerprint is
// ignored (nil,false,nil).
func (w *DeploymentWatcher) Check(ctx context.Context) (marker json.RawMessage, changed bool, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	now := w.now()
	if w.MinInterval > 0 && !w.lastProbe.IsZero() && now.Sub(w.lastProbe) < w.MinInterval {
		return nil, false, nil // gated — don't probe yet
	}
	w.lastProbe = now // gate BEFORE the call so errors don't cause a tight retry loop

	cur, ferr := w.Fingerprint(ctx)
	if ferr != nil {
		return nil, false, ferr // baseline untouched — retry next cycle
	}
	cur = strings.TrimSpace(cur)
	if cur == "" {
		return nil, false, nil // no usable fingerprint this cycle
	}
	if !w.have { // first observation: baseline only, no marker
		w.lastSeen, w.have = cur, true
		w.persist(cur)
		return nil, false, nil
	}
	if cur == w.lastSeen {
		return nil, false, nil
	}
	// CHANGE detected — the SUT was redeployed.
	w.lastSeen = cur
	w.persist(cur)
	m, _ := json.Marshal(map[string]any{
		"at":          now.UTC().Format(time.RFC3339),
		"source":      "auto",
		"fingerprint": cur,
	})
	return m, true, nil
}

func (w *DeploymentWatcher) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

func (w *DeploymentWatcher) persist(fp string) {
	if w.cachePath == "" {
		return
	}
	_ = os.MkdirAll(filepath.Dir(w.cachePath), 0o755)
	_ = os.WriteFile(w.cachePath, []byte(fp), 0o644)
}

// HTTPFingerprint builds a FingerprintFunc that GETs probeURL and extracts the deployment fingerprint from
// the JSON response field `field` (e.g. "version", "commit", "build_id"). A non-200, a non-JSON body, or a
// missing field yields "" (no fingerprint this cycle → no auto-detect) rather than an error, so a SUT whose
// health body lacks the field simply gets no auto-markers instead of log spam. Whole-body hashing is
// deliberately NOT used — health bodies carry volatile fields (uptime/time) that would fire false markers.
func HTTPFingerprint(probeURL, field string, hc *http.Client) FingerprintFunc {
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	return func(ctx context.Context) (string, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, probeURL, nil)
		if err != nil {
			return "", err
		}
		resp, err := hc.Do(req)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return "", fmt.Errorf("deployment probe %s: status %d", probeURL, resp.StatusCode)
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		var m map[string]any
		if json.Unmarshal(body, &m) != nil {
			return "", nil // non-JSON body — unusable, not an error
		}
		v, ok := m[field]
		if !ok || v == nil {
			return "", nil
		}
		return strings.TrimSpace(fmt.Sprintf("%v", v)), nil
	}
}
