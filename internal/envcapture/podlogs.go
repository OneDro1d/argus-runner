package envcapture

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

// podlogs.go — what `argus collect-logs` needs from the Kubernetes API beyond the one-shot reads in
// client.go: a pod list that tells a 403 from every other failure, and a pod-log FOLLOW stream.

// runningFrom is THE rule for "a live regular container": a pod that is Running and not terminating,
// and each of its regular containers (init and ephemeral containers are not listed). One function so
// the image measurement and the log collector cannot disagree about what is running.
func runningFrom(list livePodList) []RunningContainer {
	var out []RunningContainer
	for _, p := range list.Items {
		if p.Status.Phase != "Running" || p.Metadata.DeletionTimestamp != "" {
			continue
		}
		for _, c := range p.Status.ContainerStatuses {
			out = append(out, RunningContainer{Pod: p.Metadata.Name, Container: c.Name, ImageID: c.ImageID})
		}
	}
	return out
}

// StatusError is a Kubernetes API answer that was not 2xx. It carries the status only: the body of a
// log request can describe the container, and nothing here may carry a log line or a credential.
type StatusError struct {
	Status int
	What   string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("k8s API %s returned status %d", e.What, e.Status)
}

// Forbidden reports a 403: the RBAC grant is missing.
func (e *StatusError) Forbidden() bool { return e.Status == http.StatusForbidden }

// NewFileTokenClient is a Client whose bearer token is re-read from tokenPath for every request made
// through ListRunning and FollowPodLog. hc is used as given (tests pass an httptest client).
func NewFileTokenClient(baseURL, tokenPath string, hc *http.Client) *Client {
	return &Client{baseURL: baseURL, tokenPath: tokenPath, hc: hc}
}

// NewInClusterStreamClient is the in-cluster client for a LONG-LIVED reader: the same API server and CA
// as NewInClusterClient, but NO whole-request timeout (a follow stream is open for hours; dial, TLS
// and response-header waits are bounded instead) and the ServiceAccount token re-read per request.
func NewInClusterStreamClient() (*Client, error) {
	return NewStreamClientFromDir(DefaultServiceAccountDir)
}

// DefaultServiceAccountDir is where a pod's ServiceAccount token and CA are mounted.
const DefaultServiceAccountDir = "/var/run/secrets/kubernetes.io/serviceaccount"

// NewStreamClientFromDir is NewInClusterStreamClient reading the token and CA from saDir (tests pass a
// directory that does or does not exist). It makes no network call.
func NewStreamClientFromDir(saDir string) (*Client, error) {
	tokenPath := filepath.Join(saDir, "token")
	if _, err := os.ReadFile(tokenPath); err != nil {
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
		baseURL:   fmt.Sprintf("https://%s", net.JoinHostPort(host, port)),
		tokenPath: tokenPath,
		hc: &http.Client{
			Transport: &http.Transport{
				TLSClientConfig:       &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
				DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
				TLSHandshakeTimeout:   10 * time.Second,
				ResponseHeaderTimeout: 30 * time.Second,
			},
		},
	}, nil
}

// ListRunning lists the pods of ns and returns the live regular containers (runningFrom). An answer that
// is not 200 is a *StatusError; an empty namespace is not an error, it is an empty list.
func (c *Client) ListRunning(ctx context.Context, ns string) ([]RunningContainer, error) {
	tok, err := c.bearer()
	if err != nil {
		return nil, err
	}
	what := "GET pods in namespace " + ns
	// The list gets its OWN overall deadline (headers and body): the stream client has none, and a body
	// that stalls after its headers would otherwise stall the poll loop until TCP keepalive fires. Only
	// this request: the follow streams keep no overall timeout.
	d := c.listTimeout
	if d <= 0 {
		d = DefaultListTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/v1/namespaces/"+url.PathEscape(ns)+"/pods", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, urlErrorWithoutURL(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &StatusError{Status: resp.StatusCode, What: what}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	var list livePodList
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("pods response: %w", err)
	}
	return runningFrom(list), nil
}

// FollowPodLog opens GET .../pods/<pod>/log?container=<c>&follow=true&timestamps=true&sinceTime=<since>
// and returns the open body: each line is "<RFC3339Nano> <line>". The caller closes it (or cancels ctx).
// The token is read afresh for this request. A non-200 answer is a *StatusError.
func (c *Client) FollowPodLog(ctx context.Context, ns, pod, container string, since time.Time) (io.ReadCloser, error) {
	tok, err := c.bearer()
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	q.Set("container", container)
	q.Set("follow", "true")
	q.Set("timestamps", "true")
	q.Set("sinceTime", since.UTC().Format(time.RFC3339Nano))
	u := c.baseURL + "/api/v1/namespaces/" + url.PathEscape(ns) + "/pods/" + url.PathEscape(pod) + "/log?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	// No Accept: a log answer is text/plain, not JSON.
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, urlErrorWithoutURL(err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, &StatusError{Status: resp.StatusCode, What: "GET pod log in namespace " + ns}
	}
	return resp.Body, nil
}

// urlErrorWithoutURL drops the request URL from a transport error: the URL names a pod and a container,
// and an error is printed.
func urlErrorWithoutURL(err error) error {
	if ue, ok := err.(*url.Error); ok {
		return fmt.Errorf("%s: %w", ue.Op, ue.Err)
	}
	return err
}
