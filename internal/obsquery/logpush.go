package obsquery

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"time"
)

// LogEntry is one log line with the time Loki files it under.
type LogEntry struct {
	Time time.Time
	Line string
}

// LogStream is one Loki stream: a label set and its entries.
type LogStream struct {
	Labels  map[string]string
	Entries []LogEntry
}

// PushLogStreams ships streams to the Loki l names in ONE push request (the generic sibling of
// PushRequestEvents: same target rules via postLokiPush — PushURL verbatim, Basic Auth from
// l.Credential, X-Scope-OrgID from l.Tenant). Within each stream the entries are sent in
// non-decreasing time (a stable sort; Loki refuses or reorders otherwise). A stream with no entries is
// not sent. A nil l, or nothing to send, is a no-op. A non-2xx answer is a *PushStatusError.
//
// The line is sent exactly as given: nothing is trimmed, stripped or escaped beyond JSON encoding.
func PushLogStreams(ctx context.Context, l *Loki, streams []LogStream, timeout time.Duration) error {
	if l == nil || (l.BaseURL == "" && l.PushURL == "") {
		return nil
	}
	var out []lokiPushStream
	for _, s := range streams {
		if len(s.Entries) == 0 {
			continue
		}
		es := append([]LogEntry(nil), s.Entries...)
		sort.SliceStable(es, func(i, j int) bool { return es[i].Time.Before(es[j].Time) })
		vals := make([][2]string, 0, len(es))
		for _, e := range es {
			vals = append(vals, [2]string{strconv.FormatInt(e.Time.UnixNano(), 10), e.Line})
		}
		out = append(out, lokiPushStream{Stream: s.Labels, Values: vals})
	}
	if len(out) == 0 {
		return nil
	}
	body, err := json.Marshal(lokiPushBody{Streams: out})
	if err != nil {
		return err
	}
	return postLokiPush(ctx, l, body, timeout)
}
