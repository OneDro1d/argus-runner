package toolcore

// Reachability — "can this environment actually reach the SUT?", answered at BOOTUP rather than
// discovered by the first run (INT-019 / UC045).
//
// UC045's step 1 asks validate_config for three confirmations: the config parses, the scenarios are
// coverable, and the SUT is REACHABLE FROM THE RUNNER'S VANTAGE. Only the first two were implemented.
// Live against orderservice-compose the tool returned valid:true with no reachability field of any kind,
// and `valid:true` is exactly what an operator reads as "green, the environment is wired". The first run
// then discovered the truth, one layer later and much more noisily.
//
// WHAT A PROBE HERE CAN AND CANNOT PROVE. This dials TCP. That is deliberate: it is protocol-agnostic
// (AMQP, Postgres and HTTP answer the same question), and it has NO SIDE EFFECTS on the SUT, whereas an
// HTTP GET against an arbitrary base_url could hit a real endpoint. The cost is that an open port does
// not prove the right service is behind it, and `reachable` says so in its own words rather than being
// read as health. Vantage is the point: the dial happens from the RUNNER's network namespace, which is
// the only vantage that matters and the reason a `localhost` base_url fails here.
//
// THE RULE THIS FILE IS BUILT AROUND: a probe that could not run is never a pass. auth declares a type
// and a bearer token and NO ADDRESS, so it is permanently unprobeable — it is reported `not_checked`
// WITH A REASON rather than omitted, because omission reads as fine. AllReachable is false whenever
// anything went unchecked, for the same reason: "nothing failed" is not "everything passed".

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/OneDro1d/argus-runner/internal/config"
)

// Probe verdicts. FOUR states, and the last two are not the same thing — that distinction is what
// makes the surface honest.
const (
	ReachOK      = "reachable"   // a TCP connection was established (NOT proof of a healthy service)
	ReachDown    = "unreachable" // the dial was attempted and failed
	ReachUnknown = "not_checked" // an address was expected, and no probe was possible
	// ReachNotApplicable — there is no address to dial and there never could be, because of the SHAPE
	// of the target (VR7-J1 / V24-001). `auth` is {type, bearer_token}; `external` in its LIST form has
	// a verify_url per entry and no single address. Credentials and per-entry hooks are exercised by
	// the scenarios, not by a dial.
	//
	// 🚨 THIS IS DELIBERATELY NARROWER THAN "has no address". An EMPTY jdbc_url is a config gap — an
	// address was expected and is missing — and stays ReachUnknown so it still poisons the verdict.
	// Collapsing the two would let a SUT that forgets its database URL report REACHABLE while its
	// database was never dialled, which is the same absence-rendered-as-health this requirement exists
	// to remove, arriving through its own fix.
	ReachNotApplicable = "not_applicable"
)

// probeDisableEnv switches the dial off. Everything then reports not_checked — an operator who disables
// the probe gets silence about reachability, never a green they did not earn.
const probeDisableEnv = "ARGUS_VALIDATE_NO_PROBE"

// TargetReach is one configured target's outcome. Address is the host:port ACTUALLY dialled, with any
// userinfo stripped: broker and database URLs carry credentials and this payload is read, logged and
// pasted by humans.
type TargetReach struct {
	Target  string `json:"target"`
	Status  string `json:"status"`
	Detail  string `json:"detail"`
	Address string `json:"address,omitempty"`
}

// Reachability is the whole report. Summary exists so the answer survives being skim-read.
type Reachability struct {
	AllReachable bool          `json:"all_reachable"`
	Summary      string        `json:"summary"`
	Targets      []TargetReach `json:"targets"`
}

// probeOrder is fixed so the report is stable between calls and diffable.
var probeOrder = []string{"http", "mcp", "database", "message_broker", "external", "auth"}

