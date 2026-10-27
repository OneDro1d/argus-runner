package amqpengine

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// SUBSCRIBE: a long-lived consumer on a private queue of its own, used by the chain's `bus` step as
// evidence. Unlike Consume (one message, acknowledged and gone) it keeps counting for as long as the chain runs and
// remembers what it heard, so a step can be judged AFTER the traffic it is about.
//
// ⛔ Subscribe RETURNS ONLY AFTER consume-ok. The caller arms every subscription BEFORE the chain's first step fires, so
// a message published after Subscribe returned is routed to the queue: arming first is the happens-before guarantee.
//
// ⛔ A SUBSCRIPTION THAT IS LOST SAYS SO. A broker cancel, a queue deleted under it, a closed channel or connection and a
// closed deliveries channel all set Lost(); the step above reads it and is `error`, never "nothing was heard". The queue
// is exclusive and auto-delete (the subscription owns the connection's channel for the chain's lifetime, which the
// listen step's queue never could), and also carries x-expires and x-message-ttl, so the broker removes it even if the
// executor dies.

// CorrelationSource is ONE place a message may carry the run's correlation id. Exactly one field is set.
type CorrelationSource struct {
	Property string // an AMQP basic property: correlation_id | message_id | type | app_id | reply_to
	Header   string // a header name, matched case-insensitively
	Body     string // a dotted path into the decoded JSON body (numeric segments index a list)
}

func (c CorrelationSource) label() string {
	switch {
	case c.Property != "":
		return "property:" + c.Property
	case c.Header != "":
		return "header:" + c.Header
	default:
		return "body:" + c.Body
	}
}

// Envelope is what a broker target declares about the messages on it.
type Envelope struct {
	// Correlation lists where the run's id may be, in order; a message is correlated if ANY source matches.
	Correlation []CorrelationSource
	// ListPayload: the body is a JSON list and each object element is a record.
	ListPayload bool
}

// SubscribeOptions configure one subscription.
type SubscribeOptions struct {
	Envelope Envelope
	// CorrelationID is the run's id: a value equal to it, or starting with "<id>.", correlates a message.
	CorrelationID string
	// TTLMillis is x-expires and x-message-ttl of the queue.
	TTLMillis int
	// DecodedMax caps a gunzipped body (default 1 MiB); a body past it is undecodable.
	DecodedMax int
	// CorrelatedMax caps the correlated messages kept; SeenMax is the deliveries counted before Overflow is set.
	CorrelatedMax, SeenMax int
	// Now is the clock; nil = time.Now.
	Now func() time.Time
}

// Message is one correlated delivery, as the step's document shows it.
type Message struct {
	Exchange   string         `json:"exchange"`
	RoutingKey string         `json:"routing_key"`
	Properties map[string]any `json:"properties"`
	Headers    map[string]any `json:"headers,omitempty"`
	MatchedBy  string         `json:"matched_by"`
	Body       any            `json:"body"`
}

// Snapshot is the subscription's state at one moment.
type Snapshot struct {
	Seen, Uncorrelated, Undecodable int
	// Correlated is every message carrying the run's id; Count is those that also pass `where` (exact, never capped).
	Correlated, Count int
	// Messages are the first `keep` messages that pass `where`, in arrival order.
	Messages []Message
	// Overflow: more correlated messages (or deliveries) arrived than the subscription keeps, so the counts are a lower
	// bound and no claim may be judged on them.
	Overflow bool
}

// LostInfo says why and when a subscription stopped hearing.
type LostInfo struct {
	Reason string
	At     time.Time
}

type stored struct {
	msg        Message
	candidates []any // the decoded elements that carry the id (nil: none, or the body did not decode)
	whole      any   // the decoded body when no candidate applies
}

