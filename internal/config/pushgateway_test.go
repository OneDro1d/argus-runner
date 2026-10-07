package config

import (
	"strings"
	"testing"
	"time"
)

// observability.pushgateway.group_retention.

func pgwBlock(retention string) string {
	return rlBase + "observability:\n  pushgateway:\n    url: http://pgw:9091\n" + retention
}

func TestPushgatewayRetention_DefaultIs15m(t *testing.T) {
	c, err := loadCfgText(t, pgwBlock(""))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := c.PushgatewayGroupRetention(); got != 15*time.Minute {
		t.Errorf("default retention = %s, want 15m", got)
	}
}

func TestPushgatewayRetention_ParsesDeclaredValueAndZero(t *testing.T) {
	for in, want := range map[string]time.Duration{"2h": 2 * time.Hour, "30m": 30 * time.Minute, "0": 0, "0s": 0} {
		c, err := loadCfgText(t, pgwBlock("    group_retention: "+in+"\n"))
		if err != nil {
			t.Fatalf("%s: load: %v", in, err)
		}
		if got := c.PushgatewayGroupRetention(); got != want {
			t.Errorf("group_retention %s read as %s, want %s", in, got, want)
		}
	}
}

func TestPushgatewayRetention_RefusesGarbageNegativeAndOutOfRange(t *testing.T) {
	for _, in := range []string{"soon", "-5m", "10s", "9999h", "1.5.5"} {
		_, err := loadCfgText(t, pgwBlock("    group_retention: "+in+"\n"))
		if err == nil || !strings.Contains(err.Error(), "observability.pushgateway.group_retention") {
			t.Errorf("group_retention %q must be refused by name, got %v", in, err)
		}
	}
}

func TestPushgatewayRetention_UnknownKeyRefusedByName(t *testing.T) {
	_, err := loadCfgText(t, pgwBlock("    group_retenton: 1h\n"))
	if err == nil || !strings.Contains(err.Error(), "group_retenton") {
		t.Fatalf("a mistyped key under observability.pushgateway must be refused by name, got %v", err)
	}
}

func TestPushgatewayRetention_URLStillReads(t *testing.T) {
	c, err := loadCfgText(t, pgwBlock("    group_retention: 1h\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.PushgatewayURL() != "http://pgw:9091" {
		t.Errorf("url = %q", c.PushgatewayURL())
	}
}
