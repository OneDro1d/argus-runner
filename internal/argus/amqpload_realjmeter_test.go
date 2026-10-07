package argus

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/report"
)

// Defects found by the first REAL-JMeter run of the AMQP Load layer (..51). The fakes of
// amqpload_run_test.go never ran JMeter, the Java sampler or the broker, so none of these could show there.
// Every test here states what the REAL tool does and why a fake could not have seen it.

// ── F-47 · the vhost ──────────────────────────────────────────────────────────────────────────

// javaClientVhost emulates com.rabbitmq.client.ConnectionFactory.setUri, which the Java sampler calls with
// the `mq.amqp.uri` prop (AmqpSessionSetupSampler.factoryFor): an absent path keeps the default vhost "/",
// a path is ONE segment and its percent-decoded remainder after the first "/" is the vhost — so a bare
// trailing "/" is the EMPTY vhost, which no broker has (`530 NOT_ALLOWED - vhost  not found`).
func javaClientVhost(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("java emulation: %v", err)
	}
	rp := u.EscapedPath()
	if rp == "" {
		return "/"
	}
	if strings.Contains(rp[1:], "/") {
		t.Fatalf("java emulation: multiple path segments in %q", rp)
	}
	dec, err := url.PathUnescape(rp[1:])
	if err != nil {
		t.Fatalf("java emulation: %v", err)
	}
	return dec
}

// THE CONTRACT: whatever vhost Go's amqp091-go reads from the configured broker URL (the rest of Argus —
// probes, taps, the guard — connects with it) is the vhost the Java sampler connects to.
func TestAMQPLoad_JavaSamplerSeesTheSameVhostAsAmqp091(t *testing.T) {
	forms := []struct{ name, in, want string }{
		{"no path", "amqp://u:p@broker.invalid:5672", "/"},
		{"trailing slash", "amqp://u:p@broker.invalid:5672/", "/"},
		{"explicit default %2F", "amqp://u:p@broker.invalid:5672/%2F", "/"},
		{"named vhost", "amqp://u:p@broker.invalid:5672/lab", "lab"},
		{"named vhost with an escaped slash", "amqp://u:p@broker.invalid:5672/a%2Fb", "a/b"},
		{"named vhost with an escaped space", "amqp://u:p@broker.invalid:5672/my%20vhost", "my vhost"},
		{"tls scheme, trailing slash", "amqps://u:p@broker.invalid:5671/", "/"},
	}
	for _, f := range forms {
		t.Run(f.name, func(t *testing.T) {
			parsed, err := amqp.ParseURI(f.in)
			if err != nil {
				t.Fatalf("amqp091 cannot parse %q: %v", f.in, err)
			}
			if parsed.Vhost != f.want { // the oracle is pinned, not just computed
				t.Fatalf("amqp091 reads vhost %q for %q, the table says %q", parsed.Vhost, f.in, f.want)
			}
			s := scenarioFromMD(t, amqpLoadMD("AL-001", "2", ""))
			props := amqpLoadBaseProps(s, s.AMQPLoad, &config.MQTarget{URL: f.in}, "tr-20261002T120000000-AL-001-deadbeef")
			got := props["mq.amqp.uri"]
			if v := javaClientVhost(t, got); v != parsed.Vhost {
				t.Errorf("mq.amqp.uri = %q is vhost %q to the Java client; amqp091-go reads %q from the configured URL", got, v, parsed.Vhost)
			}
			if strings.Contains(got, "@") || strings.Contains(got, "u:p") {
				t.Errorf("mq.amqp.uri carries the userinfo: %q", got)
			}
		})
	}
}

// ── minimal JMeter property/function evaluator for the template's argument values ─────────────

var (
	jmP      = regexp.MustCompile(`\$\{__P\(([^,()]*),([^()]*)\)\}`)
	jmThread = regexp.MustCompile(`\$\{__threadNum\}`)
)

// evalJMeter evaluates ONLY the two functions whose behaviour is certain on real JMeter: ${__P(name,default)}
// and ${__threadNum}. Anything else left in the string is an error — notably a nested ${__jexl3(...)} whose
// value on real JMeter was the EMPTY string (F-49), which a test that "assumed" it computes would not catch.
func evalJMeter(expr string, props map[string]string, thread int) (string, error) {
	out := jmThread.ReplaceAllString(expr, fmt.Sprint(thread))
	out = jmP.ReplaceAllStringFunc(out, func(m string) string {
		g := jmP.FindStringSubmatch(m)
		if v, ok := props[g[1]]; ok {
			return v
		}
		return g[2]
	})
	if strings.Contains(out, "${") {
		return "", fmt.Errorf("%q still holds a function this test cannot prove (only __P and __threadNum are evaluated): %s", expr, out)
	}
	return out, nil
}

