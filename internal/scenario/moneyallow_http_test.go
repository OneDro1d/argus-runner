package scenario

import (
	"net/url"
	"strings"
	"testing"
)

// moneyallow_http_test.go — the two widenings of an HTTP money_writes entry:
// a GET entry, and a whole-segment `{name}` placeholder in the path. Everything else stays exact.

func getOrderEntry() MoneyWriteAllow {
	return MoneyWriteAllow{Method: "GET", Path: "/api/v1/orders/{id}", Spends: false}
}

func TestMatch_PlaceholderMatchesExactlyOneNonEmptySegment(t *testing.T) {
	al := MoneyWriteAllowlist{getOrderEntry()}
	for _, tc := range []struct {
		name, url string
		want      bool
	}{
		{"one segment", "https://x.example/api/v1/orders/42", true},
		{"a templated segment (the file's own text)", "${INGESTION_URL}/api/v1/orders/${saved.id}", true},
		{"query is stripped", "/api/v1/orders/42?expand=1", true},
		{"empty segment", "/api/v1/orders/", false},
		{"missing segment", "/api/v1/orders", false},
		{"extra segment", "/api/v1/orders/42/cancel", false},
		{"two segments in the slot", "/api/v1/orders/42/43", false},
		{"trailing slash", "/api/v1/orders/42/", false},
		{"dot segment", "/api/v1/orders/.", false},
		{"dot-dot segment", "/api/v1/orders/..", false},
		{"another prefix", "/api/v2/orders/42", false},
		{"case differs", "/api/v1/Orders/42", false},
	} {
		if got := al.Match("GET", tc.url) != nil; got != tc.want {
			t.Errorf("%s: Match(GET, %q) = %v, want %v", tc.name, tc.url, got, tc.want)
		}
	}
	if al.Match("POST", "/api/v1/orders/42") != nil {
		t.Error("a GET entry matched a POST")
	}
}

func TestMatch_TwoPlaceholders(t *testing.T) {
	al := MoneyWriteAllowlist{{Method: "POST", Path: "/api/v1/users/{uid}/orders/{oid}/cancel", Spends: false}}
	if al.Match("POST", "/api/v1/users/7/orders/9/cancel") == nil {
		t.Error("two placeholders did not match two single segments")
	}
	for _, u := range []string{
		"/api/v1/users/7/orders/cancel",
		"/api/v1/users/7/8/orders/9/cancel",
		"/api/v1/users//orders/9/cancel",
		"/api/v1/users/7/orders/9/cancel/",
		"/api/v1/users/7/orders/9/confirm",
	} {
		if al.Match("POST", u) != nil {
			t.Errorf("Match(POST, %q) matched; it must not", u)
		}
	}
}

func TestMatch_ExactEntryBeatsAPlaceholderEntry(t *testing.T) {
	al := MoneyWriteAllowlist{
		{Method: "POST", Path: "/api/v1/orders/{id}", Spends: true, AmountField: "a", MaxAmount: 1, MaxPerRun: 1},
		{Method: "POST", Path: "/api/v1/orders/preview", Spends: false},
	}
	if e := al.Match("POST", "/api/v1/orders/preview"); e == nil || e.Path != "/api/v1/orders/preview" {
		t.Fatalf("the exact entry did not win over the placeholder entry listed before it: %+v", e)
	}
	if e := al.Match("POST", "/api/v1/orders/77"); e == nil || e.Path != "/api/v1/orders/{id}" {
		t.Fatalf("the placeholder entry did not cover another id: %+v", e)
	}
}

