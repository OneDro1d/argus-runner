package obsquery

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/report"
)

// Spec 26 P1 (A1, observe only): the sandbox read and the pure builder.
//
// ⚠ FIXTURES ARE DOC EXAMPLES, NOT CAPTURES. OpenShell's own docs call their example lines
// illustrative (`"version": "0.3.0"` at ocsf-json-export.mdx:89 matches no release). Every fixture
// below names the doc file and line it comes from (OpenShell v0.1.2,
// https://raw.githubusercontent.com/NVIDIA/OpenShell/v0.1.2/docs/<path>) and says what was ADDED to
// it. Spec 26 G1 replaces them with captured lines once it runs.

const sb = "sb-1"

// t0 is the doc's own clock: 1775014138811 is the allowed example's `time`
// (observability/ocsf-json-export.mdx:83).
var t0 = time.UnixMilli(1775014130000)

func ms(d time.Duration) int64 { return t0.Add(d).UnixMilli() }

// fxAllowedNet: observability/ocsf-json-export.mdx:71-115 (an allowed CONNECT), verbatim except that
// `time` is a parameter and `container` (ocsf-json-export.mdx:67: "Use container.uid to associate
// the event with its sandbox") is added.
func fxAllowedNet(at int64, uid string) string {
	return fmt.Sprintf(`{"class_uid":4001,"class_name":"Network Activity","category_uid":4,"category_name":"Network Activity","activity_id":1,"activity_name":"Open","severity_id":1,"severity":"Informational","status_id":1,"status":"Success","time":%d,"message":"CONNECT allowed api.github.com:443","metadata":{"product":{"name":"OpenShell Sandbox Supervisor","vendor_name":"NVIDIA","version":"0.3.0"},"version":"1.8.0"},"action_id":1,"action":"Allowed","disposition_id":1,"disposition":"Allowed","dst_endpoint":{"domain":"api.github.com","port":443},"src_endpoint":{"ip":"10.42.0.31","port":37494},"actor":{"process":{"name":"/usr/bin/curl","pid":57}},"firewall_rule":{"name":"github_api","type":"opa"},"container":{"uid":%q,"name":"agent"}}`, at, uid)
}

// fxDeniedNet: observability/ocsf-json-export.mdx:120-150 (a denied CONNECT), verbatim except that
// `time`, `metadata` (copied from :85-92) and `container` are added — the doc's denied example has
// none of the three.
func fxDeniedNet(at int64, uid string) string {
	return fmt.Sprintf(`{"class_uid":4001,"class_name":"Network Activity","activity_id":1,"activity_name":"Open","severity_id":3,"severity":"Medium","status_id":2,"status":"Failure","action_id":2,"action":"Denied","disposition_id":2,"disposition":"Blocked","status_detail":"no matching policy","message":"CONNECT denied httpbin.org:443","time":%d,"metadata":{"version":"1.8.0"},"dst_endpoint":{"domain":"httpbin.org","port":443},"actor":{"process":{"name":"/usr/bin/curl","pid":63}},"firewall_rule":{"name":"-","type":"opa"},"container":{"uid":%q}}`, at, uid)
}

// fxDeniedHTTP: NO JSON EXAMPLE EXISTS in the v0.1.2 docs for HTTP Activity. The content is the
// shorthand line at observability/logging.mdx:117 (`HTTP:POST [MED] DENIED POST
// http://api.github.com/user/repos [policy:github_api engine:opa]`), the reason "l7 deny" from
// logging.mdx:202, and the shape (http_request.http_method, http_request.url as an OCSF URL object)
// is the OCSF v1.8.0 schema's. TLS-terminated traffic is logged as http://host:443 (C9).
func fxDeniedHTTP(at int64, uid string) string {
	return fmt.Sprintf(`{"class_uid":4002,"activity_name":"Post","severity_id":3,"action_id":2,"action":"Denied","disposition":"Blocked","status_detail":"l7 deny","time":%d,"metadata":{"version":"1.8.0"},"http_request":{"http_method":"POST","url":{"url_string":"http://api.github.com:443/user/repos","scheme":"http","hostname":"api.github.com","port":443,"path":"/user/repos"}},"actor":{"process":{"name":"/usr/bin/curl"}},"firewall_rule":{"name":"github_api"},"container":{"uid":%q}}`, at, uid)
}

// fxFinding: NO JSON EXAMPLE EXISTS for Detection Finding either. Content from the shorthand at
// observability/logging.mdx:142 (`FINDING:CREATE [MED] "configured content matched"`); shape
// (finding_info.title) from the OCSF v1.8.0 schema. A finding has no action.
func fxFinding(at int64, uid string) string {
	return fmt.Sprintf(`{"class_uid":2004,"activity_name":"Create","severity_id":3,"time":%d,"metadata":{"version":"1.8.0"},"finding_info":{"title":"configured content matched","types":["content_guard.match"]},"container":{"uid":%q}}`, at, uid)
}

// fxDNSDenied: fxDeniedNet with the reason from observability/logging.mdx:199 ("DNS resolution failed
// for <host>:<port>"). C8: a DNS failure IS a Denied network event. P1 keeps it (it observes); P2
// decides whether it counts toward the verdict.
func fxDNSDenied(at int64, uid string) string {
	return strings.Replace(fxDeniedNet(at, uid), `"status_detail":"no matching policy"`, `"status_detail":"DNS resolution failed for nope.invalid:443"`, 1)
}

// fxDowngraded: fxDeniedNet as ocsf_schema_version 1.3 writes it — `container` stripped,
// metadata.version rewritten, unmapped.downgraded_from set (ocsf-json-export.mdx:203-207, :224-231).
// It still carries the sandbox id (in a reason) so a `|= "<id>"` filter could match it.
func fxDowngraded(at int64) string {
	return fmt.Sprintf(`{"class_uid":4001,"activity_name":"Open","action_id":2,"action":"Denied","disposition":"Blocked","status_detail":"no matching policy for sb-1","time":%d,"metadata":{"version":"1.3.0"},"unmapped":{"downgraded_from":"1.8.0"},"dst_endpoint":{"domain":"httpbin.org","port":443}}`, at)
}

// fxShorthand: observability/logging.mdx:38, the human shorthand line — not OCSF JSON.
const fxShorthand = `2026-04-01T04:04:32.690Z OCSF NET:OPEN [MED] DENIED /usr/bin/curl(64) -> httpbin.org:443 [policy:- engine:opa] [reason:no matching policy]`

func line(atMs int64, body string) SandboxLine {
	return SandboxLine{TS: strconv.FormatInt(atMs*int64(time.Millisecond), 10), Line: body}
}

func readOf(lines ...SandboxLine) SandboxRead {
	return SandboxRead{Lines: lines, Limit: SandboxLineLimit}
}

var winA = ScenarioWindow{ID: "A", From: t0, To: t0.Add(20 * time.Second)}

