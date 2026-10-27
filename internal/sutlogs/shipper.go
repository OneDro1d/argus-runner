package sutlogs

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/OneDro1d/argus-runner/internal/obsquery"
)

// Defaults of the batch pusher (the brief's numbers).
const (
	DefaultFlushEvery = 100 * time.Millisecond
	DefaultFlushBytes = 64 << 10
	DefaultBufferMax  = 8 << 20
	maxBackoff        = 5 * time.Second
	maxPushBytes      = 1 << 20 // one push request is cut here, so a backlog is sent in several
	pushTimeout       = 10 * time.Second
	// rejectedAfter: a push Loki answers 400/413/422 to is retried this many times, then dropped and
	// counted. A 400 is "this body will never be accepted" (an entry too old, a line too long); retrying
	// it for ever would block every line behind it.
	rejectedAfter = 3
)

// entryOverhead is what an entry costs in the buffer beyond its line (timestamp, label references).
const entryOverhead = 64

type rec struct {
	seq    uint64
	key    string
	labels map[string]string
	ts     time.Time
	line   string
	size   int
}

// Shipper buffers log entries and pushes them to Loki: a flush every FlushEvery or as soon as FlushBytes
// are pending; a failed push is retried with a backoff capped at 5 s; the buffer holds at most BufferMax
// bytes and, when full, drops the OLDEST entries and counts them. Add never blocks and never returns an
// error. It never writes a log line anywhere but to Loki.
type Shipper struct {
	Loki       *obsquery.Loki
	Warn       *RateLog
	FlushEvery time.Duration
	FlushBytes int
	BufferMax  int
	Sleep      func(context.Context, time.Duration) // tests replace it; nil = real time

	mu       sync.Mutex
	buf      []rec
	bytes    int
	seq      uint64
	dropped  int // dropped since the last report, buffer full
	rejected int // dropped since the last report, Loki refused for good
	kick     chan struct{}
}

func (s *Shipper) init() {
	if s.FlushEvery <= 0 {
		s.FlushEvery = DefaultFlushEvery
	}
	if s.FlushBytes <= 0 {
		s.FlushBytes = DefaultFlushBytes
	}
	if s.BufferMax <= 0 {
		s.BufferMax = DefaultBufferMax
	}
	if s.kick == nil {
		s.kick = make(chan struct{}, 1)
	}
}

// Add queues one entry. labels must not be changed afterwards.
func (s *Shipper) Add(labels map[string]string, ts time.Time, line string) {
	s.mu.Lock()
	s.init()
	s.seq++
	r := rec{seq: s.seq, key: labelKey(labels), labels: labels, ts: ts, line: line, size: len(line) + entryOverhead}
	s.buf = append(s.buf, r)
	s.bytes += r.size
	for s.bytes > s.BufferMax && len(s.buf) > 0 {
		s.bytes -= s.buf[0].size
		s.buf = s.buf[1:]
		s.dropped++
	}
	full := s.bytes >= s.FlushBytes
	s.mu.Unlock()
	if full {
		select {
		case s.kick <- struct{}{}:
		default:
		}
	}
}

// Pending reports the bytes waiting (tests).
func (s *Shipper) Pending() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bytes
}

func labelKey(l map[string]string) string {
	ks := make([]string, 0, len(l))
	for k := range l {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	var b strings.Builder
	for _, k := range ks {
		b.WriteString(k)
		b.WriteByte(0)
		b.WriteString(l[k])
		b.WriteByte(1)
	}
	return b.String()
}

// Run flushes until ctx is done, then makes ONE last attempt to send what is left.
func (s *Shipper) Run(ctx context.Context) {
	s.mu.Lock()
	s.init()
	s.mu.Unlock()
	tick := time.NewTicker(s.FlushEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			fctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			s.FlushOnce(fctx, false)
			cancel()
			return
		case <-tick.C:
		case <-s.kick:
		}
		s.ReportDrops()
		s.FlushOnce(ctx, true)
	}
}

// FlushOnce sends everything pending, in pushes of at most maxPushBytes. With retry it keeps trying the
// batch in hand until it is accepted, refused for good, or ctx ends; without, it tries once.
func (s *Shipper) FlushOnce(ctx context.Context, retry bool) {
	for ctx.Err() == nil {
		batch, maxSeq := s.take()
		if len(batch) == 0 {
			return
		}
		if !s.pushWithRetry(ctx, batch, retry) {
			return
		}
		s.release(maxSeq)
	}
}

