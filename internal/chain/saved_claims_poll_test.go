package chain

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// roundTripFunc lets a test stand in for the network AND run code in the step's own goroutine
// between attempts, so the capture store can change mid-poll without a data race.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// A polled step binds its claims on EVERY attempt. In production the store does
// not change while one step polls, so the contract is pinned by changing it between attempts: attempts
// 1 and 2 compare against n=10 (7 > 10 misses), attempt 3 sees n=5 (7 > 5 passes). A step that bound
// once before the loop would keep missing until the poll timed out.
func TestSavedThreshold_Polled_BindsOnEveryAttempt(t *testing.T) {
	old := httpClient
	defer func() { httpClient = old }()
	attempt := 0
	vars := map[string]string{"n": "10"}
	httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		attempt++
		if attempt == 2 {
			vars["n"] = "5"
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"count": 7}`)),
			Header: http.Header{}, Request: r}, nil
	})}
	poll := &Poll{Timeout: 2 * time.Second, Interval: time.Millisecond}
	st := gtSaved("http://sut.invalid", "/b", poll, "count").Run("cid", vars)
	if st.Status != "passed" || attempt != 3 {
		t.Fatalf("want a pass on attempt 3, got status=%s after %d attempt(s): %+v", st.Status, attempt, st)
	}
}
