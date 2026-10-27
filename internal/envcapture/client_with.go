package envcapture

import "net/http"

// NewClientWith builds a Client against an explicit API base URL and bearer token, for callers whose tests put an
// httptest.Server where the API server would be (internal/precondition's KubeAPI). Production code uses
// NewInClusterClient; nothing in a pod ever calls this.
func NewClientWith(baseURL, token string, hc *http.Client) *Client {
	if hc == nil {
		hc = http.DefaultClient
	}
	return &Client{baseURL: baseURL, token: token, hc: hc}
}