// ProbeTargets dials every configured target concurrently and reports each outcome. timeout bounds each
// individual dial, so the whole call is bounded by one timeout rather than by their sum.
func ProbeTargets(c *config.Config, timeout time.Duration) Reachability {
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	disabled := strings.TrimSpace(getenv(probeDisableEnv)) != ""

	var mu sync.Mutex
	var wg sync.WaitGroup
	items := probeItems(c)
	out := make([]TargetReach, 0, len(items))

	for _, it := range items {
		switch {
		case it.why != "":
			// VR7-J1: WHY there is no address decides whether this poisons the verdict. A shape that
			// cannot carry an address is not evidence of anything; a missing address is a gap.
			status := ReachUnknown
			if unprobeableByShape(it.label) {
				status = ReachNotApplicable
			}
			mu.Lock()
			out = append(out, TargetReach{Target: it.label, Status: status, Detail: it.why})
			mu.Unlock()
		case disabled:
			mu.Lock()
			out = append(out, TargetReach{Target: it.label, Status: ReachUnknown, Address: it.addr,
				Detail: "probing is switched off by " + probeDisableEnv + "; reachability is UNKNOWN, not fine"})
			mu.Unlock()
		default:
			wg.Add(1)
			go func(name, addr string) {
				defer wg.Done()
				r := dialTCP(name, addr, timeout)
				mu.Lock()
				out = append(out, r)
				mu.Unlock()
			}(it.label, it.addr)
		}
	}
	wg.Wait()

	rank := map[string]int{}
	for i, it := range items {
		rank[it.label] = i
	}
	sort.SliceStable(out, func(i, j int) bool { return rank[out[i].Target] < rank[out[j].Target] })
	return summarize(out)
}

// probeItem is one thing to dial: its report label, and EITHER an address OR the reason there is none.
type probeItem struct {
	label string
	addr  string
	why   string
}

// probeItems lists every configured target in report order: the plain slots (probeOrder), then —
// VR10-S3-11 — every NAMED entry, labelled `<kind>_targets.<name>` and dialled exactly like its
// plain slot, so `graph` being wrong is discovered here, before a scenario goes red for it.
func probeItems(c *config.Config) []probeItem {
	var items []probeItem
	for _, name := range probeOrder {
		if !c.TargetPresentExported(name) {
			continue // not configured is not a finding — this SUT simply does not use that target
		}
		addr, why := dialAddrFor(c, name)
		items = append(items, probeItem{label: name, addr: addr, why: why})
	}
	for _, n := range c.NamedTargets(config.KindHTTP) {
		_, host, port := c.Targets.HTTPTargets[n].HostPort()
		items = append(items, probeItem{label: "http_targets." + n, addr: net.JoinHostPort(host, port)})
	}
	for _, n := range c.NamedTargets(config.KindMCP) {
		addr, why := addrFromURL(c.Targets.MCPTargets[n].URL(), "targets.mcp_targets."+n+".base_url")
		items = append(items, probeItem{label: "mcp_targets." + n, addr: addr, why: why})
	}
	for _, n := range c.NamedTargets(config.KindDatabase) {
		raw := ""
		if t := c.Targets.DatabaseTargets[n]; t != nil {
			raw = t.JDBCURL
		}
		addr, why := jdbcAddr(raw, "targets.database_targets."+n+".jdbc_url")
		items = append(items, probeItem{label: "database_targets." + n, addr: addr, why: why})
	}
	for _, n := range c.NamedTargets(config.KindMessageBroker) {
		addr, why := brokerAddr(c.Targets.MessageBrokerTargets[n], "targets.message_broker_targets."+n)
		items = append(items, probeItem{label: "message_broker_targets." + n, addr: addr, why: why})
	}
	return items
}

// TargetCount is how many targets ProbeTargets would report on for c: the same enumeration, so a
// caller asking "does this config declare anything to probe?" cannot disagree with the probe.
func TargetCount(c *config.Config) int { return len(probeItems(c)) }

// jdbcAddr: a JDBC URL is `jdbc:<driver>://host:port/db`. Trimming the jdbc: prefix leaves a
// parseable URL.
func jdbcAddr(raw, field string) (addr, why string) {
	return addrFromURL(strings.TrimPrefix(strings.TrimSpace(raw), "jdbc:"), field)
}

// brokerAddr dials the broker's url, else its management_url; a broker that declares neither has
// no address to dial, and says so.
func brokerAddr(t *config.MQTarget, at string) (addr, why string) {
	raw := ""
	if t != nil {
		raw = t.URL
		if strings.TrimSpace(raw) == "" {
			raw = t.ManagementURL
		}
	}
	if strings.TrimSpace(raw) == "" {
		return "", at + " declares queues/exchanges but no url or management_url, so there is no address to dial"
	}
	return addrFromURL(raw, at+".url")
}

func dialTCP(name, addr string, timeout time.Duration) TargetReach {
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		// The dial error carries the real cause (refused / no such host / i/o timeout), which is what
		// tells an operator whether to fix DNS, a port, or a stopped container.
		return TargetReach{Target: name, Status: ReachDown, Address: addr,
			Detail: fmt.Sprintf("dial tcp %s from the runner's vantage: %v", addr, rootCause(err))}
	}
	_ = conn.Close()
	return TargetReach{Target: name, Status: ReachOK, Address: addr,
		Detail: "TCP connect succeeded from the runner's vantage (proves the address is reachable, not that the service behind it is healthy)"}
}

