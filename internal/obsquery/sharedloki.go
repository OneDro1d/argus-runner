package obsquery

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// THE SHARED-LOKI LOG SOURCE: query-time parsing.
//
// An environment's ONE shared promtail ships every pod's log to one shared Loki and stamps only
// `cluster` (an external label), `namespace`, `pod`, `container`, `node` and `app`. It has no json stage, so the
// labels Argus's OWN per-instance promtail produced - argus_instance, project, service, level, correlation_id,
// event_type - do not exist there. Rather than depend on that promtail being changed, an instance whose
// observability.loki.shared_cluster is set reads with
//
//	{cluster="<cluster>", namespace="<SUT namespace>"[, container=...]}  <line filters>  | json|logfmt <derived labels>  | label_format event_type=...  | <label filters>
//
// i.e. the stream selector names WHERE the lines are (the cluster label keeps a query from one cluster off a same-named
// namespace another cluster ships into the same Loki), and level / correlation_id / event_type are DERIVED from
// the line at query time by the same rules the ingest-time pipeline applied (internal/sutlogs/labels.go is that
// pipeline in Go; sharedloki_test.go holds the two equal).
//
// ⛔ AN INSTANCE WITH ITS OWN LOKI NEVER REACHES THIS FILE: Loki.Shared is nil for it and every query is
// CorrelationLogQL / SourceLogQL, byte for byte as before.
// ⛔ NO FALLBACK. A shared source with no cluster or namespace builds NO query (an error), it does not fall back to
// the service-label selector, which would match nothing in a shared Loki and let `count == 0` hold.

// SharedLogSource says where an instance's SUT logs are in a shared Loki and how to read the labels it does not carry.
type SharedLogSource struct {
	// Cluster is the shared promtail's external `cluster` label for this instance's cluster ("cluster-a").
	Cluster string
	// Namespace is the SUT's Kubernetes namespace (ARGUS_SUT_NAMESPACE, the value the executor's NetworkPolicy is scoped to).
	Namespace string
	// Constants are the labels the per-instance pipeline stamped on EVERY line (argus_instance, project): a step that
	// selects on one with its own value is selecting everything, and is left out; any other value selected nothing
	// there, and is refused here rather than turned into a selector that matches nothing.
	Constants map[string]string

	// The SUT's declared log-field translation table (observability.loki.*), as the promtail parse stage read it.
	Format string // "json" | "logfmt" (anything else = json)
	// Regexp (observability.loki.log_regex) is a PLAIN-TEXT format: an RE2 expression with named groups, read by
	// LogQL `| regexp`. When set it wins over Format; the groups level / correlation_id / saga_marker ARE the labels
	// (LevelField and friends name JSON/logfmt fields and do not apply). A label the expression has no group for
	// is refused when a claim names it, never invented.
	Regexp           string
	LevelField       string   // -> level
	CorrelationField string   // -> correlation_id
	SagaField        string   // -> saga_marker -> event_type
	SagaMarkers      []string // the values of SagaField that mean "saga"
}

// sharedValueRe is a label value this package will put in a selector (it is also quoted).
var sharedValueRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// check refuses a source that cannot name its streams.
func (s *SharedLogSource) check() error {
	if s == nil {
		return errors.New("no shared-Loki log source")
	}
	if !sharedValueRe.MatchString(s.Cluster) {
		return fmt.Errorf("the shared Loki needs this instance's cluster label (observability.loki.shared_cluster), got %s", strconv.Quote(s.Cluster))
	}
	if !sharedValueRe.MatchString(s.Namespace) {
		return fmt.Errorf("the shared Loki needs the SUT namespace (ARGUS_SUT_NAMESPACE, set by render-k8s --sut-namespace), got %s - "+
			"refusing to read a cluster's logs without one", strconv.Quote(s.Namespace))
	}
	return nil
}

// Selector is the stream selector of every shared read of the correlation kind: the cluster and the SUT
// namespace, without the observability plane's own containers (the same exclusion CorrelationLogQL makes by
// `service`; here the label is `container`). It is also what the saga-presence template selects with.
func (s *SharedLogSource) Selector() (string, error) {
	if err := s.check(); err != nil {
		return "", err
	}
	return `{cluster=` + logqlString(s.Cluster) + `,namespace=` + logqlString(s.Namespace) + `,container!~"` + ObsInfraExclusion + `"}`, nil
}

