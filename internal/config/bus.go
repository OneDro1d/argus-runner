package config

import (
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// THE BROKER TARGET'S DECLARATIONS FOR THE "bus" CHAIN STEP: `envelope` (where the run's correlation id
// travels in the messages on this broker), `streams` (named bindings a scenario author should not hard-code) and `vhost`
// (a non-secret check that the URL names the vhost the operator meant). All three are per instance, none holds a
// credential, and there is NO default: a bus step against a target without `envelope.correlation` is a preflight error.

// BusCorrelationSource is ONE place a message may carry the run's correlation id. Exactly one field is set.
type BusCorrelationSource struct {
	Property string `yaml:"property"` // an AMQP basic property: correlation_id | message_id | type | app_id | reply_to
	Header   string `yaml:"header"`   // a header name (matched case-insensitively)
	Body     string `yaml:"body"`     // a dotted path into the decoded JSON body
}

// BusEnvelope declares how the messages on a broker carry the run's correlation id.
type BusEnvelope struct {
	// Correlation lists the sources in order; a message is correlated when ANY of them holds the id (equal, or starting
	// with "<id>.").
	Correlation []BusCorrelationSource `yaml:"correlation"`
	// ListPayload: the body is a JSON list and each object element is a record (GoKit).
	ListPayload bool `yaml:"list_payload"`
}

// BusStream is a named binding, declared once for the instance.
type BusStream struct {
	Exchange   string `yaml:"exchange"`
	RoutingKey string `yaml:"routing_key"`
}

var (
	busURLEnvRe     = regexp.MustCompile(`^\$\{([A-Za-z_][A-Za-z0-9_]*)\}$`)
	busStreamNameRe = regexp.MustCompile(`^[A-Za-z0-9_.\-]+$`)
	busPathRe       = regexp.MustCompile(`^[A-Za-z0-9_.\-]+$`)
	busProperties   = map[string]bool{"correlation_id": true, "message_id": true, "type": true, "app_id": true, "reply_to": true}
)

// BusURLEnv is the NAME of the environment variable the target's `url` is a reference to - ok only when the whole field
// is one `${NAME}`. It is read from the declaration, before any value was filled in, so the URL never passes through code.
func (t *MQTarget) BusURLEnv() (string, bool) {
	if t == nil || t.URLEnvName == "" {
		return "", false
	}
	return t.URLEnvName, true
}

// URLVhost reads the vhost out of the (resolved) URL: "/" for an empty path. ok is false when the URL cannot be read. The
// URL itself is never returned or printed.
func (t *MQTarget) URLVhost() (string, bool) {
	if t == nil {
		return "", false
	}
	u, err := url.Parse(t.URL)
	if err != nil || u.Host == "" {
		return "", false
	}
	p := strings.TrimPrefix(u.EscapedPath(), "/")
	if p == "" {
		return "/", true
	}
	v, err := url.PathUnescape(p)
	if err != nil {
		return "", false
	}
	return v, true
}

// BusPreflightProblem is the environment check of a bus step against its target: a correlation source is declared, the
// url is a whole ${NAME}, and a declared vhost is the URL's. It never prints the URL. "" when fine.
func (t *MQTarget) BusPreflightProblem(label string) string {
	if t == nil {
		return "no such broker target"
	}
	if t.Envelope == nil || len(t.Envelope.Correlation) == 0 {
		return fmt.Sprintf("%s declares no envelope.correlation - a bus step needs to know where this run's correlation id travels in the "+
			"messages (property, header or body path); there is no default", label)
	}
	if _, ok := t.BusURLEnv(); !ok {
		return fmt.Sprintf("%s.url must be one whole ${NAME} reference (the bus step dials with the variable's NAME, so the URL never passes "+
			"through code)", label)
	}
	if t.Vhost != "" {
		got, ok := t.URLVhost()
		if !ok {
			return fmt.Sprintf("%s.vhost is declared, but the URL could not be read to compare it", label)
		}
		if got != t.Vhost {
			return fmt.Sprintf("%s.vhost is %q but the URL names another vhost (%q) - one of them is wrong", label, t.Vhost, got)
		}
	}
	return ""
}

// BusStreamNamed resolves a stream declared on the target; the refusal lists the declared names.
func (t *MQTarget) BusStreamNamed(label, name string) (BusStream, error) {
	if t != nil {
		if s, ok := t.Streams[name]; ok {
			return s, nil
		}
	}
	var declared []string
	if t != nil {
		for n := range t.Streams {
			declared = append(declared, n)
		}
	}
	sort.Strings(declared)
	if len(declared) == 0 {
		return BusStream{}, fmt.Errorf("unknown stream %q - %s declares no streams", name, label)
	}
	return BusStream{}, fmt.Errorf("unknown stream %q - %s declares: %s", name, label, strings.Join(declared, ", "))
}

// BusTargetFor resolves a bus step's `target`: the named entry of targets.message_broker_targets, or the plain
// targets.message_broker when none is named. label names it for a sentence.
func (c *Config) BusTargetFor(name string) (t *MQTarget, label string, err error) {
	if strings.TrimSpace(name) != "" {
		t, err = c.MQTargetNamed(name)
		return t, "targets.message_broker_targets." + name, err
	}
	if c.Targets.MessageBroker == nil {
		return nil, "targets.message_broker", fmt.Errorf("argus-config.yaml declares no targets.message_broker - declare it, or name one under " +
			"targets.message_broker_targets with `target`")
	}
	return c.Targets.MessageBroker, "targets.message_broker", nil
}

// setBusDerived records, per broker target, the NAME its `url` references (before expandEnv fills the value in).
func (c *Config) setBusDerived() {
	each := func(t *MQTarget) {
		if t == nil {
			return
		}
		if m := busURLEnvRe.FindStringSubmatch(strings.TrimSpace(t.URL)); m != nil {
			t.URLEnvName = m[1]
		}
	}
	each(c.Targets.MessageBroker)
	for _, t := range c.Targets.MessageBrokerTargets {
		each(t)
	}
}

// validateBusDeclarations checks every broker target's envelope, streams and vhost. Load time IS validate time.
func (c *Config) validateBusDeclarations() error {
	var problems []string
	check := func(t *MQTarget, at string) {
		if t == nil {
			return
		}
		if strings.Contains(t.Vhost, "${") {
			problems = append(problems, at+".vhost carries ${…} - a vhost is a literal name, not a credential")
		}
		if e := t.Envelope; e != nil {
			for i, s := range e.Correlation {
				set := 0
				for _, v := range []string{s.Property, s.Header, s.Body} {
					if v != "" {
						set++
					}
				}
				switch {
				case set != 1:
					problems = append(problems, fmt.Sprintf("%s.envelope.correlation[%d] must set exactly one of property, header, body", at, i))
				case s.Property != "" && !busProperties[strings.ToLower(s.Property)]:
					problems = append(problems, fmt.Sprintf("%s.envelope.correlation[%d].property %q is not one of correlation_id, message_id, type, app_id, reply_to", at, i, s.Property))
				case s.Header != "" && strings.ContainsAny(s.Header, " \t${}"):
					problems = append(problems, fmt.Sprintf("%s.envelope.correlation[%d].header %q is not a header name", at, i, s.Header))
				case s.Body != "" && !busPathRe.MatchString(s.Body):
					problems = append(problems, fmt.Sprintf("%s.envelope.correlation[%d].body %q is not a dotted body path", at, i, s.Body))
				}
			}
		}
		names := make([]string, 0, len(t.Streams))
		for n := range t.Streams {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			s := t.Streams[n]
			switch {
			case !busStreamNameRe.MatchString(n):
				problems = append(problems, fmt.Sprintf("%s.streams: %q is not a stream name (letters, digits, `.`, `_`, `-`)", at, n))
			case s.Exchange == "" || s.RoutingKey == "":
				problems = append(problems, fmt.Sprintf("%s.streams.%s needs both an exchange and a routing_key", at, n))
			case strings.Contains(s.Exchange, "${") || strings.Contains(s.RoutingKey, "${"):
				problems = append(problems, fmt.Sprintf("%s.streams.%s carries ${…} - a binding is a literal", at, n))
			case strings.Trim(s.RoutingKey, "#.") == "":
				problems = append(problems, fmt.Sprintf("%s.streams.%s binds the whole exchange (routing_key %q) - name a narrow routing key", at, n, s.RoutingKey))
			}
		}
	}
	check(c.Targets.MessageBroker, "targets.message_broker")
	names := make([]string, 0, len(c.Targets.MessageBrokerTargets))
	for n := range c.Targets.MessageBrokerTargets {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		check(c.Targets.MessageBrokerTargets[n], "targets.message_broker_targets."+n)
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("bus declarations: %s", strings.Join(problems, "; "))
}

// busScenarioErrors is the `validate-config --scenarios` cross-check of a chain scenario's bus steps against THIS config
// the step's `target` must be a message_broker target (the plain one when none is named), and its
// `stream`, when it has one, must be declared under that target's `streams`. Each mismatch is reported with the scenario
// file and the step's name, so an author finds the step without running anything. A `${…}` in either field is not read
// (it is built at run time, and BusStepProblems refuses a `${…}` stream in its own right). Envelope, URL and vhost are the
// executor's preflight to judge, not this check's.
func (c *Config) busScenarioErrors(s *scenario.Scenario, file string) []ConfigError {
	if scenario.RunnerKindOf(s) != scenario.RunnerChain {
		return nil
	}
	steps, err := scenario.ParseChainSteps(s.Trigger.Payload)
	if err != nil {
		return nil
	}
	var errs []ConfigError
	for i, st := range steps {
		if st.Type != "bus" || strings.Contains(st.Target, "${") || strings.Contains(st.Stream, "${") {
			continue
		}
		name := st.Name
		if name == "" {
			name = fmt.Sprintf("#%d", i+1)
		}
		t, label, terr := c.BusTargetFor(st.Target)
		if terr != nil {
			errs = append(errs, ConfigError{Scenario: s.ID, Message: fmt.Sprintf("scenario file %s, bus step %q: %s", file, name, terr.Error())})
			continue
		}
		if st.Stream == "" {
			continue
		}
		if _, serr := t.BusStreamNamed(label, st.Stream); serr != nil {
			errs = append(errs, ConfigError{Scenario: s.ID, Message: fmt.Sprintf("scenario file %s, bus step %q: %s", file, name, serr.Error())})
		}
	}
	return errs
}
