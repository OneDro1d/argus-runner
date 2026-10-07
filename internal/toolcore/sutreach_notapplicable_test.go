package toolcore

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// VR7-J1 (V24-001) — A TARGET THAT CANNOT BE DIALLED BY ITS NATURE MUST NOT POISON THE VERDICT.
//
// ── THE DEFECT, IN ITS OWN WORDS ──────────────────────────────────────────────────────────────────
//
// dialAddrFor's `auth` case says it plainly: "Permanently unprobeable BY SHAPE: {type, bearer_token}
// contains no address. Saying so is the point — an operator must be able to tell 'checked and fine'
// from 'never checkable'."
//
// That is correct and deliberate. But SUTReachableTriState then counted it in `unknown`, and one
// unknown forces the whole verdict to nil — so a SUT whose every DIALABLE target answered still
// reported "not measured". Measured 2026-08-19 across a 7-instance estate:
//
//	Social   ["mcp"]                                             -> true    (3 instances)
//	Memstore   ["http","mcp"]                                      -> true    (2 instances)
//	Order    ["http","message_broker","database","external","auth"] -> NULL  (2 instances)
//
// Config-shaped, not tier-shaped: OrderService failed on two different tiers, Social succeeded on
// three.
//
// ── WHY A FOURTH STATUS AND NOT "IGNORE EVERYTHING WITHOUT AN ADDRESS" ────────────────────────────
//
// 🚨 The narrower rule is the load-bearing one. "No address" covers TWO different facts:
//
//	auth, external-in-LIST-form   the shape genuinely has no single address    -> NOT APPLICABLE
//	an EMPTY jdbc_url / base_url  an address was expected and is missing       -> still UNKNOWN
//
// Excluding both would let a SUT that declares a database and forgets its URL report REACHABLE while
// its database was never dialled — absence rendered as health, which is the exact failure this round
// exists to remove. So `not_applicable` is granted only where no address can exist, and a config gap
// keeps poisoning the verdict as it does today.
func TestSUTReachableTriState_AnUnprobeableTargetDoesNotPoisonTheVerdict(t *testing.T) {
	// A live listener so the http target genuinely answers — this is the whole chain, config -> dial
	// -> tri-state, not a hand-built Reachability.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer srv.Close()
	host, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatalf("split listener addr: %v", err)
	}

	// OrderService's real shape, reduced to the two targets that matter for this rule.
	cfgPath := filepath.Join(t.TempDir(), "argus-config.yaml")
	cfg := fmt.Sprintf(`project:
  name: probe-notapplicable
targets:
  http:
    base_url: http://%s:%s
  auth:
    type: bearer
    bearer_token: irrelevant-to-a-dial
`, host, port)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	c, err := loadConfigForProbe(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	rep := ProbeTargets(c, 750*time.Millisecond)

	var authStatus string
	for _, tr := range rep.Targets {
		if tr.Target == "auth" {
			authStatus = tr.Status
		}
	}
	if authStatus != ReachNotApplicable {
		t.Fatalf("the auth target reported status %q, want %q.\n"+
			"  dialAddrFor already knows it is \"permanently unprobeable BY SHAPE\". That fact has to\n"+
			"  reach the tri-state as its own status, or the tri-state cannot tell it apart from a\n"+
			"  target whose address is missing by mistake.", authStatus, ReachNotApplicable)
	}

	got := SUTReachableTriState(rep)
	if got == nil || !*got {
		t.Fatalf("a SUT whose every DIALABLE target answered reported %s, want true.\n"+
			"  Its only other target is `auth`, which contains a credential rather than an address —\n"+
			"  there was never anything to dial. Counting it as \"unknown\" is what made both\n"+
			"  OrderService instances unreportable while Social and Memstore reported fine.", boolPtrStr(got))
	}
}

// ⚠ THE GUARD ON THE FIX. A MISSING address is not the same as NO address, and this case keeps the
// narrowing honest: a broker whose queues are declared but whose URL is absent must still poison the
// verdict, because the broker is real, the runner must reach it, and nobody said where it is.
//
// ⚠ `message_broker` and not `database`, deliberately. config.go:606 makes a database "present" only
// when its jdbc_url is non-empty, so an empty one is filtered out before the probe and never reaches
// dialAddrFor at all — that config says "no database target", which is a legitimate way to disable it.
// config.go:604 makes a broker present as soon as the block exists, so THIS is the path where a target
// is declared and its address is missing. It is the only one that can distinguish the two rules.
func TestSUTReachableTriState_AMissingAddressStillPoisonsTheVerdict(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer srv.Close()
	host, port, _ := net.SplitHostPort(srv.Listener.Addr().String())

	cfgPath := filepath.Join(t.TempDir(), "argus-config.yaml")
	cfg := fmt.Sprintf(`project:
  name: probe-missing-addr
targets:
  http:
    base_url: http://%s:%s
  message_broker:
    type: amqp
    queues: {incoming: orders.incoming.q}
`, host, port)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	c, err := loadConfigForProbe(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	rep := ProbeTargets(c, 750*time.Millisecond)

	for _, tr := range rep.Targets {
		if tr.Target == "message_broker" && tr.Status != ReachUnknown {
			t.Fatalf("message_broker reported %q, want %q.\n"+
				"  Its queues are declared and its address is not. That is a GAP, and a gap must keep\n"+
				"  poisoning the verdict — only a shape that can never carry an address is exempt.",
				tr.Status, ReachUnknown)
		}
	}
	if got := SUTReachableTriState(rep); got != nil {
		t.Fatalf("a SUT that DECLARES a broker and gives it no address reported %s, want nil.\n"+
			"  If this reports reachable, the fix has traded one silent lie for another.", boolPtrStr(got))
	}
}
