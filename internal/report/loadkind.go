package report

import (
	"sort"
	"strings"
)

// loadkind.go — AC-F2.2: what of a load test's errors the limited report and the results push carry. A load error is
// carried only as a word from a closed list and a count; a broker pause only as memory, disk or other plus its times.
// Never the load tool's message, never the broker's words: those can quote what the app sent. The full report keeps
// them (Reason: argus get-report, and author_get_report for a Full token, whose user accepts that the app's answers
// can leave), so diagnosis inside the environment is unchanged. LoadStepWithoutTexts and LoadErrorsByKind are the one projection both
// exits apply: the limited report (runner.limitedReport) and the results push's load_ramp (runner.loadRampWire, and
// the outbox that re-sends a queued push).

// The load error words, a closed list.
const (
	LoadErrorKindRefused     = "refused"
	LoadErrorKindTimeout     = "timeout"
	LoadErrorKindReset       = "reset"
	LoadErrorKindUnknownHost = "unknown_host"
	LoadErrorKindTLS         = "tls"
	LoadErrorKindProtocol    = "protocol"
	LoadErrorKindOther       = "other"
)

// LoadErrorKinds is the closed list, in this order.
var LoadErrorKinds = []string{LoadErrorKindRefused, LoadErrorKindTimeout, LoadErrorKindReset, LoadErrorKindUnknownHost,
	LoadErrorKindTLS, LoadErrorKindProtocol, LoadErrorKindOther}

// loadErrorKindOfType maps an error type's simple name (the part of the class name after the last '.') to its word.
// Checked against the load tool the executor image runs (Apache JMeter 5.6.3, HttpClient 4.5.14, HTTPHC4Impl): the
// JTL's responseCode is "Non HTTP response code: " + the OUTER exception's class name. The simple name also matches the
// same classes in httpclient5 (org.apache.hc.core5.http) and java.net.ProtocolException.
//   - ClientProtocolException: an invalid header line or status line reaches the JTL as this class (message "null").
//   - ConnectionPoolTimeoutException: a ConnectTimeoutException (the wait for a pooled connection).
var loadErrorKindOfType = map[string]string{
	"ConnectException":               LoadErrorKindRefused,
	"HttpHostConnectException":       LoadErrorKindRefused,
	"SocketTimeoutException":         LoadErrorKindTimeout,
	"ConnectTimeoutException":        LoadErrorKindTimeout,
	"ConnectionPoolTimeoutException": LoadErrorKindTimeout,
	"UnknownHostException":           LoadErrorKindUnknownHost,
	"NoHttpResponseException":        LoadErrorKindProtocol,
	"MalformedChunkCodingException":  LoadErrorKindProtocol,
	"TruncatedChunkException":        LoadErrorKindProtocol,
	"ProtocolException":              LoadErrorKindProtocol,
	"ClientProtocolException":        LoadErrorKindProtocol,
	"ParseException":                 LoadErrorKindProtocol,
	"ConnectionClosedException":      LoadErrorKindProtocol,
}

// LoadErrorKind is the word of one transport failure, chosen from its error TYPE (the class in the JTL responseCode).
// The message is read only for java.net.SocketException, which the JDK raises for a reset ("Connection reset", "...
// by peer"), a broken pipe, and for other events; only the word is kept. Every class in javax.net.ssl is tls (SSLException
// and its subclasses). An unknown or empty type is other.
func LoadErrorKind(s LoadSample) string {
	class := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s.Code), nonHTTPCodePrefix))
	if strings.HasPrefix(class, "javax.net.ssl.") {
		return LoadErrorKindTLS
	}
	name := class[strings.LastIndex(class, ".")+1:]
	if name == "SocketException" {
		msg := strings.ToLower(s.Message)
		if strings.Contains(msg, "connection reset") || strings.Contains(msg, "broken pipe") {
			return LoadErrorKindReset
		}
		return LoadErrorKindOther
	}
	if k, ok := loadErrorKindOfType[name]; ok {
		return k
	}
	return LoadErrorKindOther
}

// loadErrorKindOfReason classifies an entry that has no Kind (a report written before AC-F2.2) from its own reason,
// "<class>: <message>" as loadErrorReason builds it, by the same rule.
func loadErrorKindOfReason(reason string) string {
	class, msg := reason, ""
	if i := strings.Index(reason, ": "); i >= 0 {
		class, msg = reason[:i], reason[i+2:]
	}
	return LoadErrorKind(LoadSample{Code: class, Message: msg})
}

func isLoadErrorKind(k string) bool {
	for _, w := range LoadErrorKinds {
		if k == w {
			return true
		}
	}
	return false
}

// The broker pause words, a closed list.
const (
	BlockedKindMemory = "memory"
	BlockedKindDisk   = "disk"
	BlockedKindOther  = "other"
)

// BlockedKinds is the closed list, in this order.
var BlockedKinds = []string{BlockedKindMemory, BlockedKindDisk, BlockedKindOther}

// BlockedKind is the word of a broker pause reason. RabbitMQ (3.13.7, measured) sends "low on " + "memory" / "disk",
// joined by " & " when both alarms are on, in no defined order; both give memory (a fixed precedence, so the word does
// not depend on that order). Anything else, and the sampler's "no reason given", is other.
func BlockedKind(reason string) string {
	r := strings.ToLower(reason)
	switch {
	case strings.Contains(r, "memory"):
		return BlockedKindMemory
	case strings.Contains(r, "disk"), strings.Contains(r, "disc"):
		return BlockedKindDisk
	}
	return BlockedKindOther
}

func isBlockedKind(k string) bool {
	for _, w := range BlockedKinds {
		if k == w {
			return true
		}
	}
	return false
}

// LoadErrorsByKind is what leaves of a list of load errors: one {Kind, Count} entry per word, never a Reason, most
// frequent first (ties by word). The counts add up to the input's. An entry without a Kind is classified from its
// reason; a Kind that is not on the closed list is counted as other — the exit, not the writer, keeps the list closed.
// nil for no errors. The input is not changed.
func LoadErrorsByKind(errs []LoadError) []LoadError {
	if len(errs) == 0 {
		return nil
	}
	counts := map[string]int{}
	for _, e := range errs {
		k := e.Kind
		switch {
		case k == "":
			k = loadErrorKindOfReason(e.Reason)
		case !isLoadErrorKind(k):
			k = LoadErrorKindOther
		}
		counts[k] += e.Count
	}
	out := make([]LoadError, 0, len(counts))
	for k, n := range counts {
		out = append(out, LoadError{Kind: k, Count: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Kind < out[j].Kind
	})
	return out
}

// LoadStepWithoutTexts is the step as the limited report and the results push carry it: error_reasons as LoadErrorsByKind, and each blocked
// period with its times and word but without the broker's words (a period without a Kind gets BlockedKind of its
// reason; a Kind off the list becomes other). Every other field is the step's own value. The two projected slices
// are new, so nothing written to them reaches st.
func LoadStepWithoutTexts(st LoadStep) LoadStep {
	out := st
	out.ErrorReasons = LoadErrorsByKind(st.ErrorReasons)
	out.Blocked = nil
	if st.Blocked != nil {
		out.Blocked = make([]BlockedPeriod, len(st.Blocked))
		for i, b := range st.Blocked {
			k := b.Kind
			switch {
			case k == "":
				k = BlockedKind(b.Reason)
			case !isBlockedKind(k):
				k = BlockedKindOther
			}
			out.Blocked[i] = BlockedPeriod{SinceMs: b.SinceMs, UntilMs: b.UntilMs, Kind: k}
		}
	}
	return out
}
