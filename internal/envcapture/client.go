package envcapture

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Client issues plain HTTP GETs against a Kubernetes API server, bearer-authenticated. It is the
// SAME shape internal/runner/autoscale.go's deploymentAPI/scaleDeployment use for the executor's
// self-delete/right-size calls — deliberately NO client-go dependency for one more read-only,
// low-volume caller. baseURL/token/hc are set directly by NewInClusterClient in production and by
// this package's own tests (an httptest.Server has no "SA token" to read from disk).
type Client struct {
	baseURL string
	token   string
	// tokenPath, when set, is re-read for EVERY request (a projected ServiceAccount token is rotated
	// by the kubelet and expires; a long-lived follower must not hold the one it started with). Set
	// only by NewFileTokenClient; every other Client keeps the fixed token above.
	tokenPath string
	// listTimeout, when >0, is the OVERALL deadline of ListRunning (headers AND body). 0 = DefaultListTimeout
	// for a client built with a tokenPath. The stream client has no overall timeout by design, so the pod
	// LIST, which shares it, carries its own.
	listTimeout time.Duration
	hc          *http.Client
}

// DefaultListTimeout is the overall deadline of one ListRunning call.
const DefaultListTimeout = 15 * time.Second

// WithListTimeout sets the overall deadline of ListRunning (tests use a short one). It returns c.
func (c *Client) WithListTimeout(d time.Duration) *Client {
	c.listTimeout = d
	return c
}

// bearer is the token to present on the next request.
func (c *Client) bearer() (string, error) {
	if c.tokenPath == "" {
		return c.token, nil
	}
	b, err := os.ReadFile(c.tokenPath)
	if err != nil {
		return "", fmt.Errorf("read SA token: %w", err)
	}
	return strings.TrimSpace(string(b)), nil
}

// NewInClusterClient reads the mounted ServiceAccount token + CA the SAME way autoscale.go's
// deploymentAPI does, and points at the in-cluster API server (KUBERNETES_SERVICE_HOST/PORT, or the
// conventional in-cluster DNS name). Errors when this process is not running in a pod with a
// mounted ServiceAccount (compose/local dev) — the caller turns that into a named, non-fatal
// Capture.Reason rather than a run failure.
func NewInClusterClient() (*Client, error) {
	const saDir = "/var/run/secrets/kubernetes.io/serviceaccount"
	token, err := os.ReadFile(filepath.Join(saDir, "token"))
	if err != nil {
		return nil, fmt.Errorf("read SA token: %w", err)
	}
	caPEM, err := os.ReadFile(filepath.Join(saDir, "ca.crt"))
	if err != nil {
		return nil, fmt.Errorf("read SA CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("SA CA is not valid PEM")
	}
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" {
		host, port = "kubernetes.default.svc", "443"
	}
	return &Client{
		baseURL: fmt.Sprintf("https://%s:%s", host, port),
		token:   string(token),
		hc: &http.Client{
			Timeout:   15 * time.Second,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}},
		},
	}, nil
}

// Do issues one bearer-authenticated request with an optional body (the fault step's
// scale and patch calls). Same contract as get: a non-2xx status is returned, not an error.
func (c *Client) Do(ctx context.Context, method, path, contentType string, body []byte) ([]byte, int, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rd)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return out, resp.StatusCode, err
}

// get issues one bearer-authenticated GET against an absolute Kubernetes API path (e.g.
// "/api/v1/namespaces/foo/pods") and returns the raw body, the HTTP status code, and any transport
// error. A non-2xx status is NOT an error return — the caller (capture.go) reads 403 as "forbidden"
// and anything else as its own named reason; only a genuine transport failure (DNS, TLS, timeout)
// comes back as err.
func (c *Client) get(ctx context.Context, path string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
}