// Subscription is a live consumer. Safe for concurrent use.
type Subscription struct {
	e        *Engine
	ch       channel
	queue    string
	armedAt  time.Time
	opts     SubscribeOptions
	done     chan struct{}
	closeOne sync.Once

	mu       sync.Mutex
	closing  bool
	lost     *LostInfo
	seen     int
	uncorr   int
	undec    int
	msgs     []stored
	overflow bool
	bytes    int
}

// maxStoredBytes bounds what one subscription remembers.
const maxStoredBytes = 32 << 20

// Subscribe declares queue (exclusive, auto-delete, expiring), binds it to exchange under routingKey, sets the prefetch and
// starts consuming, on one channel of this connection. The Outcome is the broker's own refusal (with its code) when any
// call is refused.
func (e *Engine) Subscribe(queue, exchange, routingKey string, o SubscribeOptions) (*Subscription, Outcome) {
	if o.TTLMillis <= 0 {
		o.TTLMillis = 120000
	}
	if o.DecodedMax <= 0 {
		o.DecodedMax = 1 << 20
	}
	if o.CorrelatedMax <= 0 {
		o.CorrelatedMax = 2000
	}
	if o.SeenMax <= 0 {
		o.SeenMax = 20000
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	var sub *Subscription
	out := e.runOutcome(e.rpcTimeout, func() Outcome {
		ch, err := e.conn.Channel()
		if err != nil {
			return errOutcome(err)
		}
		// registered BEFORE any call, so a close that lands between two of them is seen
		chClosed := ch.NotifyClose(make(chan *amqp.Error, 1))
		chCancel := ch.NotifyCancel(make(chan string, 1))
		connClosed := e.conn.NotifyClose(make(chan *amqp.Error, 1))
		fail := func(err error) Outcome {
			e.closeChannel(ch)
			return errOutcome(err)
		}
		args := amqp.Table{"x-expires": int32(o.TTLMillis), "x-message-ttl": int32(o.TTLMillis)}
		if _, err := ch.QueueDeclare(queue, false, true, true, false, args); err != nil {
			return fail(err)
		}
		if err := ch.QueueBind(queue, routingKey, exchange, false, nil); err != nil {
			return fail(err)
		}
		if err := ch.Qos(256, 0, false); err != nil {
			return fail(err)
		}
		deliveries, err := ch.ConsumeWithContext(context.Background(), queue, queue, true, true, false, false, nil)
		if err != nil {
			return fail(err)
		}
		s := &Subscription{e: e, ch: ch, queue: queue, armedAt: o.Now(), opts: o, done: make(chan struct{})}
		go s.drain(deliveries)
		go s.watch(chClosed, chCancel, connClosed)
		sub = s
		return ok()
	})
	if !out.OK {
		return nil, out
	}
	return sub, out
}

// Queue is the name of the subscription's queue.
func (s *Subscription) Queue() string { return s.queue }

// ArmedAt is when consume-ok was received.
func (s *Subscription) ArmedAt() time.Time { return s.armedAt }

// Lost reports whether the subscription stopped hearing before it was closed, and why.
func (s *Subscription) Lost() (LostInfo, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lost == nil {
		return LostInfo{}, false
	}
	return *s.lost, true
}

func (s *Subscription) markLost(reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing || s.lost != nil {
		return
	}
	s.lost = &LostInfo{Reason: RedactURL(reason), At: s.opts.Now()}
}

func closeReason(what string, e *amqp.Error) string {
	if e == nil {
		return what + " closed"
	}
	return fmt.Sprintf("%s closed by the broker (%d: %s)", what, e.Code, e.Reason)
}

// watch records the first sign that the subscription is no longer hearing. It ends when the subscription is closed or lost.
func (s *Subscription) watch(chClosed <-chan *amqp.Error, chCancel <-chan string, connClosed <-chan *amqp.Error) {
	select {
	case <-s.done:
	case e := <-chClosed:
		s.markLost(closeReason("the channel", e))
	case <-chCancel:
		s.markLost("the broker cancelled the consumer (the queue was deleted or the node failed over)")
	case e := <-connClosed:
		s.markLost(closeReason("the connection", e))
	}
}

// drain is the collector: it reads every delivery until the channel closes.
func (s *Subscription) drain(deliveries <-chan amqp.Delivery) {
	for d := range deliveries {
		s.ingest(d)
	}
	s.markLost("the consumer's delivery channel closed")
}

// Close cancels the consumer, deletes the queue and closes the channel, bounded by closeTimeout; idempotent. It never
// reports an error: the verdict was decided before this ran. The connection is the caller's (it may be shared).
func (s *Subscription) Close() {
	s.closeOne.Do(func() {
		s.mu.Lock()
		s.closing = true
		s.mu.Unlock()
		close(s.done)
		_, done := boundedCall(s.e.closeTimeout, func() struct{} {
			_ = s.ch.Cancel(s.queue, false)
			_, _ = s.ch.QueueDelete(s.queue, false, false, false)
			return struct{}{}
		})
		if !done {
			s.e.teardown()
			return
		}
		s.e.closeChannel(s.ch)
	})
}

// ingest counts, decodes and correlates ONE delivery.
func (s *Subscription) ingest(d amqp.Delivery) {
	body := d.Body
	var decoded any
	decodedOK := false
	if strings.EqualFold(strings.TrimSpace(d.ContentEncoding), "gzip") {
		plain, err := gunzipCapped(body, s.opts.DecodedMax)
		if err != nil {
			body = nil
		} else {
			body = plain
		}
	}
	if body != nil {
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.UseNumber()
		if err := dec.Decode(&decoded); err == nil {
			if _, extra := dec.Token(); extra == io.EOF {
				decodedOK = true
			}
		}
	}

	var elems []any
	if decodedOK {
		if list, isList := decoded.([]any); isList && s.opts.Envelope.ListPayload {
			for _, el := range list {
				if _, obj := el.(map[string]any); obj {
					elems = append(elems, el)
				}
			}
		} else {
			elems = []any{decoded}
		}
	}

	matchedBy := ""
	var candidates []any
	for _, src := range s.opts.Envelope.Correlation {
		switch {
		case src.Body != "":
			for _, el := range elems {
				if v, found := jsonPath(el, src.Body); found {
					if str, isScalar := scalarString(v); isScalar && carriesID(str, s.opts.CorrelationID) {
						if matchedBy == "" {
							matchedBy = src.label()
						}
						candidates = append(candidates, el)
					}
				}
			}
		default:
			if v, found := messageLevel(d, src); found && carriesID(v, s.opts.CorrelationID) && matchedBy == "" {
				matchedBy = src.label()
			}
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen++
	if s.seen > s.opts.SeenMax {
		s.overflow = true
	}
	if !decodedOK {
		s.undec++
	}
	if matchedBy == "" {
		s.uncorr++
		return
	}
	if len(s.msgs) >= s.opts.CorrelatedMax || s.bytes+len(d.Body) > maxStoredBytes {
		s.overflow = true
		return
	}
	s.bytes += len(d.Body)
	if len(candidates) == 0 {
		// carried by a property or a header: every record of the body is a candidate for `where`
		candidates = elems
	}
	s.msgs = append(s.msgs, stored{
		msg:        Message{Exchange: d.Exchange, RoutingKey: d.RoutingKey, MatchedBy: matchedBy, Properties: propertiesOf(d), Headers: headersOf(d.Headers)},
		candidates: candidates, whole: decoded,
	})
}

// Snapshot builds the counts and the first `keep` messages that pass where (body path -> any-of values, exact). A message
// passes where when any record that carries the id has every path at one of its values; with no where every correlated
// message passes. Without a decodable record a message cannot pass a non-empty where.
func (s *Subscription) Snapshot(where map[string][]string, keep int) Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap := Snapshot{Seen: s.seen, Uncorrelated: s.uncorr, Undecodable: s.undec, Correlated: len(s.msgs), Overflow: s.overflow}
	for _, st := range s.msgs {
		body, pass := st.whole, len(where) == 0
		if len(st.candidates) > 0 {
			for _, c := range st.candidates {
				if whereHolds(c, where) {
					body, pass = c, true
					break
				}
			}
			if len(where) == 0 {
				body = st.candidates[0]
			}
		}
		if !pass {
			continue
		}
		snap.Count++
		if len(snap.Messages) < keep {
			m := st.msg
			m.Body = body
			snap.Messages = append(snap.Messages, m)
		}
	}
	return snap
}

func whereHolds(el any, where map[string][]string) bool {
	for path, want := range where {
		v, found := jsonPath(el, path)
		if !found {
			return false
		}
		got, isScalar := scalarString(v)
		if !isScalar {
			return false
		}
		hit := false
		for _, w := range want {
			if got == w {
				hit = true
				break
			}
		}
		if !hit {
			return false
		}
	}
	return true
}

// carriesID is the match rule: equal to the run's id, or starting with "<id>." (an MCP call's per-call id). Never a substring.
func carriesID(value, id string) bool {
	if id == "" || value == "" {
		return false
	}
	return value == id || strings.HasPrefix(value, id+".")
}

// jsonPath reads a dotted path out of a decoded JSON value; a numeric segment indexes a list.
func jsonPath(v any, path string) (any, bool) {
	for _, seg := range strings.Split(path, ".") {
		switch t := v.(type) {
		case map[string]any:
			next, ok := t[seg]
			if !ok {
				return nil, false
			}
			v = next
		case []any:
			i, err := strconv.Atoi(seg)
			if err != nil || i < 0 || i >= len(t) {
				return nil, false
			}
			v = t[i]
		default:
			return nil, false
		}
	}
	return v, true
}

// scalarString is the text of a JSON string, number or boolean.
func scalarString(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case json.Number:
		return t.String(), true
	case bool:
		return strconv.FormatBool(t), true
	}
	return "", false
}

func messageLevel(d amqp.Delivery, src CorrelationSource) (string, bool) {
	if src.Property != "" {
		switch strings.ToLower(src.Property) {
		case "correlation_id":
			return d.CorrelationId, d.CorrelationId != ""
		case "message_id":
			return d.MessageId, d.MessageId != ""
		case "type":
			return d.Type, d.Type != ""
		case "app_id":
			return d.AppId, d.AppId != ""
		case "reply_to":
			return d.ReplyTo, d.ReplyTo != ""
		}
		return "", false
	}
	for k, v := range d.Headers {
		if strings.EqualFold(k, src.Header) {
			switch t := v.(type) {
			case string:
				return t, t != ""
			case []byte:
				return string(t), len(t) > 0
			}
		}
	}
	return "", false
}

func propertiesOf(d amqp.Delivery) map[string]any {
	p := map[string]any{}
	set := func(k, v string) {
		if v != "" {
			p[k] = v
		}
	}
	set("correlation_id", d.CorrelationId)
	set("message_id", d.MessageId)
	set("content_type", d.ContentType)
	set("content_encoding", d.ContentEncoding)
	set("type", d.Type)
	set("app_id", d.AppId)
	return p
}

// headersOf keeps the scalar headers (strings, byte strings, numbers, booleans) in a form that marshals to JSON.
func headersOf(t amqp.Table) map[string]any {
	if len(t) == 0 {
		return nil
	}
	out := make(map[string]any, len(t))
	for k, v := range t {
		switch x := v.(type) {
		case string, bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
			out[k] = x
		case []byte:
			out[k] = string(x)
		}
	}
	return out
}

// gunzipCapped gunzips b, reading at most max bytes of output; past it is an error.
func gunzipCapped(b []byte, max int) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	out, err := io.ReadAll(io.LimitReader(zr, int64(max)+1))
	if err != nil {
		return nil, err
	}
	if len(out) > max {
		return nil, fmt.Errorf("decompresses to more than %d bytes", max)
	}
	return out, nil
}
