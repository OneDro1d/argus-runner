package argus

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A load user is granted least privilege: RabbitMQ permissions are regexes over resource names, and an
// operator grants a load login configure/write/read on ONE name pattern (the msgbus lab grants `^perf\..*`).
// Argus's queue names were hard-coded `argus-load-<run>-s<step>-<n>`, so every session on such a broker was
// refused at setup (403 ACCESS_REFUSED, msgbus-homelab run 20261003T014730592). The exchange was already
// the target's `exchanges.load`; the queue prefix is now the target's `queues.load`, with the old name as
// the default.

func TestAMQPLoad_QueuePrefixDefaultsToArgusLoad(t *testing.T) {
	e := newLoadEnv(t)
	writeAMQPLoad(t, filepath.Join(e.dir, "scenarios"), "AL-001", "2", "")
	f := &loadFake{gen: func(n int, p map[string]string) string { return healthyJTL("AL-001", p) }}
	e.run(t, loadConfig(t, allowLab), f)
	if len(f.calls) == 0 {
		t.Fatal("the runner was never called")
	}
	if got := f.calls[0].props["amqp.queue_prefix"]; got != "argus-load" {
		t.Errorf("props[amqp.queue_prefix] = %q, want the historical default %q", got, "argus-load")
	}
}

func TestAMQPLoad_QueuePrefixIsTheTargetsQueuesLoad(t *testing.T) {
	e := newLoadEnv(t)
	writeAMQPLoad(t, filepath.Join(e.dir, "scenarios"), "AL-001", "2", "")
	cfg := strings.Replace(loadConfig(t, allowLab), "      exchanges:\n        load: argus.load\n",
		"      exchanges:\n        load: perf.x\n      queues:\n        load: perf.argus\n", 1)
	if !strings.Contains(cfg, "load: perf.argus") {
		t.Fatal("fixture drifted: could not add queues.load")
	}
	f := &loadFake{gen: func(n int, p map[string]string) string { return healthyJTL("AL-001", p) }}
	e.run(t, cfg, f)
	if len(f.calls) == 0 {
		t.Fatal("the runner was never called")
	}
	p := f.calls[0].props
	if p["amqp.queue_prefix"] != "perf.argus" {
		t.Errorf("props[amqp.queue_prefix] = %q, want the target's queues.load %q", p["amqp.queue_prefix"], "perf.argus")
	}
	if p["amqp.exchange"] != "perf.x" {
		t.Errorf("props[amqp.exchange] = %q, want the target's exchanges.load %q", p["amqp.exchange"], "perf.x")
	}
}

// The template must READ the prop, or the config key reaches nothing (the V28-006 silent drop).
func TestAMQPLoadTemplate_QueueNameIsBuiltFromThePrefix(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "templates", "amqp-load.jmx"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "${__P(amqp.queue_prefix,argus-load)}-${__P(amqp.run,)}-s${__P(amqp.step,1)}-${__threadNum}") {
		t.Error("templates/amqp-load.jmx does not build the session queue name from amqp.queue_prefix")
	}
}
