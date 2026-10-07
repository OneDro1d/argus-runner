package onboard

import (
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/OneDro1d/argus-runner/internal/config"
)

func TestJDBCDriver(t *testing.T) {
	cases := map[string]string{
		"jdbc:postgresql://db:5432/orders": "postgresql",
		"jdbc:mysql://db:3306/app":         "mysql",
		"jdbc:oracle:thin:@host:1521:ORCL": "oracle",
		"jdbc:sqlserver://host:1433;db=x":  "sqlserver",
		"JDBC:PostgreSQL://DB:5432/x":      "postgresql", // case-insensitive
		"postgres://db:5432/x":             "",           // not a jdbc: URL
		"":                                 "",
	}
	for in, want := range cases {
		if got := jdbcDriver(in); got != want {
			t.Errorf("jdbcDriver(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSelectImageVariant_pureRules(t *testing.T) {
	// nil / no DB -> slim
	if v, _ := SelectImageVariant(nil); v != VariantSlim {
		t.Errorf("nil cfg: got %q, want slim", v)
	}

	cases := []struct {
		name string
		yaml string
		want string
	}{
		{"no targets at all", "project:\n  name: x\n", VariantSlim},
		{"http only", "targets:\n  http:\n    base_url: http://sut:8080\n", VariantSlim},
		{"mcp only", "targets:\n  mcp:\n    base_url: http://sut:9000\n    transport: streamable-http\n", VariantSlim},
		{"rabbitmq broker", "targets:\n  message_broker:\n    url: amqp://rabbit:5672\n", VariantSlim},
		{"postgres db", "targets:\n  database:\n    jdbc_url: jdbc:postgresql://db:5432/orders\n", VariantSlim},
		{"canonical full stack", "targets:\n  http:\n    base_url: http://sut:8080\n  message_broker:\n    url: amqp://rabbit:5672\n  database:\n    jdbc_url: jdbc:postgresql://db:5432/orders\n", VariantSlim},
		{"mysql db -> full", "targets:\n  database:\n    jdbc_url: jdbc:mysql://db:3306/app\n", VariantFull},
		{"oracle db -> full", "targets:\n  database:\n    jdbc_url: jdbc:oracle:thin:@h:1521:ORCL\n", VariantFull},
	}
	for _, c := range cases {
		var cfg config.Config
		if err := yaml.Unmarshal([]byte(c.yaml), &cfg); err != nil {
			t.Fatalf("%s: yaml: %v", c.name, err)
		}
		got, reason := SelectImageVariant(&cfg)
		if got != c.want {
			t.Errorf("%s: got %q (%s), want %q", c.name, got, reason, c.want)
		}
	}
}