type jmxSampler struct {
	class string
	args  map[string]string
}

var (
	jmSamplerBlock = regexp.MustCompile(`(?s)<JavaSampler .*?</JavaSampler>`)
	jmArg          = regexp.MustCompile(`<stringProp name="Argument.name">(.*?)</stringProp><stringProp name="Argument.value">(.*?)</stringProp>`)
	jmClass        = regexp.MustCompile(`<stringProp name="classname">(.*?)</stringProp>`)
)

func amqpLoadSamplers(t *testing.T) map[string]jmxSampler {
	t.Helper()
	jmx := readTemplate(t, "amqp-load.jmx")
	out := map[string]jmxSampler{}
	for _, blk := range jmSamplerBlock.FindAllString(jmx, -1) {
		cls := jmClass.FindStringSubmatch(blk)[1]
		short := cls[strings.LastIndex(cls, ".")+1:]
		s := jmxSampler{class: cls, args: map[string]string{}}
		for _, m := range jmArg.FindAllStringSubmatch(blk, -1) {
			s.args[m[1]] = m[2]
		}
		out[short] = s
	}
	for _, want := range []string{"AmqpSessionSetupSampler", "AmqpPublishSampler", "AmqpSubscribeSampler"} {
		if _, ok := out[want]; !ok {
			t.Fatalf("amqp-load.jmx has no %s sampler (found %v)", want, keysOf(out))
		}
	}
	return out
}

func keysOf(m map[string]jmxSampler) []string {
	var k []string
	for n := range m {
		k = append(k, n)
	}
	sort.Strings(k)
	return k
}

// ── F-49 · the per-session suffix ─────────────────────────────────────────────────────────────

// Every session of a step must get its OWN queue and routing key, and the three samplers of one session
// must agree on them: the setup sampler declares the queue and binds the key, the publish sampler sends
// to the key, the subscribe sampler finds the session by its id.
func TestAMQPLoadTemplate_EverySessionHasItsOwnQueueAndKey_AgreedAcrossSamplers(t *testing.T) {
	sm := amqpLoadSamplers(t)
	setup, pub, sub := sm["AmqpSessionSetupSampler"], sm["AmqpPublishSampler"], sm["AmqpSubscribeSampler"]
	props := map[string]string{"amqp.run": "20261002T120000000", "amqp.step": "2"}
	const sessions = 25
	queues, keys := map[string]int{}, map[string]int{}
	for th := 1; th <= sessions; th++ {
		ev := func(s jmxSampler, arg string) string {
			t.Helper()
			raw, ok := s.args[arg]
			if !ok {
				t.Fatalf("%s passes no %q argument", s.class, arg)
			}
			v, err := evalJMeter(raw, props, th)
			if err != nil {
				t.Fatalf("%s.%s: %v", s.class, arg, err)
			}
			return v
		}
		q, rk := ev(setup, "queue"), ev(setup, "routing_key")
		if strings.HasSuffix(q, "-") || strings.HasSuffix(q, ".") || strings.HasSuffix(rk, ".") || strings.HasSuffix(rk, "-") {
			t.Errorf("thread %d: queue %q / routing key %q end in an empty suffix", th, q, rk)
		}
		if got := ev(pub, "routing_key"); got != rk {
			t.Errorf("thread %d: publish routes to %q but the setup bound %q", th, got, rk)
		}
		sess := ev(setup, "session")
		if ev(pub, "session") != sess || ev(sub, "session") != sess {
			t.Errorf("thread %d: the three samplers disagree on the session id", th)
		}
		queues[q]++
		keys[rk]++
	}
	if len(queues) != sessions {
		t.Errorf("%d sessions share %d distinct queue names: %v", sessions, len(queues), queues)
	}
	if len(keys) != sessions {
		t.Errorf("%d sessions share %d distinct routing keys: %v", sessions, len(keys), keys)
	}
}

// ── F-51 · queue names across runs ────────────────────────────────────────────────────────────

// Two runs with different run ids declare queues and bind keys with no name in common, in every step, so
// one run's x-expires reaper, purge or leftover can never touch another's.
func TestAMQPLoadTemplate_TwoRunsGetDisjointQueueNamesAndKeys(t *testing.T) {
	setup := amqpLoadSamplers(t)["AmqpSessionSetupSampler"]
	names := func(run string) (qs, ks map[string]bool) {
		qs, ks = map[string]bool{}, map[string]bool{}
		for step := 1; step <= 4; step++ {
			for th := 1; th <= 12; th++ {
				props := map[string]string{"amqp.run": run, "amqp.step": fmt.Sprint(step)}
				q, err := evalJMeter(setup.args["queue"], props, th)
				if err != nil {
					t.Fatal(err)
				}
				k, err := evalJMeter(setup.args["routing_key"], props, th)
				if err != nil {
					t.Fatal(err)
				}
				qs[q], ks[k] = true, true
			}
		}
		return
	}
	qa, ka := names("20261002T120000000")
	qb, kb := names("20261002T120000001")
	if len(qa) != 48 || len(qb) != 48 {
		t.Fatalf("a run's queue names are not all distinct: %d and %d of 48", len(qa), len(qb))
	}
	for q := range qa {
		if qb[q] {
			t.Errorf("queue %q is used by both runs", q)
		}
	}
	for k := range ka {
		if kb[k] {
			t.Errorf("routing key %q is used by both runs", k)
		}
	}
}

