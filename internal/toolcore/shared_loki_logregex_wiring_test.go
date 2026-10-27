package toolcore

import (
	"strings"
	"testing"
)

// observability.loki.log_regex reaches the executor's Loki client, and from there the generated `| regexp` query.
func TestLokiFor_SharedSourceCarriesTheDeclaredPlainTextRegex(t *testing.T) {
	t.Setenv("ARGUS_SUT_NAMESPACE", "shop-main")
	e := lokiCredEnv(t, `observability:
  loki:
    url: http://loki.observability.svc:3100
    shared_cluster: cluster-a
    log_regex: '^(?P<time>\S+) (?P<service>\S+) (?P<level>[A-Z]+): '
`)
	e.Loki = "http://loki.observability.svc:3100"
	l := lokiFor(e)
	if l.Shared == nil || l.Shared.Regexp != `^(?P<time>\S+) (?P<service>\S+) (?P<level>[A-Z]+): ` {
		t.Fatalf("the declared regex must reach the shared source: %+v", l.Shared)
	}
	q, err := l.SourceQuery(map[string]string{"service": "main-engine", "level": "WARN"}, "", "")
	if err != nil || !strings.Contains(q, ` | regexp "^(?P<time>\\S+) (?P<service>\\S+) (?P<level>[A-Z]+): " | level="WARN"`) {
		t.Fatalf("got %q / %v", q, err)
	}
}
