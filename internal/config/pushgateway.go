package config

import (
	"fmt"
	"strings"
	"time"
)

// DefaultPushgatewayGroupRetention is how long a finished run's Pushgateway group is kept before the
// executor deletes it. It is well above any normal Prometheus
// scrape interval, so a group is scraped many times before it goes; the history Prometheus already
// holds is untouched by the delete.
const DefaultPushgatewayGroupRetention = 15 * time.Minute

// Bounds for a non-zero group_retention. Below the minimum a group could be deleted before a
// 30-60s scrape sees it; above the maximum the setting is no longer a retention window.
const (
	MinPushgatewayGroupRetention = time.Minute
	MaxPushgatewayGroupRetention = 30 * 24 * time.Hour
)

// PushgatewayObs is observability.pushgateway.
type PushgatewayObs struct {
	// URL is the operator's own Pushgateway (adopt mode); see Config.PushgatewayURL.
	URL string `yaml:"url"`
	// GroupRetention is a Go duration (e.g. 15m, 2h). After a successful push the executor deletes
	// THIS instance's older per-run groups (job=argus, instance=<id>, run_id=<other run>) whose last
	// push is older than it. "0" keeps every group for ever (the behaviour before this key). Absent
	// means DefaultPushgatewayGroupRetention.
	GroupRetention string `yaml:"group_retention"`
}

// validate refuses a garbage, negative or out-of-range group_retention by name. Absent is valid.
func (p *PushgatewayObs) validate() error {
	if p == nil {
		return nil
	}
	_, err := p.parseRetention()
	return err
}

func (p *PushgatewayObs) parseRetention() (time.Duration, error) {
	raw := strings.TrimSpace(p.GroupRetention)
	if raw == "" {
		return DefaultPushgatewayGroupRetention, nil
	}
	if raw == "0" {
		return 0, nil
	}
	d, err := time.ParseDuration(raw)
	switch {
	case err != nil:
		return 0, fmt.Errorf("observability.pushgateway.group_retention: %q is not a duration (e.g. 15m, 2h; 0 keeps groups for ever)", p.GroupRetention)
	case d == 0:
		return 0, nil
	case d < 0:
		return 0, fmt.Errorf("observability.pushgateway.group_retention: %q is negative (use 0 to keep groups for ever)", p.GroupRetention)
	case d < MinPushgatewayGroupRetention:
		return 0, fmt.Errorf("observability.pushgateway.group_retention: %q is below the minimum of %s (a group must outlive a Prometheus scrape; 0 keeps groups for ever)", p.GroupRetention, MinPushgatewayGroupRetention)
	case d > MaxPushgatewayGroupRetention:
		return 0, fmt.Errorf("observability.pushgateway.group_retention: %q exceeds the maximum of %s (use 0 to keep groups for ever)", p.GroupRetention, MaxPushgatewayGroupRetention)
	}
	return d, nil
}

// PushgatewayGroupRetention is the effective retention: the declared value, or the 15m default when
// absent. 0 means keep for ever. A value that failed validate cannot reach here (Load refuses it);
// an unparseable one reads as the default.
func (c *Config) PushgatewayGroupRetention() time.Duration {
	d, err := c.Observability.Pushgateway.parseRetention()
	if err != nil {
		return DefaultPushgatewayGroupRetention
	}
	return d
}
