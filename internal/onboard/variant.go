package onboard

import (
	"strings"

	"github.com/OneDro1d/argus-runner/internal/config"
)

// Execution-plane image variants (D-EXEC.2 point 1 / plan §3 item 11, owner-approved
// 2026-07-10). slim is the DEFAULT and runs 100% of the canonical the operator layer set:
// argus + templates + JMeter core (HTTP) + the AMQP sampler (RabbitMQ) + the
// PostgreSQL JDBC driver + node/npm/Playwright (web UI) + the runner-native mcp-call
// path. full = slim ∪ the comprehensified plugin surface (Kafka, MQTT, gRPC, Selenium,
// all non-PostgreSQL JDBC drivers) for SUTs that deviate from the canonical stack.
const (
	VariantSlim = "slim"
	VariantFull = "full"
)

// SelectImageVariant picks a SUT's execution-plane image TRANSPARENTLY from its
// argus-config targets — the user never chooses (UC164). slim is returned unless a
// declared target needs a plugin that only full carries. The reason string explains the
// choice (for onboarding output / logs).
//
// Guarantee (plan §3 item 11): for every canonical the operator SUT — RabbitMQ + PostgreSQL +
// HTTP + MCP + web UI — slim runs ALL scenarios on ALL tiers. Today the only
// non-canonical signal the argus-config schema can express is a non-PostgreSQL JDBC
// driver in targets.database.jdbc_url (the message broker is generic AMQP/RabbitMQ; HTTP,
// MCP and the Playwright web UI are all canonical). When the schema later grows explicit
// Kafka / MQTT / gRPC / Selenium targets, add them to the checks below.
func SelectImageVariant(cfg *config.Config) (variant, reason string) {
	if cfg == nil {
		return VariantSlim, "no config — slim (the default) covers the canonical layer set"
	}
	if db := cfg.Targets.Database; db != nil && db.JDBCURL != "" {
		if drv := jdbcDriver(db.JDBCURL); drv != "" && !isPostgresDriver(drv) {
			return VariantFull, "database driver " + drv + " is not PostgreSQL — slim ships the PostgreSQL JDBC driver only"
		}
	}
	return VariantSlim, "all declared targets are covered by the canonical (slim) layer set"
}

// jdbcDriver extracts the driver token from a jdbc_url — e.g. "postgresql" from
// "jdbc:postgresql://host:5432/db", "oracle" from "jdbc:oracle:thin:@host:1521:sid".
// Returns "" when the value is not a jdbc: URL.
func jdbcDriver(jdbcURL string) string {
	s := strings.ToLower(strings.TrimSpace(jdbcURL))
	if !strings.HasPrefix(s, "jdbc:") {
		return ""
	}
	rest := s[len("jdbc:"):]
	if i := strings.IndexByte(rest, ':'); i >= 0 {
		rest = rest[:i] // the token before the next ':' (subprotocol or "://")
	}
	return strings.TrimSpace(rest)
}

// isPostgresDriver reports whether a JDBC driver token is PostgreSQL — the one DB driver
// slim ships. "postgresql" is canonical; "postgres" is accepted as an alias.
func isPostgresDriver(driver string) bool {
	return driver == "postgresql" || driver == "postgres"
}