// The run id that keeps queue names apart comes from the correlation id. Two RunAll calls with different
// run ids must hand the template different, non-empty `amqp.run` values.
func TestAMQPLoad_DifferentRunIdsReachTheTemplateAsDifferentAmqpRun(t *testing.T) {
	seen := map[string]bool{}
	for _, run := range []string{"20261002T120000000", "20261002T120000001"} {
		e := newLoadEnv(t)
		writeAMQPLoad(t, filepath.Join(e.dir, "scenarios"), "AL-001", "2", "")
		f := &loadFake{gen: func(n int, p map[string]string) string { return healthyJTL("AL-001", p) }}
		c := loadCfg(t, loadConfig(t, allowLab))
		if _, err := RunAll(c, filepath.Join(e.dir, "scenarios"), e.results, "lab", "", "", "", run, f); err != nil {
			t.Fatal(err)
		}
		if len(f.calls) != 1 {
			t.Fatalf("runner calls = %d", len(f.calls))
		}
		got := f.calls[0].props["amqp.run"]
		if got != run {
			t.Errorf("amqp.run = %q, want the run id %q", got, run)
		}
		seen[got] = true
	}
	if len(seen) != 2 {
		t.Errorf("two runs produced %d distinct amqp.run values", len(seen))
	}
}

// An EMPTY run id would make every queue name `argus-load--s1-N`, shared by every run that ever had one.
// It is refused at the last door, before anything dials the broker. runIDFromCorr("tr--AL-001-x") used to
// return "-AL-001-x" (non-empty, nonsense): the corr-id shapes with no real run id are all refused.
func TestAMQPLoad_EmptyRunIdIsRefusedBeforeFiring(t *testing.T) {
	for _, corr := range []string{"tr--AL-001-deadbeef", "tr-", "", "tr-20261002T120000000"} {
		t.Run(fmt.Sprintf("corr=%q", corr), func(t *testing.T) {
			e := newLoadEnv(t)
			writeAMQPLoad(t, filepath.Join(e.dir, "scenarios"), "AL-001", "2", "")
			f := &loadFake{gen: func(n int, p map[string]string) string { return healthyJTL("AL-001", p) }}
			c := loadCfg(t, loadConfig(t, allowLab))
			sf := scenarioFile{s: parseFile(t, e.dir, "AL-001")}
			res := runOneScenario(c, sf, corr, e.results, f, nil, nil, "", nil)
			if len(f.calls) != 0 {
				t.Fatalf("the runner (the dial) was called %d time(s) with run id of corr %q", len(f.calls), corr)
			}
			if e.envCalls != 0 || len(e.sleeps) != 0 {
				t.Errorf("a refused scenario touched the environment (%d) or slept (%d)", e.envCalls, len(e.sleeps))
			}
			if res.Status != report.StatusError || res.Failure == nil || !strings.Contains(res.Failure.Observed, "run id") {
				t.Errorf("status=%s failure=%+v, want an error naming the run id", res.Status, res.Failure)
			}
			for _, leak := range []string{"load-lab.invalid", "amqp://", loadSecret, "loaduser"} {
				if res.Failure != nil && strings.Contains(res.Failure.Observed, leak) {
					t.Errorf("refusal leaks %q", leak)
				}
			}
		})
	}
}

// ── minor · the username is a credential ──────────────────────────────────────────────────────

func TestAMQPLoad_UsernameNeverTravelsAsJ(t *testing.T) {
	r := &LocalJMeterRunner{TemplatesDir: "/templates"}
	s := scenarioFromMD(t, amqpLoadMD("AL-001", "2", ""))
	props := amqpLoadBaseProps(s, s.AMQPLoad, &config.MQTarget{URL: "amqp://loaduser:" + loadSecret + "@broker.invalid:5672/"}, "tr-20261002T120000000-AL-001-deadbeef")
	if props["mq.amqp.username"] != "loaduser" {
		t.Fatalf("control: the username is not in the props: %q", props["mq.amqp.username"])
	}
	argv := strings.Join(r.buildArgs("amqp-load", "/results/x.jtl", "/tmp/argus-run-1.properties", props), " ")
	for _, leak := range []string{"loaduser", loadSecret} {
		if strings.Contains(argv, leak) {
			t.Errorf("%q is on the jmeter command line (visible in ps): %s", leak, argv)
		}
	}
	_, secret := splitSecretProps(props)
	if secret["mq.amqp.username"] != "loaduser" {
		t.Errorf("the username must ride the private properties file, secret set = %v", keysOfStr(secret))
	}
}

