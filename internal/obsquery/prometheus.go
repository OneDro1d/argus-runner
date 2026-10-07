package obsquery

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// PrometheusClient is the real HTTP implementation of PromQuery (AC-11's survival-plane read):
// Prometheus's instant-query API (`/api/v1/query`), read-only, best-effort like every obsquery
// client — a query that errors or comes back empty answers (0, false, err-or-nil), never a panic.
type PrometheusClient struct {
	Base string
	HTTP *http.Client
}

// NewPrometheusClient builds a client against base (e.g. "http://prometheus:9090"). A 5s timeout,
// matching the other best-effort obs reads in this package — a hung Prometheus must never hang a run.
func NewPrometheusClient(base string) *PrometheusClient {
	return &PrometheusClient{Base: strings.TrimRight(base, "/"), HTTP: &http.Client{Timeout: 5 * time.Second}}
}

type promInstantResponse struct {
	Status string `json:"status"`
	Data   struct {
		Result []struct {
			Value [2]any `json:"value"` // [unix_seconds(number), "value"(string)]
		} `json:"result"`
	} `json:"data"`
}

// InstantValue reads ONE query at a point in time (`time=` on the instant-query API). ok is false
// on anything short of a genuine numeric result — an empty result set, a non-"success" status, or a
// transport/decode error (returned alongside) are all "no value", never a fabricated zero-with-ok.
func (c *PrometheusClient) InstantValue(query string, at time.Time) (float64, bool, error) {
	if c == nil || c.Base == "" {
		return 0, false, fmt.Errorf("obsquery: no Prometheus endpoint configured")
	}
	u := c.Base + "/api/v1/query?query=" + url.QueryEscape(query) + "&time=" + strconv.FormatInt(at.Unix(), 10)
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return 0, false, err
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return 0, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, false, fmt.Errorf("obsquery: prometheus responded %d", resp.StatusCode)
	}
	var pr promInstantResponse
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		return 0, false, err
	}
	if pr.Status != "success" || len(pr.Data.Result) == 0 {
		return 0, false, nil
	}
	s, ok := pr.Data.Result[0].Value[1].(string)
	if !ok {
		return 0, false, nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false, err
	}
	return v, true, nil
}

func (c *PrometheusClient) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}