// unprobeableByShape answers whether a target CANNOT carry a dialable address at all, as opposed to
// having one that is missing or malformed (VR7-J1 / V24-001).
//
// Only `auth` qualifies today, and dialAddrFor says why in its own words: "Permanently unprobeable BY
// SHAPE: {type, bearer_token} contains no address." Credentials are exercised by the scenarios.
//
// ⚠ DELIBERATELY NARROWER THAN THE SA'S LIST, and the reason is worth keeping. The SA also named
// `external` in LIST form and `message_broker` with queues but no URL. Neither can be admitted safely
// as the code stands:
//
//   - `external` returns ONE reason string covering two different situations — "is in the LIST form
//     (per-entry verify_url) OR declares no webhook_base_url". The first is a shape; the second is a
//     gap. They are indistinguishable to the caller, so admitting the pair would admit the gap.
//   - `message_broker` "declares queues/exchanges but no url or management_url" IS a missing address:
//     the broker is real, the runner must reach it, and nobody said where it is.
//
// Splitting the `external` reason is a genuine follow-up. Until then this stays at the one case the
// source itself certifies, because the cost of being wrong is asymmetric: too narrow yields an
// unnecessary grey, too wide yields a green nobody earned.
func unprobeableByShape(name string) bool { return name == "auth" }

// dialAddrFor returns the host:port to dial, or a REASON why this target cannot be dialled. Exactly one
// of the two is non-empty; a target that yields neither would vanish from the report, which is the
// failure mode this whole file exists to prevent.
func dialAddrFor(c *config.Config, name string) (addr, why string) {
	switch name {
	case "http":
		proto, host, port := c.HTTPHostPort()
		_ = proto
		return net.JoinHostPort(host, port), ""
	case "mcp":
		return addrFromURL(c.MCPBaseURL(), "targets.mcp.base_url")
	case "database":
		raw := ""
		if c.Targets.Database != nil {
			raw = c.Targets.Database.JDBCURL
		}
		return jdbcAddr(raw, "targets.database.jdbc_url")
	case "message_broker":
		return brokerAddr(c.Targets.MessageBroker, "targets.message_broker")
	case "external":
		if u := strings.TrimSpace(c.ExternalWebhookBaseURL()); u != "" {
			return addrFromURL(u, "targets.external.webhook_base_url")
		}
		// The LIST shape (`external: [{name, verify_url}]`) is a real second form in the wild; it is
		// named here rather than silently skipped.
		return "", "targets.external is in the LIST form (per-entry verify_url) or declares no webhook_base_url; no single address to dial"
	case "auth":
		// Permanently unprobeable BY SHAPE: {type, bearer_token} contains no address. Saying so is the
		// point — an operator must be able to tell "checked and fine" from "never checkable".
		return "", "targets.auth declares a type and a token, not an address — there is nothing to dial (credentials are exercised by the scenarios, not here)"
	}
	return "", "no probe is implemented for this target"
}

// addrFromURL extracts host:port, filling in the scheme's default port when the URL omits one. It never
// returns userinfo: broker and database URLs carry credentials.
func addrFromURL(raw, field string) (addr, why string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", field + " is empty, so there is no address to dial"
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		// Deliberately does NOT echo raw — an unparseable value is exactly the case where it might be a
		// paste of something sensitive.
		return "", fmt.Sprintf("%s could not be parsed as a URL, so no host could be extracted", field)
	}
	port := u.Port()
	if port == "" {
		if port = defaultPort(u.Scheme); port == "" {
			return "", fmt.Sprintf("%s has no port and scheme %q has no default this build knows", field, u.Scheme)
		}
	}
	return net.JoinHostPort(u.Hostname(), port), ""
}

func defaultPort(scheme string) string {
	switch strings.ToLower(scheme) {
	case "http", "ws":
		return "80"
	case "https", "wss":
		return "443"
	case "amqp":
		return "5672"
	case "amqps":
		return "5671"
	case "postgresql", "postgres":
		return "5432"
	case "mysql", "mariadb":
		return "3306"
	case "mongodb":
		return "27017"
	case "redis":
		return "6379"
	}
	return ""
}

