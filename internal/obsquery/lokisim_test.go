package obsquery_test

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"text/template"
)

// A deliberately small LogQL evaluator for the SHAPES Argus's shared-Loki builder emits: a stream
// selector (=, !=, =~, !~), line filters (|= and |~), `| json` / `| logfmt` with name="field" expressions,
// `| label_format name="<go template>"` and `| label = "v"` / `| label =~ "v"` filters. Anything else is a test
// failure ("simulator does not know ..."), so a change to the builder's output cannot slip past unevaluated.
//
// It is NOT Loki. It exists so a test can feed a SUT log line through the generated query and see whether it is
// selected - which a string comparison of the query cannot say. Its template engine is Go's text/template (what
// Loki's label_format uses) with the one non-builtin function the builder relies on, sprig's `default`.

type simStream struct {
	labels map[string]string
	lines  []string
}

// simQuery returns, in order, the lines of the streams the query selects and keeps.
func simQuery(t *testing.T, q string, streams []simStream) []string {
	t.Helper()
	p := &simParser{t: t, s: q}
	matchers := p.selector()
	stages := p.stages()
	var out []string
	for _, st := range streams {
		if !matchers(st.labels) {
			continue
		}
		for _, line := range st.lines {
			labels := map[string]string{}
			for k, v := range st.labels {
				labels[k] = v
			}
			keep := true
			for _, stage := range stages {
				if keep = stage(line, labels); !keep {
					break
				}
			}
			if keep {
				out = append(out, line)
			}
		}
	}
	return out
}

type simParser struct {
	t *testing.T
	s string
	i int
}

func (p *simParser) fail(f string, a ...any) {
	p.t.Helper()
	p.t.Fatalf("simulator does not know %s (at %d of %q)", fmt.Sprintf(f, a...), p.i, p.s)
}

func (p *simParser) skipSpace() {
	for p.i < len(p.s) && p.s[p.i] == ' ' {
		p.i++
	}
}

func (p *simParser) ident() string {
	start := p.i
	for p.i < len(p.s) && (p.s[p.i] == '_' || p.s[p.i] >= 'a' && p.s[p.i] <= 'z' || p.s[p.i] >= 'A' && p.s[p.i] <= 'Z' || p.s[p.i] >= '0' && p.s[p.i] <= '9') {
		p.i++
	}
	if p.i == start {
		p.fail("an identifier here")
	}
	return p.s[start:p.i]
}

func (p *simParser) quoted() string {
	if p.i >= len(p.s) || p.s[p.i] != '"' {
		p.fail("a quoted string here")
	}
	j := p.i + 1
	for j < len(p.s) && p.s[j] != '"' {
		if p.s[j] == '\\' {
			j++
		}
		j++
	}
	v, err := strconv.Unquote(p.s[p.i : j+1])
	if err != nil {
		p.fail("this string literal: %v", err)
	}
	p.i = j + 1
	return v
}

func (p *simParser) op() string {
	for _, o := range []string{"=~", "!~", "!=", "="} {
		if strings.HasPrefix(p.s[p.i:], o) {
			p.i += len(o)
			return o
		}
	}
	p.fail("this operator")
	return ""
}

func matchOp(op, have, want string) bool {
	switch op {
	case "=":
		return have == want
	case "!=":
		return have != want
	case "=~":
		return regexp.MustCompile("^(?:" + want + ")$").MatchString(have)
	case "!~":
		return !regexp.MustCompile("^(?:" + want + ")$").MatchString(have)
	}
	return false
}

func (p *simParser) selector() func(map[string]string) bool {
	if p.s[p.i] != '{' {
		p.fail("a query that does not start with a selector")
	}
	p.i++
	type m struct{ name, op, val string }
	var ms []m
	for {
		p.skipSpace()
		if p.s[p.i] == '}' {
			p.i++
			break
		}
		if p.s[p.i] == ',' {
			p.i++
			continue
		}
		name := p.ident()
		op := p.op()
		ms = append(ms, m{name, op, p.quoted()})
	}
	return func(l map[string]string) bool {
		for _, x := range ms {
			if !matchOp(x.op, l[x.name], x.val) {
				return false
			}
		}
		return true
	}
}

