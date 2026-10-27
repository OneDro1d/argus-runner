package sutlogs

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/OneDro1d/argus-runner/internal/envcapture"
)

// API is the slice of the Kubernetes API the collector uses (envcapture.Client satisfies it).
type API interface {
	ListRunning(ctx context.Context, ns string) ([]envcapture.RunningContainer, error)
	FollowPodLog(ctx context.Context, ns, pod, container string, since time.Time) (io.ReadCloser, error)
}

// Sink receives every shipped entry (the Shipper).
type Sink interface {
	Add(labels map[string]string, ts time.Time, line string)
}

// Options configures Run.
type Options struct {
	API       API
	Namespace string // the SUT namespace to read
	// SelfPod / SelfNamespace: when the SUT namespace IS the instance namespace the collector must not
	// follow its own pod (its stderr is not shipped, and following it would be a loop in waiting).
	SelfPod, SelfNamespace string
	Rules                  Rules
	Sink                   Sink
	Warn                   *RateLog

	ListEvery  time.Duration // 0 = 5 s
	FirstSight time.Duration // 0 = 5 min: how far back a container seen for the first time is read
	// ReopenDelay is the wait before a follow stream that ended or failed is reopened (0 = 1 s); a
	// failure doubles it up to 5 s.
	ReopenDelay time.Duration
	Now         func() time.Time
}

// maxLineBytes bounds one line held in memory; a longer line is shipped cut here.
const maxLineBytes = 1 << 20

func (o *Options) defaults() {
	if o.ListEvery <= 0 {
		o.ListEvery = 5 * time.Second
	}
	if o.FirstSight <= 0 {
		o.FirstSight = 5 * time.Minute
	}
	if o.ReopenDelay <= 0 {
		o.ReopenDelay = time.Second
	}
	if o.Now == nil {
		o.Now = time.Now
	}
}

// Run lists the namespace's pods every ListEvery and keeps exactly one follow stream per (pod,
// container) that is listed. It returns only when ctx ends. It never exits on an API or Loki failure.
func Run(ctx context.Context, o Options) {
	o.defaults()
	c := &collector{o: o, streams: map[string]context.CancelFunc{}}
	defer c.wg.Wait()
	defer c.cancelAll()
	for {
		c.reconcile(ctx)
		t := time.NewTimer(o.ListEvery)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

type collector struct {
	o       Options
	streams map[string]context.CancelFunc
	wg      sync.WaitGroup
}

func (c *collector) cancelAll() {
	for k, cancel := range c.streams {
		cancel()
		delete(c.streams, k)
	}
}

func (c *collector) forbidden() {
	c.o.Warn.Warn("forbidden", fmt.Sprintf(
		"the Kubernetes API refused (403) to read pods or pod logs in namespace %q: that namespace's owner must grant it — render the Role with `argus render-k8s ... --sut-logs-via api --emit-sut-access-role` and apply sut-access-role.yaml in %q; retrying",
		c.o.Namespace, c.o.Namespace))
}

func (c *collector) reconcile(ctx context.Context) {
	list, err := c.o.API.ListRunning(ctx, c.o.Namespace)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		var se *envcapture.StatusError
		if errors.As(err, &se) && se.Forbidden() {
			c.forbidden()
		} else {
			c.o.Warn.Warn("list", "cannot list the pods of namespace "+fmt.Sprintf("%q", c.o.Namespace)+": "+err.Error()+"; retrying")
		}
		return // keep the streams that are open: a failed list says nothing about the pods
	}
	want := map[string]envcapture.RunningContainer{}
	for _, rc := range list {
		if rc.Pod == c.o.SelfPod && c.o.Namespace == c.o.SelfNamespace && c.o.SelfPod != "" {
			continue
		}
		want[rc.Pod+"/"+rc.Container] = rc
	}
	for k, cancel := range c.streams {
		if _, ok := want[k]; !ok {
			cancel()
			delete(c.streams, k)
		}
	}
	for k, rc := range want {
		if _, ok := c.streams[k]; ok {
			continue
		}
		sctx, cancel := context.WithCancel(ctx)
		c.streams[k] = cancel
		c.wg.Add(1)
		go func(rc envcapture.RunningContainer) {
			defer c.wg.Done()
			c.follow(sctx, rc)
		}(rc)
	}
}

func (c *collector) sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// follow keeps one container's stream open for as long as ctx lives (the container is still listed).
func (c *collector) follow(ctx context.Context, rc envcapture.RunningContainer) {
	since := c.o.Now().Add(-c.o.FirstSight)
	delay := c.o.ReopenDelay
	for ctx.Err() == nil {
		body, err := c.o.API.FollowPodLog(ctx, c.o.Namespace, rc.Pod, rc.Container, since)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			var se *envcapture.StatusError
			if errors.As(err, &se) && se.Forbidden() {
				c.forbidden()
			} else {
				c.o.Warn.Warn("stream", "cannot follow a pod log in namespace "+fmt.Sprintf("%q", c.o.Namespace)+": "+err.Error()+"; retrying")
			}
			c.sleep(ctx, delay)
			if delay *= 2; delay > maxBackoff {
				delay = maxBackoff
			}
			continue
		}
		delay = c.o.ReopenDelay
		if last, ok := c.read(ctx, body, rc.Container); ok {
			since = last // resume from the last timestamp shipped (Loki drops an exact duplicate)
		}
		body.Close()
		c.sleep(ctx, delay)
	}
}

// read ships every complete line of one stream. It returns the timestamp of the last line it shipped.
func (c *collector) read(ctx context.Context, body io.Reader, container string) (time.Time, bool) {
	br := bufio.NewReaderSize(body, 64<<10)
	var last time.Time
	var got bool
	for ctx.Err() == nil {
		raw, ok := readLine(br)
		if !ok {
			return last, got // EOF or a broken stream; a trailing partial line is not shipped
		}
		ts, line, parsed := splitTimestamp(raw)
		if !parsed {
			ts = c.o.Now()
		}
		// promtail drops an empty line, and so does this: it is not shipped, but a stream that resumes
		// must still start after it. A whitespace-only line is the SUT's and is shipped unchanged.
		if line != "" {
			c.o.Sink.Add(Labels(c.o.Rules, container, line), ts, line)
		}
		if parsed {
			last, got = ts, true
		}
	}
	return last, got
}

// readLine returns the next newline-terminated line without its "\n" (a "\r" before it is kept: the line
// is the SUT's, byte for byte). A line longer than maxLineBytes is returned cut. ok=false at EOF or on a
// read error; an unterminated tail is dropped.
func readLine(br *bufio.Reader) (string, bool) {
	var acc []byte
	for {
		frag, err := br.ReadSlice('\n')
		if len(acc) < maxLineBytes {
			acc = append(acc, frag...)
		}
		switch err {
		case nil:
			return strings.TrimSuffix(string(acc), "\n"), true
		case bufio.ErrBufferFull:
			continue
		default:
			return "", false
		}
	}
}

// splitTimestamp separates the RFC3339Nano prefix the API adds (timestamps=true) from the SUT's line. The
// line returned is everything after the ONE separating space, untouched. ok=false when there is no
// parsable timestamp: the whole text is then the line.
func splitTimestamp(raw string) (time.Time, string, bool) {
	i := strings.IndexByte(raw, ' ')
	head, rest := raw, ""
	if i >= 0 {
		head, rest = raw[:i], raw[i+1:]
	}
	ts, err := time.Parse(time.RFC3339Nano, head)
	if err != nil {
		return time.Time{}, raw, false
	}
	return ts, rest, true
}