// CorrelationLogQL is CorrelationLogQL for a shared Loki: the same line filters over the cluster+namespace selector.
func (s *SharedLogSource) CorrelationLogQL(correlationID, contains, matching string) (string, error) {
	sel, err := s.Selector()
	if err != nil {
		return "", err
	}
	return sel + ` |= ` + logqlString(correlationID) + lineFilters(contains, matching), nil
}

// derivedLabels are the labels that only exist at ingest time in an instance's own Loki.
var derivedLabels = map[string]bool{"level": true, "correlation_id": true, "event_type": true}

// SourceLogQL is SourceLogQL for a shared Loki. The author's labels are translated:
//
//	service                          -> container (the per-instance pipeline's `service` WAS the container name)
//	level, correlation_id, event_type -> a parse stage and a label filter (see above)
//	argus_instance, project           -> dropped when they name this instance's own value, else refused
//	cluster, namespace                -> refused: the instance fixes them
//	anything else (app, pod, node, container, ...) -> a stream label, as written
func (s *SharedLogSource) SourceLogQL(labels map[string]string, contains, matching string) (string, error) {
	if err := s.check(); err != nil {
		return "", err
	}
	if len(labels) == 0 {
		return "", errors.New("a source query needs at least one label")
	}
	names := make([]string, 0, len(labels))
	for k := range labels {
		names = append(names, k)
	}
	sort.Strings(names)
	stream := map[string]string{"cluster": s.Cluster, "namespace": s.Namespace}
	derived := map[string]string{}
	for _, k := range names {
		v := labels[k]
		if !lokiLabelNameRe.MatchString(k) {
			return "", fmt.Errorf("label name %s is not a Loki label name", strconv.Quote(k))
		}
		if v == "" {
			return "", fmt.Errorf("label %s has an empty value, which would match every stream without it", k)
		}
		switch {
		case k == "cluster" || k == "namespace":
			return "", fmt.Errorf("label %s is fixed by the instance in a shared Loki (cluster %s, namespace %s) - remove it from the step", k, s.Cluster, s.Namespace)
		case k == "argus_instance" || k == "project":
			own, ok := s.Constants[k]
			if !ok || own == "" {
				return "", fmt.Errorf("label %s does not exist in a shared Loki and this instance has no value of it to compare with - select by app/container, or remove it", k)
			}
			if v != own {
				return "", fmt.Errorf("label %s=%s does not exist in a shared Loki (this instance's own value is not that), so it could only match nothing - remove it from the step", k, strconv.Quote(v))
			}
			// this instance's own value: every line carried it, so it selects nothing in particular
		case k == "service":
			if _, both := labels["container"]; both {
				return "", errors.New("labels service and container both name the container in a shared Loki - give one")
			}
			stream["container"] = v
		case derivedLabels[k]:
			if k == "correlation_id" && !strings.HasPrefix(v, "tr-") {
				return "", fmt.Errorf("label correlation_id=%s: only an Argus id (tr-...) was ever a correlation_id label; select other ids with `contains`", strconv.Quote(v))
			}
			derived[k] = v
		default:
			stream[k] = v
		}
	}
	snames := make([]string, 0, len(stream))
	for k := range stream {
		snames = append(snames, k)
	}
	sort.Strings(snames)
	parts := make([]string, 0, len(snames))
	for _, k := range snames {
		parts = append(parts, k+"="+logqlString(stream[k]))
	}
	q := "{" + strings.Join(parts, ",") + "}" + lineFilters(contains, matching)
	stages, err := s.deriveStages(derived)
	if err != nil {
		return "", err
	}
	return q + stages, nil
}

// regexpGroup is the named group a plain-text format must carry for each derived label.
var regexpGroup = map[string]string{"level": "level", "correlation_id": "correlation_id", "event_type": "saga_marker"}

// regexpGroups is the set of named groups of the declared Regexp ("" Regexp = none, nil error).
func (s *SharedLogSource) regexpGroups() (map[string]bool, error) {
	re, err := regexp.Compile(s.Regexp)
	if err != nil {
		return nil, fmt.Errorf("observability.loki.log_regex is not a valid RE2 expression: %v", err)
	}
	groups := map[string]bool{}
	for _, n := range re.SubexpNames() {
		if n != "" {
			groups[n] = true
		}
	}
	return groups, nil
}

