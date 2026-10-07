package obsquery

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestPrometheusClient_InstantValue exercises the real HTTP client against a fake Prometheus
// instant-query endpoint (AC-11) — the JSON shape actual Prometheus returns.
func TestPrometheusClient_InstantValue(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("query") == "" || r.URL.Query().Get("time") == "" {
			t.Errorf("expected query+time params, got %s", r.URL.RawQuery)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "success",
			"data": map[string]any{
				"result": []map[string]any{
					{"value": []any{1789900000, "7.5"}},
				},
			},
		})
	}))
	defer srv.Close()

	c := NewPrometheusClient(srv.URL)
	v, ok, err := c.InstantValue("sum(up)", time.Now())
	if err != nil {
		t.Fatalf("InstantValue: %v", err)
	}
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if v != 7.5 {
		t.Fatalf("v = %v, want 7.5", v)
	}
}

// An empty result set is "no value", not an error and not a fabricated zero.
func TestPrometheusClient_InstantValue_EmptyResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "success",
			"data":   map[string]any{"result": []map[string]any{}},
		})
	}))
	defer srv.Close()

	c := NewPrometheusClient(srv.URL)
	_, ok, err := c.InstantValue("sum(up)", time.Now())
	if err != nil {
		t.Fatalf("InstantValue: %v", err)
	}
	if ok {
		t.Fatal("ok = true on an empty result set, want false")
	}
}

// An unconfigured client (no base) never dials anywhere and answers an error, not a panic.
func TestPrometheusClient_InstantValue_NoEndpoint(t *testing.T) {
	c := NewPrometheusClient("")
	_, ok, err := c.InstantValue("sum(up)", time.Now())
	if err == nil || ok {
		t.Fatalf("want (false, err) with no endpoint, got ok=%v err=%v", ok, err)
	}
}
