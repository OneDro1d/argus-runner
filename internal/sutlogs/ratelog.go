package sutlogs

import (
	"encoding/json"
	"io"
	"sync"
	"time"
)

// RateLog writes the collector's OWN stderr lines: JSON, one per event, and at most one per key per
// minute. It is handed only text the collector composed (a namespace name, a status code, a count) —
// never a log line of the SUT, a token or a credential, which no caller of it holds in a message.
type RateLog struct {
	Out   io.Writer
	Every time.Duration // 0 = one minute
	Now   func() time.Time

	mu   sync.Mutex
	last map[string]time.Time
}

func (r *RateLog) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// Allow reports whether key may speak now, and if so starts its quiet period.
func (r *RateLog) Allow(key string) bool {
	every := r.Every
	if every <= 0 {
		every = time.Minute
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.last == nil {
		r.last = map[string]time.Time{}
	}
	now := r.now()
	if t, ok := r.last[key]; ok && now.Sub(t) < every {
		return false
	}
	r.last[key] = now
	return true
}

// Warn writes msg unless key spoke within the last minute.
func (r *RateLog) Warn(key, msg string) {
	if r == nil || !r.Allow(key) {
		return
	}
	r.Emit("warn", msg)
}

// Emit writes one JSON line, always.
func (r *RateLog) Emit(level, msg string) {
	if r == nil || r.Out == nil {
		return
	}
	b, err := json.Marshal(map[string]string{
		"ts": r.now().UTC().Format(time.RFC3339), "level": level, "component": "collect-logs", "msg": msg,
	})
	if err != nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_, _ = r.Out.Write(append(b, '\n'))
}