// deriveStages is the query-time image of the promtail parse stage, the saga template and the labels stage.
func (s *SharedLogSource) deriveStages(derived map[string]string) (string, error) {
	if len(derived) == 0 {
		return "", nil
	}
	if s.Regexp != "" {
		return s.deriveRegexpStages(derived)
	}
	var exprs []string
	add := func(label, field string) { exprs = append(exprs, label+"="+logqlString(field)) }
	if _, ok := derived["level"]; ok {
		add("level", s.LevelField)
	}
	if _, ok := derived["correlation_id"]; ok {
		add("correlation_id", s.CorrelationField)
	}
	if _, ok := derived["event_type"]; ok {
		add("saga_marker", s.SagaField)
	}
	parser := "json"
	if strings.EqualFold(strings.TrimSpace(s.Format), "logfmt") {
		parser = "logfmt"
	}
	out := " | " + parser + " " + strings.Join(exprs, ", ")
	if _, ok := derived["event_type"]; ok {
		out += " | label_format event_type=" + logqlString(s.sagaTemplate())
	}
	for _, k := range []string{"level", "correlation_id", "event_type"} {
		if v, ok := derived[k]; ok {
			out += " | " + k + "=" + logqlString(v)
		}
	}
	return out, nil
}

// deriveRegexpStages is deriveStages for a plain-text format: `| regexp "<expr>"` (its named groups become the
// labels), the same event_type label_format, and the same label filters. A label the expression has no group for
// is refused - the SUT's line does not carry it as far as the declaration says, and inventing it would let the
// claim hold on a label that can never be set.
func (s *SharedLogSource) deriveRegexpStages(derived map[string]string) (string, error) {
	groups, err := s.regexpGroups()
	if err != nil {
		return "", err
	}
	for _, k := range []string{"level", "correlation_id", "event_type"} {
		if _, ok := derived[k]; ok && !groups[regexpGroup[k]] {
			return "", fmt.Errorf("label %s cannot be derived: observability.loki.log_regex has no (?P<%s>...) group - add one, or remove %s from the step", k, regexpGroup[k], k)
		}
	}
	out := " | regexp " + logqlString(s.Regexp)
	if _, ok := derived["event_type"]; ok {
		out += " | label_format event_type=" + logqlString(s.sagaTemplate())
	}
	for _, k := range []string{"level", "correlation_id", "event_type"} {
		if v, ok := derived[k]; ok {
			out += " | " + k + "=" + logqlString(v)
		}
	}
	return out, nil
}

// sagaTemplate is the promtail `event_type` template (obsconfig.RenderPromtail) verbatim: every declared marker
// value becomes "saga"; any other value passes through unchanged, so it never masquerades as a saga.
func (s *SharedLogSource) sagaTemplate() string {
	eqs := make([]string, 0, len(s.SagaMarkers))
	for _, v := range s.SagaMarkers {
		eqs = append(eqs, `(eq .saga_marker `+strconv.Quote(v)+`)`)
	}
	cond := `eq .saga_marker "saga"`
	switch len(eqs) {
	case 0:
	case 1:
		cond = strings.TrimSuffix(strings.TrimPrefix(eqs[0], "("), ")")
	default:
		cond = "or " + strings.Join(eqs, " ")
	}
	return `{{ if ` + cond + ` }}saga{{ else }}{{ .saga_marker | default "" }}{{ end }}`
}

// leadingSelector returns the `{...}` a query starts with (quotes honoured), or false.
func leadingSelector(q string) (string, bool) {
	if !strings.HasPrefix(q, "{") {
		return "", false
	}
	inStr := false
	for i := 1; i < len(q); i++ {
		switch c := q[i]; {
		case inStr && c == '\\':
			i++
		case c == '"':
			inStr = !inStr
		case !inStr && c == '}':
			return q[:i+1], true
		}
	}
	return "", false
}

// LogsQueryBuilder builds a logs step's LogQL. *Loki implements it: its own-Loki queries are exactly
// CorrelationLogQL and SourceLogQL, a shared Loki's are the SharedLogSource's.
type LogsQueryBuilder interface {
	CorrelationQuery(correlationID, contains, matching string) (string, error)
	SourceQuery(labels map[string]string, contains, matching string) (string, error)
}

// CorrelationQuery is the logs step's correlation-scope query for this Loki.
func (l *Loki) CorrelationQuery(correlationID, contains, matching string) (string, error) {
	if l.Shared != nil {
		return l.Shared.CorrelationLogQL(correlationID, contains, matching)
	}
	return CorrelationLogQL(correlationID, contains, matching), nil
}

// SourceQuery is the logs step's source-scope query for this Loki.
func (l *Loki) SourceQuery(labels map[string]string, contains, matching string) (string, error) {
	if l.Shared != nil {
		return l.Shared.SourceLogQL(labels, contains, matching)
	}
	return SourceLogQL(labels, contains, matching)
}