func (p *simParser) stages() []func(line string, labels map[string]string) bool {
	var out []func(string, map[string]string) bool
	for {
		p.skipSpace()
		if p.i >= len(p.s) {
			return out
		}
		switch {
		case strings.HasPrefix(p.s[p.i:], "|="):
			p.i += 2
			p.skipSpace()
			want := p.quoted()
			out = append(out, func(line string, _ map[string]string) bool { return strings.Contains(line, want) })
		case strings.HasPrefix(p.s[p.i:], "|~"):
			p.i += 2
			p.skipSpace()
			re := regexp.MustCompile(p.quoted())
			out = append(out, func(line string, _ map[string]string) bool { return re.MatchString(line) })
		case strings.HasPrefix(p.s[p.i:], "| "):
			p.i += 2
			p.skipSpace()
			name := p.ident()
			switch name {
			case "json", "logfmt":
				exprs := map[string]string{}
				var order []string
				for {
					p.skipSpace()
					if p.i >= len(p.s) || p.s[p.i] == '|' {
						break
					}
					label := p.ident()
					if p.s[p.i] != '=' {
						p.fail("an extraction without an expression")
					}
					p.i++
					exprs[label] = p.quoted()
					order = append(order, label)
					p.skipSpace()
					if p.i < len(p.s) && p.s[p.i] == ',' {
						p.i++
					}
				}
				if len(order) == 0 {
					p.fail("a bare | %s", name)
				}
				out = append(out, simExtract(name, order, exprs))
			case "regexp":
				// LogQL `| regexp "<RE2 with named groups>"`: each named group of a matching line becomes a label; a
				// line the expression does not match gets no label and NO __error__ (Loki's regexp parser).
				p.skipSpace()
				re := regexp.MustCompile(p.quoted())
				out = append(out, func(line string, labels map[string]string) bool {
					if m := re.FindStringSubmatch(line); m != nil {
						for i, n := range re.SubexpNames() {
							if n != "" {
								labels[n] = m[i]
							}
						}
					}
					return true
				})
			case "label_format":
				p.skipSpace()
				dst := p.ident()
				if p.s[p.i] != '=' {
					p.fail("label_format without =")
				}
				p.i++
				tmpl := template.Must(template.New(dst).Funcs(template.FuncMap{"default": func(d string, v any) string {
					if s, ok := v.(string); ok && s != "" {
						return s
					}
					return d
				}}).Parse(p.quoted()))
				out = append(out, func(_ string, labels map[string]string) bool {
					data := map[string]any{}
					for k, v := range labels {
						data[k] = v
					}
					var b strings.Builder
					if err := tmpl.Execute(&b, data); err != nil {
						p.t.Fatalf("label_format template: %v", err)
					}
					labels[dst] = b.String()
					return true
				})
			default:
				op := p.op()
				want := p.quoted()
				out = append(out, func(_ string, labels map[string]string) bool { return matchOp(op, labels[name], want) })
			}
		default:
			p.fail("this stage")
		}
	}
}

// simExtract is `| json a="x.y"` / `| logfmt a="x"`: a label is set when the line parses and has the field.
// A json line that does not parse gets __error__="JSONParserErr" and no label, and is kept (as Loki does).
func simExtract(parser string, order []string, exprs map[string]string) func(string, map[string]string) bool {
	return func(line string, labels map[string]string) bool {
		var get func(path string) (string, bool)
		if parser == "json" {
			var doc map[string]any
			if json.Unmarshal([]byte(line), &doc) != nil {
				labels["__error__"] = "JSONParserErr" // Loki keeps the line and sets __error__
				return true
			}
			get = func(path string) (string, bool) {
				var cur any = doc
				for _, part := range strings.Split(path, ".") {
					m, ok := cur.(map[string]any)
					if !ok {
						return "", false
					}
					if cur, ok = m[part]; !ok {
						return "", false
					}
				}
				switch v := cur.(type) {
				case string:
					return v, true
				case nil:
					return "", false
				default:
					return fmt.Sprint(v), true
				}
			}
		} else {
			kv := map[string]string{}
			for _, f := range strings.Fields(line) {
				if k, v, ok := strings.Cut(f, "="); ok {
					kv[k] = strings.Trim(v, `"`)
				}
			}
			get = func(path string) (string, bool) { v, ok := kv[path]; return v, ok }
		}
		for _, label := range order {
			if v, ok := get(exprs[label]); ok {
				labels[label] = v
			}
		}
		return true
	}
}
