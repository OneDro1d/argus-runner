// Package sutlogs is the pod-log collector behind `argus collect-logs` (render-k8s --sut-logs-via api): it
// follows the logs of the SUT namespace's pods through the Kubernetes API and ships them to the executor's
// Loki with the SAME labels and the SAME line content the promtail DaemonSet (--sut-logs-via node) gives
// them, with no hostPath, no DaemonSet and nothing cluster-scoped.
package sutlogs

import (
	"encoding/json"
	"strconv"
	"strings"
)

// Rules is everything the label derivation reads. The first four become the FIXED labels, the rest are
// the SUT's declared log-field translation table (config: observability.loki.*).
type Rules struct {
	Instance string // argus_instance
	Project  string // project   (config.ProjectLabel())
	Cluster  string // cluster

	Format           string   // config.LogFormat(): "json" | "logfmt"
	LevelField       string   // config.LevelField()
	CorrelationField string   // config.CorrelationField()
	SagaEventField   string   // config.SagaEventField()
	SagaMarkers      []string // config.SagaEventValues()
}

// Labels is THE label derivation, one pure function. It mirrors the promtail pipeline k8srender renders
// for --sut-logs-via node (renderBundledPromtailBlock, renderExportPromtail, renderSharedInstanceObs —
// held equal by labels_test.go, which quotes the template lines):
//
//	static labels   argus_instance, project, cluster          <- Rules
//	regex stage     service = the container directory name    <- container
//	json | logfmt   level, correlation_id, saga_marker        <- the line, by the declared field names
//	template        correlation_id: '{{ if hasPrefix "tr-" .Value }}{{ .Value }}{{ end }}'
//	template        event_type: '{{ if <eq .saga_marker "a" or ...> }}saga{{ else }}{{ .saga_marker | default "" }}{{ end }}'
//	labels          level, correlation_id, event_type
//
// A label whose value is empty is NOT set (promtail drops it). There is no pod, namespace, container,
// job, filename or stream label: every distinct value would be a new Loki stream. A line that does not
// parse still carries the four fixed labels (argus_instance, project, cluster, service).
func Labels(r Rules, container, line string) map[string]string {
	out := make(map[string]string, 7)
	set := func(k, v string) {
		if v != "" {
			out[k] = v
		}
	}
	set("argus_instance", r.Instance)
	set("project", r.Project)
	set("cluster", r.Cluster)
	set("service", container)

	fields := extract(r, line)
	set("level", fields["level"])
	if v := fields["correlation_id"]; strings.HasPrefix(v, "tr-") {
		set("correlation_id", v)
	}
	marker := fields["saga_marker"]
	for _, m := range r.SagaMarkers {
		if marker == m {
			marker = "saga"
			break
		}
	}
	set("event_type", marker)
	return out
}

// extract is promtail's parse stage: the three extracted values by the SUT's declared field names. A
// line the stage cannot parse extracts nothing.
func extract(r Rules, line string) map[string]string {
	if r.Format == "logfmt" {
		kv, ok := parseLogfmt(line)
		if !ok {
			return nil
		}
		return map[string]string{
			"level":          kv[r.LevelField],
			"correlation_id": kv[r.CorrelationField],
			"saga_marker":    kv[r.SagaEventField],
		}
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(line), &doc); err != nil {
		return nil
	}
	return map[string]string{
		"level":          jsonField(doc, r.LevelField),
		"correlation_id": jsonField(doc, r.CorrelationField),
		"saga_marker":    jsonField(doc, r.SagaEventField),
	}
}

// jsonField reads a (dotted) path out of a decoded JSON object the way promtail's json stage reads a
// simple expression, and renders it as promtail does: a string as is, a number without exponent, a
// bool as true/false, an object or array as compact JSON, null as nothing.
func jsonField(doc map[string]any, path string) string {
	var cur any = doc
	for _, part := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur, ok = m[part]
		if !ok {
			return ""
		}
	}
	switch v := cur.(type) {
	case nil:
		return ""
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(v)
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return ""
		}
		return string(b)
	}
}

// parseLogfmt reads key=value pairs the way go-logfmt (promtail's logfmt stage) does: a key is a run of
// bytes above space other than '=' and '"'; a value is bare (same rule, and a '=' or '"' inside it is an
// error) or double-quoted with Go/JSON escapes; a key with no '=' has an empty value. Any syntax error
// makes the whole line yield nothing, as promtail's decoder does. The LAST occurrence of a key wins.
func parseLogfmt(line string) (map[string]string, bool) {
	kv := map[string]string{}
	i, n := 0, len(line)
	isKeyByte := func(c byte) bool { return c > ' ' && c != '=' && c != '"' }
	for i < n {
		for i < n && line[i] <= ' ' {
			i++
		}
		if i >= n {
			break
		}
		start := i
		for i < n && isKeyByte(line[i]) {
			i++
		}
		if i == start {
			return nil, false // a stray '=' or '"' where a key must start
		}
		key := line[start:i]
		if i >= n || line[i] <= ' ' {
			kv[key] = ""
			continue
		}
		if line[i] != '=' {
			return nil, false
		}
		i++ // '='
		if i < n && line[i] == '"' {
			j := i + 1
			for j < n && line[j] != '"' {
				if line[j] == '\\' {
					j++
				}
				j++
			}
			if j >= n {
				return nil, false // unterminated quote
			}
			v, err := strconv.Unquote(line[i : j+1])
			if err != nil {
				return nil, false
			}
			kv[key] = v
			i = j + 1
			if i < n && line[i] > ' ' {
				return nil, false
			}
			continue
		}
		vs := i
		for i < n && line[i] > ' ' {
			if line[i] == '=' || line[i] == '"' {
				return nil, false
			}
			i++
		}
		kv[key] = line[vs:i]
	}
	return kv, true
}