// take copies the front of the buffer (up to maxPushBytes) WITHOUT removing it: an entry leaves the
// buffer only when its push is accepted (or refused for good), so a Loki outage loses nothing the
// buffer can still hold.
func (s *Shipper) take() ([]rec, uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n, size int
	for n < len(s.buf) && (n == 0 || size+s.buf[n].size <= maxPushBytes) {
		size += s.buf[n].size
		n++
	}
	if n == 0 {
		return nil, 0
	}
	batch := append([]rec(nil), s.buf[:n]...)
	return batch, batch[n-1].seq
}

// release removes every entry with seq <= maxSeq that is still at the front (the drop-oldest rule may
// already have taken some of them).
func (s *Shipper) release(maxSeq uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for len(s.buf) > 0 && s.buf[0].seq <= maxSeq {
		s.bytes -= s.buf[0].size
		s.buf = s.buf[1:]
	}
}

func (s *Shipper) pushWithRetry(ctx context.Context, batch []rec, retry bool) bool {
	streams := group(batch)
	backoff := 100 * time.Millisecond
	refused := 0
	for {
		err := obsquery.PushLogStreams(ctx, s.Loki, streams, pushTimeout)
		if err == nil {
			return true
		}
		var pse *obsquery.PushStatusError
		if errors.As(err, &pse) && (pse.Status == 400 || pse.Status == 413 || pse.Status == 422) {
			refused++
			if refused >= rejectedAfter {
				s.mu.Lock()
				s.rejected += len(batch)
				s.mu.Unlock()
				return true // dropped: release them so the lines behind them can go
			}
		}
		s.warn("loki-push", "cannot push to Loki: "+describePushErr(err)+" (retrying; the buffer is bounded)")
		if !retry || ctx.Err() != nil {
			return false
		}
		s.ReportDrops()
		s.sleep(ctx, backoff)
		if backoff *= 2; backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// describePushErr is the push error with nothing in it that is a credential or a log line: the status,
// or the transport reason without the URL.
func describePushErr(err error) string {
	var pse *obsquery.PushStatusError
	if errors.As(err, &pse) {
		return fmt.Sprintf("status %d", pse.Status)
	}
	// A transport error names the host it dialled and can echo a URL: say only that Loki was unreachable.
	return "cannot reach Loki"
}

func (s *Shipper) sleep(ctx context.Context, d time.Duration) {
	if s.Sleep != nil {
		s.Sleep(ctx, d)
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

func (s *Shipper) warn(key, msg string) {
	if s.Warn != nil {
		s.Warn.Warn(key, msg)
	}
}

// ReportDrops says, at most once a minute, how many entries the full buffer dropped. Run it from the
// collector's loop; it is separate from Add so Add never writes.
func (s *Shipper) ReportDrops() {
	if s.Warn == nil {
		return
	}
	s.mu.Lock()
	d, r := s.dropped, s.rejected
	s.mu.Unlock()
	if d > 0 && s.Warn.Allow("buffer-full") {
		s.Warn.Emit("warn", fmt.Sprintf("the %d MiB log buffer was full (Loki is slow or down): dropped the %d oldest entries since the last report", s.BufferMax>>20, d))
		s.mu.Lock()
		s.dropped -= d
		s.mu.Unlock()
	}
	if r > 0 && s.Warn.Allow("loki-refused-count") {
		s.Warn.Emit("warn", fmt.Sprintf("Loki refused %d log entries for good since the last report; they were dropped", r))
		s.mu.Lock()
		s.rejected -= r
		s.mu.Unlock()
	}
}

// Dropped returns the totals not yet reported (tests).
func (s *Shipper) Dropped() (bufferFull, refused int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dropped, s.rejected
}

func group(batch []rec) []obsquery.LogStream {
	idx := map[string]int{}
	var out []obsquery.LogStream
	for _, r := range batch {
		i, ok := idx[r.key]
		if !ok {
			i = len(out)
			idx[r.key] = i
			out = append(out, obsquery.LogStream{Labels: r.labels})
		}
		out[i].Entries = append(out[i].Entries, obsquery.LogEntry{Time: r.ts, Line: r.line})
	}
	return out
}