func keysOfStr(m map[string]string) []string {
	var k []string
	for n := range m {
		k = append(k, n)
	}
	sort.Strings(k)
	return k
}

// ── F-48 · the scheduler, on EVERY template ───────────────────────────────────────────────────

var jmxMainSchedulerLine = regexp.MustCompile(`<(boolProp|stringProp) name="ThreadGroup.scheduler">([^<]*)</`)

// JMeter does not evaluate a function inside a <boolProp>: it parses the text as a boolean, which is false.
// So `<boolProp name="ThreadGroup.scheduler">${__P(load.scheduler,false)}</boolProp>` switched the scheduler
// off on every template, and a `## LOAD` duration was ignored. A <stringProp> is function-evaluated and read
// back as a boolean by the thread group.
func TestEveryTemplateCanSwitchTheSchedulerOn(t *testing.T) {
	files, err := filepath.Glob(filepath.FromSlash("../../templates/*.jmx"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no templates found: %v", err)
	}
	for _, f := range files {
		name := filepath.Base(f)
		t.Run(name, func(t *testing.T) {
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range jmxMainSchedulerLine.FindAllStringSubmatch(string(b), -1) {
				if m[1] == "boolProp" && strings.Contains(m[2], "${") {
					t.Errorf("%s: <boolProp name=\"ThreadGroup.scheduler\">%s</boolProp> — JMeter reads a function in a boolProp as false, so a LOAD duration is ignored; use a stringProp", name, m[2])
				}
			}
			if strings.Contains(string(b), `name="ThreadGroup.scheduler">${__P(load.scheduler,false)}<`) &&
				!strings.Contains(string(b), `<stringProp name="ThreadGroup.scheduler">${__P(load.scheduler,false)}</stringProp>`) {
				t.Errorf("%s: the scheduler property is not a stringProp", name)
			}
		})
	}
}

// A LOAD duration only SUSTAINS the load if the loop does not end first: with loops=1 the thread leaves
// after one pass whatever the scheduler says. DeriveProps therefore asks for loops=-1 together with the
// duration, and says nothing otherwise, so a scenario without LOAD (or without a Duration) still runs once.
func TestDeriveProps_LoadDurationAsksForLoopsUntilTheSchedulerStops(t *testing.T) {
	c := &config.Config{}
	c.Targets.HTTP = &config.HTTPTarget{BaseURL: "http://sut.invalid"}

	withDur := minimalLoadScenario(t, "- **Users**: 2\n- **Ramp Seconds**: 0\n- **Duration Seconds**: 15\n- **Target P95 Ms**: 300\n- **Max Error Rate**: 0.05")
	p, err := DeriveProps(c, withDur, "tr-t")
	if err != nil {
		t.Fatal(err)
	}
	if p["load.scheduler"] != "true" || p["load.duration"] != "15" || p["load.loops"] != "-1" {
		t.Errorf("a LOAD with a duration: scheduler=%q duration=%q loops=%q, want true/15/-1", p["load.scheduler"], p["load.duration"], p["load.loops"])
	}

	noDur := minimalLoadScenario(t, "- **Users**: 2\n- **Ramp Seconds**: 0\n- **Duration Seconds**: 0\n- **Target P95 Ms**: 300\n- **Max Error Rate**: 0.05")
	if noDur.Load == nil || noDur.Load.DurationSeconds != 0 {
		t.Fatalf("fixture must carry a Load without a duration: %+v", noDur.Load)
	}
	p, _ = DeriveProps(c, noDur, "tr-t")
	if _, ok := p["load.loops"]; ok {
		t.Errorf("a LOAD without a duration must keep one pass per thread, got load.loops=%q", p["load.loops"])
	}
	p, _ = DeriveProps(c, minimalLoadScenario(t, ""), "tr-t")
	if _, ok := p["load.loops"]; ok {
		t.Errorf("a scenario without LOAD must emit no load.loops, got %q", p["load.loops"])
	}
}

// Every main thread group reads load.loops with default 1, so an unprofiled scenario still runs once.
func TestLoadDrivenTemplatesReadLoopsWithDefaultOne(t *testing.T) {
	for _, name := range loadDrivenTemplates {
		jmx := readTemplate(t, name)
		if !strings.Contains(jmx, `<stringProp name="LoopController.loops">${__P(load.loops,1)}</stringProp>`) {
			t.Errorf("%s: the main thread group does not read ${__P(load.loops,1)} — a LOAD duration cannot sustain the load", name)
		}
	}
}
