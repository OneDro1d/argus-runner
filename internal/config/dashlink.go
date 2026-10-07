package config

import (
	"fmt"
	"strings"

	"github.com/OneDro1d/argus-runner/internal/dashlink"
)

// DashboardLinkDecl returns the declared observability.dashboard_link template and label, trimmed ("" when
// not declared). The label is returned as declared: the default ("Open dashboard") is applied where it is
// shown, never stored. (, UI-2)
func (c *Config) DashboardLinkDecl() (template, label string) {
	return strings.TrimSpace(c.Observability.DashboardLink.Template), strings.TrimSpace(c.Observability.DashboardLink.Label)
}

// validateDashboardLink checks observability.dashboard_link in full at load time -- load time IS validate
// time, like every block in parseConfig. The block is lenient for an older executor (it ignores the key),
// strict for this one: a template that could carry a credential, or name a placeholder that does not
// exist, is refused by key, never repaired. The refusal does not echo the template.
func (c *Config) validateDashboardLink() error {
	tmpl, label := c.DashboardLinkDecl()
	if tmpl == "" {
		if label != "" {
			return fmt.Errorf("observability.dashboard_link.template is required when observability.dashboard_link.label is declared")
		}
		return nil
	}
	if err := dashlink.Validate(tmpl); err != nil {
		return fmt.Errorf("observability.dashboard_link.template %w", err)
	}
	if err := dashlink.ValidateLabel(label); err != nil {
		return fmt.Errorf("observability.dashboard_link.label %w", err)
	}
	return nil
}
