package config

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// faultallowed.go -- "fault only where it is allowed", the model being load_allowed_targets.
//
//	# argus-config.yaml, TOP LEVEL, beside `targets:` (never under it)
//	fault_allowed_targets:
//	  deployments:
//	    trades: {namespace: calm-demo}     # allowlist name -> the Deployment of that name in that namespace
//	  links:
//	    mcp-to-trades: {toxiproxy_api: "http://toxiproxy.calm-demo:8474", proxy: trades}
//
// ⛔ OFF BY DEFAULT: an absent or empty block, or a name not listed, means no fault step may run.
// ⛔ TOP-LEVEL, never a field of a target, for load_allowed_targets' reason: `targets` is decoded STRICTLY,
// so an executor built before this key existed would REFUSE a new key under it and a rollback would take the
// executor down; a top-level key falls into that executor's lenient inline map and is ignored.
// ⛔ A REFUSAL NAMES THE ENTRY, NEVER THE URL.

// FaultAllow is the top-level `fault_allowed_targets` block.
type FaultAllow struct {
	Deployments map[string]FaultDeployment `yaml:"deployments"`
	Links       map[string]FaultLink       `yaml:"links"`
}

// FaultDeployment allows a `fault` step to stop/start/scale/wait on one Deployment. The allowlist NAME is
// the Deployment's own name: a step cannot reach a deployment under a different name than the one listed.
type FaultDeployment struct {
	Namespace string `yaml:"namespace"`
}

// FaultLink allows a `fault` step to add Argus-owned toxics to, or disable, one Toxiproxy proxy.
type FaultLink struct {
	ToxiproxyAPI string `yaml:"toxiproxy_api"`
	Proxy        string `yaml:"proxy"`
}

var (
	k8sNameRe  = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)
	k8sLabelRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	proxyRe    = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)
)

// FaultEnabled reports whether any fault target is listed.
func (c *Config) FaultEnabled() bool {
	a := c.FaultAllowedTargets
	return a != nil && (len(a.Deployments) > 0 || len(a.Links) > 0)
}

// FaultDeployment resolves an allowlisted deployment name to its namespace.
func (c *Config) FaultDeployment(name string) (namespace string, ok bool) {
	if c == nil || c.FaultAllowedTargets == nil {
		return "", false
	}
	d, ok := c.FaultAllowedTargets.Deployments[name]
	return d.Namespace, ok
}

// FaultLink resolves an allowlisted link name to its Toxiproxy API address and proxy name.
func (c *Config) FaultLink(name string) (api, proxy string, ok bool) {
	if c == nil || c.FaultAllowedTargets == nil {
		return "", "", false
	}
	l, ok := c.FaultAllowedTargets.Links[name]
	return l.ToxiproxyAPI, l.Proxy, ok
}

// validateFaultAllowed is the load-time check (validate-config and onboarding see it). It never echoes a
// URL: a refusal names the entry.
func (c *Config) validateFaultAllowed() error {
	a := c.FaultAllowedTargets
	if a == nil {
		return nil
	}
	var errs []string
	for _, n := range sortedKeys(a.Deployments) {
		where := "fault_allowed_targets.deployments." + n
		d := a.Deployments[n]
		switch {
		case !k8sNameRe.MatchString(n) || len(n) > 253:
			errs = append(errs, where+": the name must be the Deployment's own name (lower-case letters, digits, '-' and '.')")
		case d.Namespace == "":
			errs = append(errs, where+": namespace is required")
		case !k8sLabelRe.MatchString(d.Namespace) || len(d.Namespace) > 63:
			errs = append(errs, where+": namespace is not a valid Kubernetes namespace name")
		case looksSharedOrProd(d.Namespace):
			errs = append(errs, where+": its namespace looks shared/production - a fault may only be applied to a lab or a dedicated test deployment, never a live one")
		}
	}
	for _, n := range sortedKeys(a.Links) {
		where := "fault_allowed_targets.links." + n
		l := a.Links[n]
		if !k8sNameRe.MatchString(n) || len(n) > 253 {
			errs = append(errs, where+": the link name must use lower-case letters, digits, '-' and '.'")
			continue
		}
		if l.ToxiproxyAPI == "" || l.Proxy == "" {
			errs = append(errs, where+": both toxiproxy_api and proxy are required")
			continue
		}
		if !proxyRe.MatchString(l.Proxy) {
			errs = append(errs, where+": proxy is not a valid Toxiproxy proxy name")
		}
		u, err := url.Parse(l.ToxiproxyAPI)
		switch {
		case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "":
			errs = append(errs, where+": toxiproxy_api must be an absolute http(s) address")
		case u.User != nil:
			errs = append(errs, where+": toxiproxy_api must not carry credentials")
		case looksSharedOrProd(l.ToxiproxyAPI):
			errs = append(errs, where+": its toxiproxy_api looks shared/production - a fault may only be applied to a lab or a dedicated test deployment, never a live one")
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("%s", strings.Join(errs, "; "))
}
