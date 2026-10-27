package scenario

import (
	"encoding/json"
	"testing"
)

// moneyallow_golden_test.go — the wire contract of point 1/9 of the engine-allow change: a
// money_writes list of TODAY'S shape (HTTP entries only) must serialise BYTE-IDENTICALLY after the
// entry type grew its engine fields, because an executor reports this JSON and an older control plane
// decodes it. The literal below was produced by, and first asserted against, the base commit
// 91b6833d779dc9b0a029d97aa89df3a468f68e06 (before any change).

const goldenTodayShapedAllowJSON = `[{"method":"POST","path":"/api/v1/trading/quote","spends":false},` +
	`{"method":"POST","path":"/api/v1/trading/order","spends":true,"amount_field":"source_amount","max_amount":25,"max_per_run":4}]`

func TestMoneyWriteAllow_TodayShapedListSerialisesByteIdentically(t *testing.T) {
	al := MoneyWriteAllowlist{
		{Method: "POST", Path: "/api/v1/trading/quote", Spends: false},
		{Method: "POST", Path: "/api/v1/trading/order", Spends: true, AmountField: "source_amount", MaxAmount: 25, MaxPerRun: 4},
	}
	got, err := json.Marshal(al)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != goldenTodayShapedAllowJSON {
		t.Fatalf("today-shaped list changed on the wire:\n got: %s\nwant: %s", got, goldenTodayShapedAllowJSON)
	}
	// the empty list is the reported "none" and must stay "[]" (NULL means never reported)
	empty, _ := json.Marshal(MoneyWriteAllowlist{})
	if string(empty) != "[]" {
		t.Fatalf("empty list = %s, want []", empty)
	}
}