func TestMatchResolved_AnEncodedSlashOrDotsInAPlaceholderSegmentNeverMatches(t *testing.T) {
	al := MoneyWriteAllowlist{getOrderEntry()}
	for _, tc := range []struct {
		raw  string
		want bool
	}{
		{"https://x.example/api/v1/orders/42", true},
		{"https://x.example/api/v1/orders/a-b_c", true},
		{"https://x.example/api/v1/orders/1%2Fcancel", false},
		{"https://x.example/api/v1/orders/1%2fcancel", false},
		{"https://x.example/api/v1/orders/1%5Ccancel", false},
		{"https://x.example/api/v1/orders/%2e%2e", false},
		{"https://x.example/api/v1/orders/%2E", false},
		{"https://x.example/api/v1/orders/..", false},
		{"https://x.example/api/v1/orders/1/cancel", false},
		{"https://x.example/api/v1/orders/", false},
		{"https://x.example/api/v1/orders/1?x=%2F", true},
	} {
		u, err := url.Parse(tc.raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := al.MatchResolved("GET", u) != nil; got != tc.want {
			t.Errorf("MatchResolved(GET, %q) = %v, want %v", tc.raw, got, tc.want)
		}
	}
	// a decoded "?" is data, not a query: it must not truncate the path into a listed one
	exact := MoneyWriteAllowlist{{Method: "POST", Path: "/api/v1/order", Spends: false}}
	u, _ := url.Parse("https://x.example/api/v1/order%3Fx")
	if exact.MatchResolved("POST", u) != nil {
		t.Error("a path with a decoded %3F matched the exact entry that is its prefix")
	}
}

func TestGETEntry_ExemptsAMoneyWordPathForThatRouteOnly(t *testing.T) {
	al := MoneyWriteAllowlist{getOrderEntry()}
	ok := httpScenario("GET", "https://x.example/api/v1/orders/42", naCleanup)
	if v := MoneyGuardViolations(ok, al); len(v) != 0 {
		t.Fatalf("a GET of the listed order route was refused: %v", v)
	}
	// refused without the entry — the sentence is the base one
	v := MoneyGuardViolations(ok, nil)
	if len(v) != 1 || !strings.Contains(v[0], `"orders" path`) {
		t.Fatalf("without the entry the GET of an order path must be refused as it always was: %v", v)
	}
	// another id SHAPE (an extra segment) is not the listed route
	for _, u := range []string{
		"https://x.example/api/v1/orders/42/cancel",
		"https://x.example/api/v1/orders",
		"https://x.example/api/v1/orders/",
		"https://x.example/api/v1/quotes/42",
	} {
		if v := MoneyGuardViolations(httpScenario("GET", u, naCleanup), al); len(v) == 0 {
			t.Errorf("GET %s was allowed by an entry that does not name it", u)
		}
	}
	// and the entry is for GET only
	if v := MoneyGuardViolations(httpScenario("DELETE", "https://x.example/api/v1/orders/42", naCleanup), al); len(v) == 0 {
		t.Error("a GET entry allowed a DELETE")
	}
}

func TestPlaceholderEntry_SpendsIsBoundedInOneLedgerBucketAcrossIds(t *testing.T) {
	entry := MoneyWriteAllow{Method: "POST", Path: "/api/v1/orders/{id}/fill", Spends: true,
		AmountField: "amount", MaxAmount: 10, MaxPerRun: 2}
	al := MoneyWriteAllowlist{entry}
	var l MoneySpendLedger
	for i, id := range []string{"a1", "b2", "c3"} {
		u, _ := url.Parse("https://x.example/api/v1/orders/" + id + "/fill")
		e := al.MatchResolved("POST", u)
		if e == nil || !e.Spends {
			t.Fatalf("id %s: the spending entry did not match", id)
		}
		ok, _ := l.Reserve(e)
		if want := i < 2; ok != want {
			t.Errorf("id %s (spend %d): Reserve = %v, want %v — two ids must share ONE max_per_run bucket", id, i+1, ok, want)
		}
	}
	if msg := EvaluateSpendAmount(&entry, `{"amount": 11}`); msg == "" || !strings.Contains(msg, "/api/v1/orders/{id}/fill") {
		t.Errorf("the amount check must bound a placeholder entry and name the ENTRY: %q", msg)
	}
}

func TestResolvedRequestViolations_SameRuleAsTheFile(t *testing.T) {
	al := MoneyWriteAllowlist{getOrderEntry()}
	parse := func(s string) *url.URL {
		u, err := url.Parse(s)
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	const tmpl = "${INGESTION_URL}/api/v1/orders/${saved.id}"
	for _, tc := range []struct {
		name, method, resolved string
		allow                  MoneyWriteAllowlist
		refused                bool
	}{
		{"the entry still covers it", "GET", "https://x.example/api/v1/orders/42", al, false},
		{"saved value climbs to another route", "GET", "https://x.example/api/v1/orders/42/cancel", al, true},
		{"saved value is an encoded slash", "GET", "https://x.example/api/v1/orders/42%2Fcancel", al, true},
		{"saved value is dot-dot", "GET", "https://x.example/api/v1/orders/..", al, true},
		{"no entry needed: a plain read of a non-money path", "GET", "https://x.example/api/v1/items/42", nil, false},
		{"no entry, and the value made it a money path", "GET", "https://x.example/api/v1/items/orders", nil, true},
		{"no entry, and the value climbed to a money path", "GET", "https://x.example/api/v1/items/../orders/1", nil, true},
		{"no entry, money word in the query", "GET", "https://x.example/api/v1/items?kind=order", nil, true},
		{"an encoded money word", "GET", "https://x.example/api/v1/items/%6frders", nil, true},
		{"the host is never read", "GET", "https://orders.example/api/v1/items/1", nil, false},
		{"a write needs its entry", "POST", "https://x.example/api/v1/items/1", nil, true},
	} {
		v := ResolvedRequestViolations("chain step s", tc.method, tmpl, parse(tc.resolved), tc.allow)
		if (len(v) > 0) != tc.refused {
			t.Errorf("%s: violations = %v, refused want %v", tc.name, v, tc.refused)
		}
		for _, s := range v {
			if strings.Contains(s, "42") || strings.Contains(s, tc.resolved) {
				t.Errorf("%s: a sentence echoes the resolved url (a saved value may be a secret): %q", tc.name, s)
			}
		}
	}
	if v := ResolvedRequestViolations("chain step s", "GET", tmpl, nil, al); len(v) == 0 {
		t.Error("an unreadable url must be refused")
	}
}