// summarize builds the one line a skim-reader will actually see, and the AllReachable flag.
func summarize(t []TargetReach) Reachability {
	var ok, down, unknown, na int
	for _, x := range t {
		switch x.Status {
		case ReachOK:
			ok++
		case ReachDown:
			down++
		case ReachNotApplicable:
			na++
			// VR7-J1: not counted anywhere. A target with no address BY SHAPE is not evidence for or
			// against reachability, so it must not sit in `unknown` and drag AllReachable down — the
			// tri-state and AllReachable are pinned to agree, and they have to agree about this too.
			continue
		default:
			unknown++
		}
	}
	r := Reachability{Targets: t}
	// AllReachable requires at least one target actually probed AND nothing down AND nothing unchecked.
	// An empty target list is not "all reachable" — there is no evidence either way.
	r.AllReachable = ok > 0 && down == 0 && unknown == 0

	switch {
	case len(t) == 0:
		r.Summary = "no dialable targets are configured, so reachability is UNKNOWN"
	default:
		parts := []string{fmt.Sprintf("%d reachable", ok)}
		if down > 0 {
			parts = append(parts, fmt.Sprintf("%d UNREACHABLE", down))
		}
		if unknown > 0 {
			parts = append(parts, fmt.Sprintf("%d not checked", unknown))
		}
		// VR7-J1: EXCLUDED FROM THE VERDICT, NEVER FROM THE REPORT. This file's own rule is that a
		// target which could not be dialled is "reported WITH A REASON rather than omitted, because
		// omission reads as fine". Not counting it toward `unknown` must not become not mentioning it.
		if na > 0 {
			parts = append(parts, fmt.Sprintf("%d not applicable", na))
		}
		r.Summary = strings.Join(parts, " · ")
		if down == 0 && unknown > 0 {
			r.Summary += " — nothing failed, but that is not the same as everything passing"
		}
	}
	return r
}

// reachabilityWarnings surfaces the same facts in `warnings`, because an operator who reads valid:true
// and skips the nested block is the exact reader INT-019 was about.
func reachabilityWarnings(r Reachability) []string {
	var w []string
	for _, x := range r.Targets {
		switch x.Status {
		case ReachDown:
			w = append(w, fmt.Sprintf("target %s is UNREACHABLE from the runner's vantage (%s): %s — the config is fine; the SUT is not answering", x.Target, x.Address, x.Detail))
		case ReachUnknown:
			w = append(w, fmt.Sprintf("target %s was NOT CHECKED: %s", x.Target, x.Detail))
		}
	}
	return w
}

// rootCause unwraps net/OpError noise down to the sentence an operator can act on ("connection
// refused" rather than "dial tcp 127.0.0.1:5432: connect: connection refused" nested twice).
func rootCause(err error) error {
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Err != nil {
		return opErr.Err
	}
	return err
}

// getenv is a seam so the disable switch is testable via t.Setenv.
func getenv(k string) string { return os.Getenv(k) }

// loadConfigForProbe is the probe-side config load, kept beside the probe so a test can build a
// Reachability from a file without going through the whole tool.
func loadConfigForProbe(path string) (*config.Config, error) { return config.Load(path) }

// SUTReachableTriState collapses a Reachability report into the single tri-state the control plane
// carries on the poll (VR6-W1, V23-011). nil is NOT MEASURED, and it is a state rather than a missing
// value: the Environments page renders it grey and says so.
//
// A boolean was rejected because `false` would have to carry both "I dialled it and it refused" and "I
// never dialled it", and the page would then paint a red verdict on a SUT nobody measured. That is the
// same class of defect as the one this feature exists to fix, only inverted — V18-008 showed a green
// nobody earned; a two-state version would show a red nobody earned.
//
// Precedence, and the reason for each step:
//
//	any UNREACHABLE      -> false   a measured failure is the loudest true fact on the report, and must
//	                                not be downgraded to grey by an unrelated gap in coverage
//	else any not_checked -> nil     partial evidence is not evidence. summarize() already says this in
//	                                prose — "nothing failed, but that is not the same as everything
//	                                passing" — and the tri-state must not contradict its own summary
//	else at least one OK -> true
//	else (no targets)    -> nil     an empty report is not a pass
//
// true is returned in exactly the cases where AllReachable is true; a test pins that, so the page and
// the summary line can never come to differ.
func SUTReachableTriState(r Reachability) *bool {
	var ok, down, unknown int
	for _, x := range r.Targets {
		switch x.Status {
		case ReachOK:
			ok++
		case ReachDown:
			down++
		case ReachNotApplicable:
			// VR7-J1 (V24-001). dialAddrFor already knew this target could never be dialled and said so
			// on purpose — "an operator must be able to tell 'checked and fine' from 'never checkable'".
			// Counting it as unknown is what silenced BOTH OrderService instances while Social and
			// Memstore reported fine on three tiers between them.
			continue
		default:
			unknown++
		}
	}
	switch {
	case down > 0:
		f := false
		return &f
	case unknown > 0, ok == 0:
		return nil
	default:
		t := true
		return &t
	}
}