// count is a block's denied_count, or -1 when it is null (an unavailable block).
func count(p *report.SandboxPolicy) int {
	if p.DeniedCount == nil {
		return -1
	}
	return *p.DeniedCount
}

func policyJSON(t *testing.T, p *report.SandboxPolicy) string {
	t.Helper()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// ── the read ────────────────────────────────────────────────────────────────

type fakeLoki struct {
	mu       sync.Mutex
	requests int
	query    map[string]string
}

func newFakeLoki(t *testing.T, body string) (*fakeLoki, *httptest.Server) {
	t.Helper()
	f := &fakeLoki{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests++
		f.query = map[string]string{}
		for k, v := range r.URL.Query() {
			f.query[k] = v[0]
		}
		f.mu.Unlock()
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

func TestSandboxLines_QueryShape(t *testing.T) {
	f, srv := newFakeLoki(t, `{"status":"success","data":{"result":[{"stream":{"job":"openshell-gateway"},"values":[["1775014140000000000","x"]]}]}}`)
	from, to := t0, t0.Add(25*time.Second)
	read := (&Loki{BaseURL: srv.URL}).SandboxLines(`{job="openshell-gateway",source="sandbox-jsonl"}`, sb, from, to)
	if read.Err != nil || len(read.Lines) != 1 || read.Limit != 2000 {
		t.Fatalf("read = %+v", read)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	want := map[string]string{
		"query":     `{job="openshell-gateway",source="sandbox-jsonl"} |= "sb-1"`,
		"start":     strconv.FormatInt(from.UnixNano(), 10),
		"end":       strconv.FormatInt(to.UnixNano(), 10),
		"limit":     "2000",
		"direction": "forward",
	}
	for k, v := range want {
		if f.query[k] != v {
			t.Errorf("query param %s = %q, want %q", k, f.query[k], v)
		}
	}
}

func TestSandboxLines_RefusesUnsafeSandboxWithoutQuerying(t *testing.T) {
	f, srv := newFakeLoki(t, `{"status":"success","data":{"result":[]}}`)
	for _, bad := range []string{`a"} or {x="`, `sb 1`, `-sb`, `sb|="x`, strings.Repeat("a", 129), ""} {
		read := (&Loki{BaseURL: srv.URL}).SandboxLines(`{job="x"}`, bad, t0, t0.Add(time.Second))
		if read.Err == nil {
			t.Errorf("sandbox %q must be refused", bad)
		} else if bad != "" && strings.Contains(read.Err.Error(), bad) {
			t.Errorf("the refusal must not echo the refused value: %v", read.Err)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.requests != 0 {
		t.Fatalf("an unsafe sandbox id must never reach Loki: %d requests", f.requests)
	}
}

// The read refuses an unsafe id without echoing it; the block must not carry it either. A refused
// id is written as "" (the reason already says the id was refused).
func TestSandboxPolicy_RefusedSandboxIDNotWrittenToBlock(t *testing.T) {
	for _, bad := range []string{`a"} or {x="`, "sb 1", "-sb", strings.Repeat("a", 129)} {
		read := (&Loki{BaseURL: "http://127.0.0.1:1"}).SandboxLines(`{job="x"}`, bad, t0, t0.Add(time.Second))
		p := SandboxPolicyFor(read, bad, winA, nil)
		if p.Sandbox != "" || strings.Contains(policyJSON(t, p), bad) {
			t.Errorf("refused id %q reached the block: %s", bad, policyJSON(t, p))
		}
		if p.Coverage != report.CoverageUnavailable || !strings.Contains(p.CoverageReason, "not a safe identifier") {
			t.Errorf("refused id %q: want unavailable naming the refusal, got %s", bad, policyJSON(t, p))
		}
	}
	// A safe id is written as it is.
	if p := SandboxPolicyFor(readOf(), sb, winA, nil); p.Sandbox != sb {
		t.Errorf("safe id: block sandbox = %q, want %q", p.Sandbox, sb)
	}
}

func TestSandboxLines_NoBaseURLIsAnError(t *testing.T) {
	read := (&Loki{}).SandboxLines(`{job="x"}`, sb, t0, t0.Add(time.Second))
	if read.Err == nil || !strings.Contains(read.Err.Error(), "--loki is empty") {
		t.Fatalf("no --loki must be a named error, got %+v", read)
	}
}

func TestSandboxLines_HTTPErrorIsCarried(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	read := (&Loki{BaseURL: srv.URL}).SandboxLines(`{job="x"}`, sb, t0, t0.Add(time.Second))
	if read.Err == nil || !strings.Contains(read.Err.Error(), "HTTP 502") {
		t.Fatalf("a 502 must reach the read as its reason, got %+v", read)
	}
}

// ── the builder ─────────────────────────────────────────────────────────────

func TestSandboxPolicy_ReadErrorIsUnavailableNeverClean(t *testing.T) {
	p := SandboxPolicyFor(SandboxRead{Err: errors.New("Loki answered HTTP 401"), Limit: 2000}, sb, winA, nil)
	if p.Coverage != report.CoverageUnavailable || !strings.Contains(p.CoverageReason, "Loki answered HTTP 401") || count(p) != -1 {
		t.Fatalf("block = %+v", *p)
	}
	if !strings.Contains(policyJSON(t, p), `"events":[]`) {
		t.Fatalf("events must be [] never null: %s", policyJSON(t, p))
	}
}

// The liveness rule (, finding A6): Loki answers a dead selector with HTTP 200 and nothing.
func TestSandboxPolicy_NoLineIsUnavailable(t *testing.T) {
	p := SandboxPolicyFor(readOf(), sb, winA, nil)
	if p.Coverage != report.CoverageUnavailable || !strings.Contains(p.CoverageReason, "no line from sandbox sb-1 between") {
		t.Fatalf("an empty window must be unavailable (liveness), got %+v", *p)
	}
}

func TestSandboxPolicy_OnlyAllowedLinesIsCompleteAndEmpty(t *testing.T) {
	p := SandboxPolicyFor(readOf(
		line(ms(1*time.Second), fxAllowedNet(ms(1*time.Second), sb)),
		line(ms(2*time.Second), fxAllowedNet(ms(2*time.Second), sb)),
		line(ms(3*time.Second), fxAllowedNet(ms(3*time.Second), sb)),
	), sb, winA, nil)
	if p.Coverage != report.CoverageComplete || count(p) != 0 || len(p.Events) != 0 {
		t.Fatalf("three allowed lines = complete with no events, got %+v", *p)
	}
	if !strings.Contains(p.CoverageReason, "read 3 lines of sandbox sb-1") {
		t.Errorf("reason = %q", p.CoverageReason)
	}
}

func TestSandboxPolicy_KeepsDeniedBlockedAndFindings(t *testing.T) {
	p := SandboxPolicyFor(readOf(
		line(ms(1*time.Second), fxAllowedNet(ms(1*time.Second), sb)),
		line(ms(2*time.Second), fxDeniedNet(ms(2*time.Second), sb)),
		line(ms(3*time.Second), fxDeniedHTTP(ms(3*time.Second), sb)),
		line(ms(4*time.Second), fxFinding(ms(4*time.Second), sb)),
		line(ms(5*time.Second), fxDNSDenied(ms(5*time.Second), sb)),
	), sb, winA, nil)
	if p.Coverage != report.CoverageComplete || count(p) != 4 || len(p.Events) != 4 {
		t.Fatalf("want 4 kept (denied net, denied http, finding, DNS denial), got %+v", *p)
	}
	want := []report.SandboxPolicyEvent{
		{Time: "2026-04-01T03:28:52.000Z", Class: "NET:OPEN", Action: "Denied", Target: "httpbin.org:443", Process: "/usr/bin/curl", Rule: "-", Reason: "no matching policy"},
		{Time: "2026-04-01T03:28:53.000Z", Class: "HTTP:POST", Action: "Denied", Target: "POST http://api.github.com:443/user/repos", Process: "/usr/bin/curl", Rule: "github_api", Reason: "l7 deny"},
		{Time: "2026-04-01T03:28:54.000Z", Class: "FINDING:CREATE", Action: "", Target: "configured content matched", Process: "", Rule: "", Reason: ""},
		{Time: "2026-04-01T03:28:55.000Z", Class: "NET:OPEN", Action: "Denied", Target: "httpbin.org:443", Process: "/usr/bin/curl", Rule: "-", Reason: "DNS resolution failed for nope.invalid:443"},
	}
	for i := range want {
		if p.Events[i] != want[i] {
			t.Errorf("event %d =\n %+v\nwant\n %+v", i, p.Events[i], want[i])
		}
	}
}

// C7: a class-0 relay/mediation event (ocsf-json-export.mdx:180-183) is kept when it carries a denial.
func TestSandboxPolicy_KeepsClassZeroDenial(t *testing.T) {
	ev := fmt.Sprintf(`{"class_uid":0,"action_id":2,"status_detail":"relay refused","time":%d,"container":{"uid":"sb-1"}}`, ms(time.Second))
	p := SandboxPolicyFor(readOf(line(ms(time.Second), ev)), sb, winA, nil)
	if count(p) != 1 || p.Events[0].Class != "EVENT" {
		t.Fatalf("a class-0 denial must be kept as EVENT, got %+v", *p)
	}
}

func TestSandboxPolicy_OtherSandboxExcluded(t *testing.T) {
	p := SandboxPolicyFor(readOf(
		line(ms(1*time.Second), fxAllowedNet(ms(1*time.Second), sb)),
		line(ms(2*time.Second), fxDeniedNet(ms(2*time.Second), "sb-10")), // matched |= "sb-1" by substring
		line(ms(3*time.Second), strings.Replace(fxDeniedNet(ms(3*time.Second), sb), `,"container":{"uid":"sb-1"}`, "", 1)),
	), sb, winA, nil)
	if count(p) != 0 || p.Coverage != report.CoverageComplete {
		t.Fatalf("another sandbox's denial and a record with no container must not count, got %+v", *p)
	}
}

// The event's own time wins over Loki's entry time (A5): an entry ingested inside the window for an
// event that happened before it is not this scenario's, and the reverse is.
func TestSandboxPolicy_AttributesByEventTime(t *testing.T) {
	early := ms(-5 * time.Second) // before winA.From
	p := SandboxPolicyFor(readOf(
		line(ms(1*time.Second), fxAllowedNet(ms(1*time.Second), sb)),
		line(ms(2*time.Second), fxDeniedNet(early, sb)),                   // Loki time inside, event time outside
		line(ms(30*time.Second), fxDeniedHTTP(ms(19*time.Second), sb)),    // Loki time outside, event time inside
		line(ms(21*time.Second), fxDeniedNet(ms(21*time.Second), "sb-1")), // the query's tail past To: cut
	), sb, winA, nil)
	if count(p) != 1 || p.Events[0].Class != "HTTP:POST" {
		t.Fatalf("want only the HTTP denial (event time inside), got %+v", *p)
	}
}

func TestSandboxPolicy_LokiTimeWhenEventTimeAbsent(t *testing.T) {
	noTime := strings.Replace(fxDeniedNet(0, sb), `"time":0,`, "", 1)
	p := SandboxPolicyFor(readOf(line(ms(4*time.Second), noTime)), sb, winA, nil)
	if count(p) != 1 || p.Events[0].Time != "2026-04-01T03:28:54.000Z" {
		t.Fatalf("with no OCSF time the Loki entry time places the event, got %+v", *p)
	}
}

func TestSandboxPolicy_LimitIsLossy(t *testing.T) {
	r := SandboxRead{Limit: 3, Lines: []SandboxLine{
		line(ms(1*time.Second), fxAllowedNet(ms(1*time.Second), sb)),
		line(ms(2*time.Second), fxDeniedNet(ms(2*time.Second), sb)),
		line(ms(3*time.Second), fxAllowedNet(ms(3*time.Second), sb)),
	}}
	p := SandboxPolicyFor(r, sb, winA, nil)
	if p.Coverage != report.CoverageLossy || !strings.Contains(p.CoverageReason, "limit of 3 lines") || count(p) != 1 {
		t.Fatalf("a read that hit its limit is lossy (events still listed), got %+v", *p)
	}
}

func TestSandboxPolicy_UnparsedBesideOCSFIsLossy(t *testing.T) {
	p := SandboxPolicyFor(readOf(
		line(ms(1*time.Second), fxAllowedNet(ms(1*time.Second), sb)),
		line(ms(2*time.Second), `{"class_uid":4001,"time":"not-a-number","container":{"uid":"sb-1"}}`),
		line(ms(3*time.Second), `{"truncated":`),
	), sb, winA, nil)
	if p.Coverage != report.CoverageLossy || !strings.Contains(p.CoverageReason, "2 lines could not be parsed") {
		t.Fatalf("unparsable lines beside OCSF ones = lossy, got %+v", *p)
	}
}

func TestSandboxPolicy_DowngradedSchemaIsUnavailable(t *testing.T) {
	p := SandboxPolicyFor(readOf(
		line(ms(1*time.Second), fxAllowedNet(ms(1*time.Second), sb)),
		line(ms(2*time.Second), fxDowngraded(ms(2*time.Second))),
	), sb, winA, nil)
	if p.Coverage != report.CoverageUnavailable || !strings.Contains(p.CoverageReason, "downgraded OCSF schema (metadata.version 1.3.0)") {
		t.Fatalf("a downgraded line makes the window unavailable, got %+v", *p)
	}
	// metadata.version alone, without the marker, is enough too — and 1.10 is not 1.1.
	v13 := strings.Replace(fxDeniedNet(ms(2*time.Second), sb), `"version":"1.8.0"`, `"version":"1.3"`, 1)
	if p := SandboxPolicyFor(readOf(line(ms(2*time.Second), v13)), sb, winA, nil); p.Coverage != report.CoverageUnavailable {
		t.Errorf("metadata.version 1.3 = downgraded, got %+v", *p)
	}
	v110 := strings.Replace(fxDeniedNet(ms(2*time.Second), sb), `"version":"1.8.0"`, `"version":"1.10.0"`, 1)
	if p := SandboxPolicyFor(readOf(line(ms(2*time.Second), v110)), sb, winA, nil); p.Coverage != report.CoverageComplete {
		t.Errorf("metadata.version 1.10.0 is not a 1.1 downgrade, got %+v", *p)
	}
}

func TestSandboxPolicy_ShorthandOnlyIsUnavailable(t *testing.T) {
	p := SandboxPolicyFor(readOf(line(ms(2*time.Second), fxShorthand+" sb-1"), line(ms(3*time.Second), fxShorthand+" sb-1")), sb, winA, nil)
	if p.Coverage != report.CoverageUnavailable || !strings.Contains(p.CoverageReason, "2 lines matched the sandbox id but are not OCSF JSON") {
		t.Fatalf("shorthand-only = unavailable, got %+v", *p)
	}
}

// Invariant 5: no payload and no credential value ever reaches the block. A canary sits in every
// place a payload or a credential can hide in an OCSF line.
func TestSandboxPolicy_NoPayloadOrCredentialReachesReport(t *testing.T) {
	canaryLine := fmt.Sprintf(`{"class_uid":4002,"activity_name":"Post","action_id":2,"action":"Denied","time":%d,"metadata":{"version":"1.8.0"},`+
		`"message":"POST body CANARY-MSG",`+
		`"status_detail":"denied https://bob:CANARY-SDPASS@api.example.test/x?key=CANARY-SDQ [see?CANARY-SDQ2]",`+
		`"http_request":{"http_method":"POST","url":{"url_string":"http://alice:CANARY-PASS@api.github.com:443/repos/x?token=CANARY-QS#CANARY-FRAG","query_string":"token=CANARY-QS2"},`+
		`"body":"CANARY-BODY","http_headers":[{"name":"Authorization","value":"Bearer CANARY-HDR"}]},`+
		`"unmapped":{"secret":"CANARY-UNMAPPED"},"evidences":[{"data":"CANARY-EVID"}],`+
		`"actor":{"process":{"name":"/usr/bin/curl","cmd_line":"curl -H CANARY-CMD"}},"container":{"uid":"sb-1"}}`, ms(time.Second))
	stringURL := strings.Replace(fxDeniedHTTP(ms(2*time.Second), sb), `{"url_string":"http://api.github.com:443/user/repos","scheme":"http","hostname":"api.github.com","port":443,"path":"/user/repos"}`,
		`"https://carol:CANARY-PASS3@api.github.com/user?access_token=CANARY-QS3"`, 1)
	p := SandboxPolicyFor(readOf(line(ms(time.Second), canaryLine), line(ms(2*time.Second), stringURL)), sb, winA, nil)
	if count(p) != 2 {
		t.Fatalf("both canary lines are denials and must be kept, got %+v", *p)
	}
	out := policyJSON(t, p)
	if strings.Contains(out, "CANARY") {
		t.Fatalf("a payload or credential reached the report block: %s", out)
	}
	if p.Events[0].Target != "POST http://api.github.com:443/repos/x" || p.Events[1].Target != "POST https://api.github.com/user" {
		t.Errorf("targets = %q, %q", p.Events[0].Target, p.Events[1].Target)
	}
	if p.Events[0].Reason != "denied https://api.example.test/x [see]" {
		t.Errorf("reason = %q", p.Events[0].Reason)
	}
}

func TestSandboxPolicy_HTTPTargetIsMethodHostPortPath(t *testing.T) {
	ev := strings.Replace(fxDeniedHTTP(ms(time.Second), sb), `"url_string":"http://api.github.com:443/user/repos"`, `"url_string":"http://api.github.com:443/repos/x?token=c"`, 1)
	p := SandboxPolicyFor(readOf(line(ms(time.Second), ev)), sb, winA, nil)
	if got := p.Events[0].Target; got != "POST http://api.github.com:443/repos/x" {
		t.Fatalf("target = %q, want POST http://api.github.com:443/repos/x", got)
	}
	// The URL object without url_string is assembled from its parts.
	parts := strings.Replace(fxDeniedHTTP(ms(time.Second), sb), `"url_string":"http://api.github.com:443/user/repos",`, "", 1)
	if got := SandboxPolicyFor(readOf(line(ms(time.Second), parts)), sb, winA, nil).Events[0].Target; got != "POST http://api.github.com:443/user/repos" {
		t.Fatalf("target from parts = %q", got)
	}
}

// Invariant 6: the same Loki answer gives the same bytes, whatever order Loki returned it in.
func TestSandboxPolicy_DeterministicBytes(t *testing.T) {
	var lines []SandboxLine
	for i := 0; i < 12; i++ {
		at := ms(time.Duration(1+i%4) * time.Second) // ties on time, broken by the other fields
		switch i % 3 {
		case 0:
			lines = append(lines, line(at, fxDeniedNet(at, sb)))
		case 1:
			lines = append(lines, line(at, fxDeniedHTTP(at, sb)))
		default:
			lines = append(lines, line(at, fxFinding(at, sb)))
		}
	}
	first := policyJSON(t, SandboxPolicyFor(readOf(lines...), sb, winA, nil))
	rng := rand.New(rand.NewSource(26))
	for round := 0; round < 20; round++ {
		shuffled := append([]SandboxLine(nil), lines...)
		rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		if got := policyJSON(t, SandboxPolicyFor(readOf(shuffled...), sb, winA, nil)); got != first {
			t.Fatalf("round %d: different bytes for the same lines:\n%s\n%s", round, got, first)
		}
	}
}

// (finding A5): sequential scenarios touch only at their padding, so `shared_with` marks only
// an event that actually sits inside another scenario's padded window.
func TestSandboxPolicy_SharedOnlyInOverlap(t *testing.T) {
	a := ScenarioWindow{ID: "A", From: t0, To: t0.Add(10 * time.Second)}
	b := ScenarioWindow{ID: "B", From: t0.Add(8 * time.Second), To: t0.Add(20 * time.Second)}
	alive := line(ms(1*time.Second), fxAllowedNet(ms(1*time.Second), sb))
	in := SandboxPolicyFor(readOf(alive, line(ms(9*time.Second), fxDeniedNet(ms(9*time.Second), sb))), sb, a, []ScenarioWindow{a, b})
	if len(in.SharedWith) != 1 || in.SharedWith[0] != "B" {
		t.Fatalf("an event at 9s sits in both windows: shared_with = %v", in.SharedWith)
	}
	out := SandboxPolicyFor(readOf(alive, line(ms(5*time.Second), fxDeniedNet(ms(5*time.Second), sb))), sb, a, []ScenarioWindow{a, b})
	if len(out.SharedWith) != 0 || strings.Contains(policyJSON(t, out), "shared_with") {
		t.Fatalf("an event at 5s is A's alone: %+v", *out)
	}
}

func TestSandboxPolicy_CapsEventsKeepsCount(t *testing.T) {
	lines := []SandboxLine{}
	for i := 0; i < 60; i++ {
		at := ms(time.Duration(i) * 100 * time.Millisecond)
		lines = append(lines, line(at, fxDeniedNet(at, sb)))
	}
	p := SandboxPolicyFor(readOf(lines...), sb, winA, nil)
	if len(p.Events) != 50 || p.EventsOmitted != 10 || count(p) != 60 {
		t.Fatalf("60 denials = 50 listed, 10 omitted, count 60; got %d / %d / %d", len(p.Events), p.EventsOmitted, count(p))
	}
	if p.Events[0].Time != "2026-04-01T03:28:50.000Z" {
		t.Errorf("the earliest events are the ones listed, got first %s", p.Events[0].Time)
	}
}

func TestSandboxPolicy_LongFieldsAreClipped(t *testing.T) {
	long := strings.Repeat("é", 300)
	ev := strings.Replace(fxDeniedNet(ms(time.Second), sb), `"status_detail":"no matching policy"`, `"status_detail":"`+long+`"`, 1)
	p := SandboxPolicyFor(readOf(line(ms(time.Second), ev)), sb, winA, nil)
	if n := len([]rune(p.Events[0].Reason)); n != 256 {
		t.Fatalf("reason has %d runes, want 256", n)
	}
}

// Spec 26 §4 and §7: `unavailable` must never read as "no denials". A count beside it would: a 0 reads
// as a measured zero, and a count from the lines that did parse (a window where only some lines are
// downgraded) reads as the window's total. So an `unavailable` block carries `"denied_count":null`
// and `"events":[]`, whatever lines the read returned, and nothing else derived from them.
func TestSandboxPolicy_UnavailableCarriesNoCount(t *testing.T) {
	reads := map[string]struct {
		read    SandboxRead
		sandbox string
	}{
		"read error":   {SandboxRead{Err: errors.New("Loki answered HTTP 401"), Limit: 2000}, sb},
		"empty window": {readOf(), sb},
		"mixed downgrade": {readOf(
			line(ms(1*time.Second), fxDeniedNet(ms(1*time.Second), sb)),
			line(ms(2*time.Second), fxDowngraded(ms(2*time.Second))),
		), sb},
		"shorthand only": {readOf(line(ms(time.Second), fxShorthand+" sb-1")), sb},
		"no sandbox id":  {readOf(line(ms(time.Second), fxDeniedNet(ms(time.Second), sb))), ""},
	}
	b := ScenarioWindow{ID: "B", From: t0, To: t0.Add(30 * time.Second)} // holds every line above
	for name, c := range reads {
		t.Run(name, func(t *testing.T) {
			p := SandboxPolicyFor(c.read, c.sandbox, winA, []ScenarioWindow{winA, b})
			out := policyJSON(t, p)
			if p.Coverage != report.CoverageUnavailable {
				t.Fatalf("fixture: want unavailable, got %s", out)
			}
			for _, want := range []string{`"denied_count":null`, `"events":[]`} {
				if !strings.Contains(out, want) {
					t.Errorf("an unavailable block must carry %s: %s", want, out)
				}
			}
			for _, never := range []string{`"denied_count":0`, `"denied_count":1`, "events_omitted", "shared_with"} {
				if strings.Contains(out, never) {
					t.Errorf("an unavailable block must not carry %s: %s", never, out)
				}
			}
		})
	}
}

// The other side of the same rule: `complete` and `lossy` are measurements, so their count is a
// number even when it is 0.
func TestSandboxPolicy_MeasuredBlocksCarryANumber(t *testing.T) {
	allowed := readOf(line(ms(time.Second), fxAllowedNet(ms(time.Second), sb)))
	if out := policyJSON(t, SandboxPolicyFor(allowed, sb, winA, nil)); !strings.Contains(out, `"coverage":"complete"`) || !strings.Contains(out, `"denied_count":0`) {
		t.Errorf("a complete window with nothing denied carries a measured 0: %s", out)
	}
	lossy := SandboxRead{Limit: 2, Lines: []SandboxLine{
		line(ms(1*time.Second), fxAllowedNet(ms(1*time.Second), sb)),
		line(ms(2*time.Second), fxDeniedNet(ms(2*time.Second), sb)),
	}}
	if out := policyJSON(t, SandboxPolicyFor(lossy, sb, winA, nil)); !strings.Contains(out, `"coverage":"lossy"`) || !strings.Contains(out, `"denied_count":1`) {
		t.Errorf("a lossy window carries the count it read: %s", out)
	}
}

// Invariant 3 over every outcome above: coverage is one of three values, its reason is never empty,
// and events is never null.
func TestSandboxPolicy_CoverageReasonNeverEmpty(t *testing.T) {
	reads := map[string]SandboxRead{
		"error":      {Err: errors.New("x"), Limit: 2000},
		"empty":      readOf(),
		"allowed":    readOf(line(ms(time.Second), fxAllowedNet(ms(time.Second), sb))),
		"denied":     readOf(line(ms(time.Second), fxDeniedNet(ms(time.Second), sb))),
		"limit":      {Limit: 1, Lines: []SandboxLine{line(ms(time.Second), fxAllowedNet(ms(time.Second), sb))}},
		"unparsed":   readOf(line(ms(time.Second), fxAllowedNet(ms(time.Second), sb)), line(ms(time.Second), "x")),
		"downgraded": readOf(line(ms(time.Second), fxDowngraded(ms(time.Second)))),
		"shorthand":  readOf(line(ms(time.Second), fxShorthand)),
		// A record with no sandbox association omits `container` (ocsf-json-export.mdx:67): with an
		// empty sandbox id it would otherwise "match" container.uid == "".
		"nocontainer": readOf(line(ms(time.Second), strings.Replace(fxDeniedNet(ms(time.Second), sb), `,"container":{"uid":"sb-1"}`, "", 1))),
	}
	for name, r := range reads {
		for _, sandbox := range []string{sb, ""} {
			p := SandboxPolicyFor(r, sandbox, winA, nil)
			switch p.Coverage {
			case report.CoverageComplete, report.CoverageLossy, report.CoverageUnavailable:
			default:
				t.Errorf("%s/%q: coverage %q", name, sandbox, p.Coverage)
			}
			if strings.TrimSpace(p.CoverageReason) == "" {
				t.Errorf("%s/%q: empty coverage_reason", name, sandbox)
			}
			if p.Events == nil || !strings.Contains(policyJSON(t, p), `"events":[`) {
				t.Errorf("%s/%q: events must be an array", name, sandbox)
			}
			if sandbox == "" && p.Coverage != report.CoverageUnavailable && name != "error" {
				t.Errorf("%s: an empty sandbox id can never tie a line to the SUT, got %q", name, p.Coverage)
			}
		}
	}
}

// ── #448: what a target, a title and a reason may carry ─────────────────────
//
// Loom_HoP's review of #445: safeURL kept the whole path and target() copied finding_info.title, so a
// token in a path segment (`/bot<TOKEN>/sendMessage`) or in a title reached the block, and from there
// the builder's report and argus.build_record on a build run. Every fake token below is assembled at
// run time from a repeated pattern: none is a real credential, and no literal in this file has a
// real token's full shape.

// fakeMixed is 32 letters and digits mixed, the shape of a random base64 secret with no prefix.
var fakeMixed = strings.Repeat("k3Rq", 8)

// fakeHex is 32 hex digits with no digit 0-9 in it, so only the hex rule can catch it.
var fakeHex = strings.Repeat("deadbeef", 4)

// httpDenial is fxDeniedHTTP with its url object replaced by url (a JSON value).
func httpDenial(at int64, url string) string {
	return strings.Replace(fxDeniedHTTP(at, sb), `{"url_string":"http://api.github.com:443/user/repos","scheme":"http","hostname":"api.github.com","port":443,"path":"/user/repos"}`, url, 1)
}

func findingTitled(at int64, title string) string {
	return strings.Replace(fxFinding(at, sb), `"title":"configured content matched"`, `"title":`+strconv.Quote(title), 1)
}

func deniedBecause(at int64, reason string) string {
	return strings.Replace(fxDeniedNet(at, sb), `"status_detail":"no matching policy"`, `"status_detail":`+strconv.Quote(reason), 1)
}

// soleEvent builds the block for one line and returns its only event.
func soleEvent(t *testing.T, body string) report.SandboxPolicyEvent {
	t.Helper()
	p := SandboxPolicyFor(readOf(line(ms(time.Second), body)), sb, winA, nil)
	if count(p) != 1 || len(p.Events) != 1 {
		t.Fatalf("want exactly one kept event, got %s", policyJSON(t, p))
	}
	return p.Events[0]
}

func TestSandboxPolicy_TokenShapedPathSegmentNeverReachesBlock(t *testing.T) {
	cases := []struct {
		name, url, secret, want string
	}{
		{"bot token in a segment (the issue's example)",
			strconv.Quote("http://api.example.test:443/bot8123456789:" + fakeMixed + "/sendMessage"), fakeMixed,
			"POST http://api.example.test:443/[redacted]/sendMessage"},
		{"hex secret", strconv.Quote("https://hooks.example.test/hooks/" + fakeHex), fakeHex,
			"POST https://hooks.example.test/hooks/[redacted]"},
		{"known prefix, no digits (only the prefix rule can catch it)",
			strconv.Quote("https://api.example.test/v1/syn_" + strings.Repeat("ab", 10) + "/tools"), "syn_" + strings.Repeat("ab", 10),
			"POST https://api.example.test/v1/[redacted]/tools"},
		{"JWT", strconv.Quote("https://api.example.test/session/eyJ" + strings.Repeat("hbGc", 5) + "/x"), "eyJ" + strings.Repeat("hbGc", 5),
			"POST https://api.example.test/session/[redacted]/x"},
		{"Bearer value, percent-encoded in the path", strconv.Quote("https://api.example.test/a/Bearer%20s3cr3t-value/x"), "s3cr3t-value",
			"POST https://api.example.test/a/[redacted]/x"},
		{"URL object built from parts",
			`{"scheme":"http","hostname":"api.example.test","port":443,"path":"/bot8123456789:` + fakeMixed + `/sendMessage"}`, fakeMixed,
			"POST http://api.example.test:443/[redacted]/sendMessage"},
		{"not an absolute URL (the text fallback)", strconv.Quote("api.example.test/bot8123456789:" + fakeMixed + "/sendMessage"), fakeMixed,
			"POST api.example.test/[redacted]/sendMessage"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ev := soleEvent(t, httpDenial(ms(time.Second), c.url))
			b, _ := json.Marshal(ev)
			if strings.Contains(string(b), c.secret) {
				t.Errorf("a token-shaped path segment reached the block: %s", b)
			}
			if ev.Target != c.want {
				t.Errorf("target = %q, want %q", ev.Target, c.want)
			}
		})
	}
}

func TestSandboxPolicy_TokenShapedTitleAndReasonNeverReachBlock(t *testing.T) {
	titles := []struct{ title, secret, want string }{
		{"content matched ghp_" + fakeMixed + " in body", fakeMixed, "content matched [redacted] in body"},
		{"key " + fakeHex + " matched", fakeHex, "key [redacted] matched"},
		{"Authorization: Bearer s3cr3t-value in request", "s3cr3t-value", "Authorization: Bearer [redacted] in request"},
		{"token=syn_" + strings.Repeat("ab", 10), "syn_" + strings.Repeat("ab", 10), "[redacted]"},
	}
	for _, c := range titles {
		ev := soleEvent(t, findingTitled(ms(time.Second), c.title))
		if strings.Contains(ev.Target, c.secret) || ev.Target != c.want {
			t.Errorf("title %q: target = %q, want %q", c.title, ev.Target, c.want)
		}
	}
	reason := "denied https://api.example.test/bot8123456789:" + fakeMixed + "/x"
	if ev := soleEvent(t, deniedBecause(ms(time.Second), reason)); strings.Contains(ev.Reason, fakeMixed) || ev.Reason != "denied https://api.example.test/bot8123456789:[redacted]" {
		t.Errorf("reason = %q", ev.Reason)
	}
}

// The positive control: ordinary paths, titles and reasons are carried as they were logged — a UUID,
// a long slug, a pull number, a CamelCase word of 36 letters with no digit.
func TestSandboxPolicy_OrdinaryPathTitleAndReasonStillShow(t *testing.T) {
	for _, u := range []string{
		"http://api.github.com:443/repos/example/argus-runner/pulls/448/files",
		"https://api.example.test/v1/projects/3f2c1a4e-9b7d-4e2a-8c1f-0a9b8c7d6e5f/chat/completions",
		"https://api.example.test/",
	} {
		if got := soleEvent(t, httpDenial(ms(time.Second), strconv.Quote(u))).Target; got != "POST "+u {
			t.Errorf("an ordinary path changed: target = %q, want %q", got, "POST "+u)
		}
	}
	for _, title := range []string{
		"configured content matched",
		"content_guard.match on POST /v1/chat/completions for api.github.com",
		"ContentGuardMatchedConfiguredPatternRule",
	} {
		if got := soleEvent(t, findingTitled(ms(time.Second), title)).Target; got != title {
			t.Errorf("an ordinary title changed: %q, want %q", got, title)
		}
	}
	for _, reason := range []string{"no matching policy", "l7 deny", "DNS resolution failed for nope.invalid:443", "denied https://api.example.test/user/repos"} {
		if got := soleEvent(t, deniedBecause(ms(time.Second), reason)).Reason; got != reason {
			t.Errorf("an ordinary reason changed: %q, want %q", got, reason)
		}
	}
}

// The bound: at most the first 8 path segments, then one "…"; a title cut to 128 runes.
func TestSandboxPolicy_PathAndTitleAreBounded(t *testing.T) {
	var segs []string
	for i := 1; i <= 12; i++ {
		segs = append(segs, "s"+strconv.Itoa(i))
	}
	got := soleEvent(t, httpDenial(ms(time.Second), strconv.Quote("https://api.example.test/"+strings.Join(segs, "/")))).Target
	if want := "POST https://api.example.test/s1/s2/s3/s4/s5/s6/s7/s8/…"; got != want {
		t.Errorf("12 segments: target = %q, want %q", got, want)
	}
	exact := "https://api.example.test/" + strings.Join(segs[:8], "/")
	if got := soleEvent(t, httpDenial(ms(time.Second), strconv.Quote(exact))).Target; got != "POST "+exact {
		t.Errorf("8 segments are kept whole: target = %q", got)
	}
	title := soleEvent(t, findingTitled(ms(time.Second), strings.Repeat("é", 300))).Target
	if n := len([]rune(title)); n != 128 {
		t.Errorf("title has %d runes, want 128", n)
	}
}

// A title is free text too: a URL in it loses userinfo and query string, as a reason's does, so "no
// userinfo, no query string" holds for every field of an event (#448).
func TestSandboxPolicy_TitleLosesUserinfoAndQueryString(t *testing.T) {
	ev := soleEvent(t, findingTitled(ms(time.Second), "fetched https://bob:pw-CANARY@api.example.test/x?key=CANARY-Q in body"))
	if strings.Contains(ev.Target, "CANARY") || ev.Target != "fetched https://api.example.test/x in body" {
		t.Fatalf("title target = %q, want %q", ev.Target, "fetched https://api.example.test/x in body")
	}
}

// ── #448, second pass: what the first shape check and the text branch let through ──
//
// A review of the first fix measured that a real secret is usually written with "-", "_", "+" or "/"
// in it, which cut it into pieces of letters and digits too short for the 24-character rule (about a
// third of random base64url secrets passed), and that the text branch kept userinfo holding a "/" or
// white space. Every fake secret below is assembled at run time; none is a real credential.

// fakeB64URL is a base64url secret with a "-" in the middle: no run of letters and digits alone
// reaches 24 (19, then 16).
var fakeB64URL = "AAH" + strings.Repeat("k3Rq", 4) + "-" + strings.Repeat("Zp7w", 4)

// fakeStdB64 is a standard-base64 secret cut by "/" and "+" into pieces of 12, 12 and 8.
var fakeStdB64 = strings.Repeat("k3Rq", 3) + "/" + strings.Repeat("Zp7w", 3) + "+" + strings.Repeat("Ab9x", 2)

// httpDenialWithEndpoint is httpDenial with a dst_endpoint, so a dropped URL falls back to it.
func httpDenialWithEndpoint(at int64, url string) string {
	return strings.Replace(httpDenial(at, url), `"status_detail":"l7 deny",`, `"status_detail":"l7 deny","dst_endpoint":{"domain":"api.example.test","port":443},`, 1)
}

func TestSandboxPolicy_SeparatedTokenNeverReachesBlock(t *testing.T) {
	underscored := "AAH" + strings.Repeat("k3Rq", 3) + "_" + strings.Repeat("Zp7w", 4)
	paths := []struct{ name, url, secret, want string }{
		{"bot token with '-' (the issue's /bot<TOKEN>/sendMessage)",
			"https://api.example.test/bot8123456789:" + fakeB64URL + "/sendMessage", fakeB64URL,
			"POST https://api.example.test/[redacted]/sendMessage"},
		{"base64url key with '_'", "https://api.example.test/v1/keys/" + underscored + "/x", underscored,
			"POST https://api.example.test/v1/keys/[redacted]/x"},
	}
	for _, c := range paths {
		if got := soleEvent(t, httpDenial(ms(time.Second), strconv.Quote(c.url))).Target; strings.Contains(got, c.secret) || got != c.want {
			t.Errorf("%s: target = %q, want %q", c.name, got, c.want)
		}
	}
	if got := soleEvent(t, findingTitled(ms(time.Second), "content matched "+fakeB64URL+" in body")).Target; got != "content matched [redacted] in body" {
		t.Errorf("title target = %q", got)
	}
	if got := soleEvent(t, deniedBecause(ms(time.Second), "secret "+fakeStdB64+" rejected")).Reason; got != "secret [redacted] rejected" {
		t.Errorf("reason = %q", got)
	}
}

// The measured rate: 20000 seeded random secrets of each kind. A secret escapes when its first or last
// 12 characters survive. Before this pass about a third escaped; the bound is 1% (200 of 20000).
func TestSandboxPolicy_RandomSecretsEscapeUnderOnePercent(t *testing.T) {
	r := rand.New(rand.NewSource(448))
	random := func(n int) []byte {
		b := make([]byte, n)
		for i := range b {
			b[i] = byte(r.Intn(256))
		}
		return b
	}
	kinds := []struct {
		name  string
		gen   func() string
		carry func(secret string) string
	}{
		{"base64url, 32 bytes, in a path segment", func() string { return base64.RawURLEncoding.EncodeToString(random(32)) },
			func(s string) string { return safePath("/v1/keys/" + s + "/x") }},
		{"bot token in a path segment", func() string { return "AAH" + base64.RawURLEncoding.EncodeToString(random(24))[:32] },
			func(s string) string { return safePath("/bot8123456789:" + s + "/sendMessage") }},
		{"standard base64, 30 bytes, in a reason", func() string { return base64.StdEncoding.EncodeToString(random(30)) },
			func(s string) string { return safeReason("secret " + s + " rejected") }},
		{"base64url, 32 bytes, in a title", func() string { return base64.RawURLEncoding.EncodeToString(random(32)) },
			func(s string) string { return safeTitle("content matched " + s + " in body") }},
	}
	for _, k := range kinds {
		escaped := 0
		for i := 0; i < 20000; i++ {
			s := k.gen()
			if out := k.carry(s); strings.Contains(out, s[:12]) || strings.Contains(out, s[len(s)-12:]) {
				escaped++
			}
		}
		t.Logf("%s: %d of 20000 escaped", k.name, escaped)
		if escaped > 200 {
			t.Errorf("%s: %d of 20000 random secrets reached the block, want at most 200", k.name, escaped)
		}
	}
}

// An HTTP target never carries userinfo. On the text branch (a URL that does not parse) the rule
// cannot tell where userinfo holding a "/" or white space ends, or where a scheme-less "user:pw@host"
// begins, so a URL that still holds an "@" is dropped and the target falls back to dst_endpoint.
func TestSandboxPolicy_HTTPTargetNeverCarriesUserinfo(t *testing.T) {
	for _, u := range []string{
		"https://admin:pw/CANARY-SL@api.example.test/x",
		"https://admin:pw CANARY-SP@api.example.test/x",
		"admin:CANARY-NS@api.example.test/x",
		"https://api.example.test:xx/npm/@scope/pkg",
	} {
		ev := soleEvent(t, httpDenialWithEndpoint(ms(time.Second), strconv.Quote(u)))
		if strings.Contains(ev.Target, "CANARY") || ev.Target != "POST api.example.test:443" {
			t.Errorf("url %q: target = %q, want %q", u, ev.Target, "POST api.example.test:443")
		}
	}
	// A URL that parses keeps an "@" in its path.
	ok := "https://api.example.test/npm/@scope/pkg"
	if got := soleEvent(t, httpDenialWithEndpoint(ms(time.Second), strconv.Quote(ok))).Target; got != "POST "+ok {
		t.Errorf("a parsed path with an @ changed: %q", got)
	}
}

// On the text branch the 8-segment bound counts path segments only, as on the parsed branch: the
// scheme and the host are not segments.
func TestSandboxPolicy_TextBranchKeepsEightPathSegments(t *testing.T) {
	got := soleEvent(t, httpDenial(ms(time.Second), strconv.Quote("https://api.example.test:xx/1/2/3/4/5/6/7/8/9"))).Target
	if want := "POST https://api.example.test:xx/1/2/3/4/5/6/7/8/…"; got != want {
		t.Errorf("target = %q, want %q", got, want)
	}
}

// The coverage_reason of a downgraded window names metadata.version: redacted like a reason, and cut
// to 32 runes.
func TestSandboxPolicy_DowngradedVersionIsRedactedAndBounded(t *testing.T) {
	withVersion := func(v string) *report.SandboxPolicy {
		body := strings.Replace(fxDowngraded(ms(time.Second)), `"version":"1.3.0"`, `"version":`+strconv.Quote(v), 1)
		return SandboxPolicyFor(readOf(line(ms(time.Second), body)), sb, winA, nil)
	}
	if p := withVersion("1.3.0 build " + fakeMixed); strings.Contains(p.CoverageReason, fakeMixed) || !strings.Contains(p.CoverageReason, "metadata.version 1.3.0 build [redacted])") {
		t.Errorf("coverage_reason = %q", p.CoverageReason)
	}
	if p := withVersion(strings.Repeat("v", 100000)); !strings.Contains(p.CoverageReason, "metadata.version "+strings.Repeat("v", 32)+")") {
		t.Errorf("a long version is not cut to 32 runes: %d runes of coverage_reason", len([]rune(p.CoverageReason)))
	}
}

// The stated thresholds and prefixes, each at its edge: one short of it is kept, at it is redacted.
func TestSandboxPolicy_TokenShapeBoundaries(t *testing.T) {
	letters := func(n int) string { return strings.Repeat("qz", 9)[:n] } // no digit, no hex, no upper case
	type tc struct{ name, value string }
	kept := []tc{
		{"23 hex digits", strings.Repeat("deadbeef", 3)[:23]},
		{"23 letters and digits", strings.Repeat("k3Rq", 6)[:23]},
		{"23 base64url characters", strings.Repeat("k3Rq", 3)[:11] + "-" + strings.Repeat("Zp7w", 3)[:11]},
		{"a 7-character mixed piece in a 24+ name", "release-Ab3defg-candidate-x"},
	}
	redacted := []tc{
		{"24 hex digits", strings.Repeat("deadbeef", 3)},
		{"24 letters and digits", strings.Repeat("k3Rq", 6)},
		{"24 base64url characters", strings.Repeat("k3Rq", 3)[:11] + "-" + strings.Repeat("Zp7w", 3)[:12]},
		{"an 8-character mixed piece in a 24+ name", "release-Ab3defgh-candidate-x"},
	}
	for _, p := range []string{"syn_", "sk-", "sk_", "ghp_", "gho_", "ghu_", "ghs_", "ghr_", "github_pat_", "glpat-", "xoxb-", "xoxp-", "npm_", "AKIA", "eyJ"} {
		kept = append(kept, tc{p + " and 15", p + letters(15)})
		redacted = append(redacted, tc{p + " and 16", p + letters(16)})
	}
	for _, c := range kept {
		if got := safeReason("x " + c.value + " y"); got != "x "+c.value+" y" {
			t.Errorf("%s: %q became %q, want it kept", c.name, c.value, got)
		}
	}
	for _, c := range redacted {
		if got := safeReason("x " + c.value + " y"); got != "x [redacted] y" {
			t.Errorf("%s: %q became %q, want [redacted]", c.name, c.value, got)
		}
	}
}

// The other positive control: a word after Basic or Bearer, and a long hyphenated name, are not
// tokens. Only a value after Basic or Bearer that is not a plain word is redacted.
func TestSandboxPolicy_WordsAndHyphenatedNamesStillShow(t *testing.T) {
	for _, title := range []string{"Basic auth credentials sent over plain HTTP", "Bearer token missing"} {
		if got := soleEvent(t, findingTitled(ms(time.Second), title)).Target; got != title {
			t.Errorf("an ordinary title changed: %q, want %q", got, title)
		}
	}
	for _, reason := range []string{"basic policy denies host", "denied https://api.example.test/api/v2/Orders/ORD-2026-0001/items"} {
		if got := soleEvent(t, deniedBecause(ms(time.Second), reason)).Reason; got != reason {
			t.Errorf("an ordinary reason changed: %q, want %q", got, reason)
		}
	}
	for _, u := range []string{
		"https://api.example.test/plans/Basic%20plan/x",
		"https://api.github.com/repos/o/r/git/refs/heads/CAT-6610-Add-Change-Notice-Bot",
		"https://api.github.com/repos/o/r/branches/Feature-Branch-2026-Q4-Release",
	} {
		if got := soleEvent(t, httpDenial(ms(time.Second), strconv.Quote(u))).Target; got != "POST "+u {
			t.Errorf("an ordinary path changed: target = %q, want %q", got, "POST "+u)
		}
	}
	if got := safeReason("Authorization: Basic dXNlcjpwYXNz rejected"); got != "Authorization: Basic [redacted] rejected" {
		t.Errorf("a Basic credential is not redacted: %q", got)
	}
}
