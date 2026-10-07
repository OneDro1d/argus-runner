package testtargets

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// testtargets_test.go — (UI-6 backend): the pure core of "one instance, several named
// test targets". No database, no files.

func live() Target {
	return Target{Name: "live", Label: "msgbus live", Namespace: "msgbus",
		Match: Match{ScenarioPrefixes: []string{"NHB-"}, Tags: []string{"heartbeat", "slack-pulse"}}}
}

func lab() Target {
	return Target{Name: "lab", Label: "msgbus lab", Namespace: "msgbus-lab",
		Match: Match{ScenarioPrefixes: []string{"NLB-"}, Tags: []string{"lab", "sink"}}}
}

func TestValidate_AcceptsTheDesignExample(t *testing.T) {
	if err := (List{live(), lab()}).Validate(); err != nil {
		t.Fatalf("the §8 example was refused: %v", err)
	}
}

func TestValidate_AbsentAndEmptyAreValid(t *testing.T) {
	if err := (List(nil)).Validate(); err != nil {
		t.Fatalf("absent test_targets refused: %v", err)
	}
	if err := (List{}).Validate(); err != nil {
		t.Fatalf("empty test_targets refused: %v", err)
	}
}

func TestValidate_Refusals(t *testing.T) {
	nine := List{}
	for i := 0; i < 9; i++ {
		nine = append(nine, Target{Name: "t" + string(rune('a'+i)), Match: Match{Tags: []string{"x"}}})
	}
	cases := []struct {
		name string
		in   List
		want string
	}{
		{"more than eight", nine, "at most 8"},
		{"duplicate name", List{live(), live()}, "duplicate"},
		{"bad name: uppercase", List{{Name: "Live", Match: Match{Tags: []string{"x"}}}}, "name"},
		{"bad name: leading digit", List{{Name: "1live", Match: Match{Tags: []string{"x"}}}}, "name"},
		{"bad name: too long", List{{Name: "a" + strings.Repeat("b", 32), Match: Match{Tags: []string{"x"}}}}, "name"},
		{"empty name", List{{Name: "", Match: Match{Tags: []string{"x"}}}}, "name"},
		{"reserved name", List{{Name: "unassigned", Match: Match{Tags: []string{"x"}}}}, "unassigned"},
		{"empty match", List{{Name: "live"}}, "match"},
		{"blank prefix", List{{Name: "live", Match: Match{ScenarioPrefixes: []string{" "}}}}, "scenario_prefixes"},
		{"blank tag", List{{Name: "live", Match: Match{Tags: []string{""}}}}, "tags"},
	}
	for _, c := range cases {
		err := c.in.Validate()
		if err == nil {
			t.Errorf("%s: accepted", c.name)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: refusal %q does not mention %q", c.name, err, c.want)
		}
		if !strings.Contains(err.Error(), "test_targets") {
			t.Errorf("%s: refusal %q does not name the key test_targets", c.name, err)
		}
	}
}

// ── tag → target mapping ──────────────────────────────────────────────────────────────────────────

func TestMap_PrefixThenTagsThenUnassigned(t *testing.T) {
	l := List{live(), lab()}
	cases := []struct {
		id   string
		tags []string
		want string
	}{
		{"NHB-001", nil, "live"},                        // prefix of the first target
		{"NLB-001", []string{"heartbeat"}, "lab"},       // PREFIX beats a tag of an earlier target
		{"ZZZ-1", []string{"sink"}, "lab"},              // no prefix: tag intersection
		{"ZZZ-2", []string{"lab", "heartbeat"}, "live"}, // several tags: first TARGET (declaration order) wins
		{"ZZZ-3", []string{"unrelated"}, Unassigned},
		{"ZZZ-4", nil, Unassigned},
	}
	for _, c := range cases {
		if got := l.Map(c.id, c.tags); got != c.want {
			t.Errorf("Map(%q, %v) = %q, want %q", c.id, c.tags, got, c.want)
		}
	}
}

func TestMap_NoDeclarationIsUnassigned(t *testing.T) {
	if got := (List(nil)).Map("NHB-001", []string{"heartbeat"}); got != Unassigned {
		t.Fatalf("Map on an undeclared list = %q, want %q", got, Unassigned)
	}
}

func TestForRun(t *testing.T) {
	l := List{live(), lab()}
	tags := map[string][]string{"S-9": {"sink"}, "S-8": {"nope"}}
	cases := []struct {
		name string
		decl List
		ids  []string
		want []string
	}{
		{"no declaration => NULL (today's view)", nil, []string{"NHB-1"}, nil},
		{"empty declaration => NULL", List{}, []string{"NHB-1"}, nil},
		{"one target", l, []string{"NHB-1", "NHB-2"}, []string{"live"}},
		{"both, declaration order", l, []string{"NLB-1", "NHB-1"}, []string{"live", "lab"}},
		{"tag-mapped", l, []string{"S-9"}, []string{"lab"}},
		{"unassigned last", l, []string{"S-8", "NHB-1"}, []string{"live", Unassigned}},
		{"no scenarios => stamped, empty (not NULL)", l, nil, []string{}},
	}
	for _, c := range cases {
		got := c.decl.ForRun(c.ids, tags)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: ForRun = %#v, want %#v", c.name, got, c.want)
		}
	}
}

// ── wire encoding ─────────────────────────────────────────────────────────────────────────────────

func TestDecode_NilIsNotReported_EmptyArrayIsReportedNone(t *testing.T) {
	l, reported, err := Decode(nil)
	if err != nil || reported || l != nil {
		t.Fatalf("Decode(nil) = %v, %v, %v; want nil,false,nil", l, reported, err)
	}
	l, reported, err = Decode(json.RawMessage(`[]`))
	if err != nil || !reported || len(l) != 0 {
		t.Fatalf("Decode([]) = %v, %v, %v; want empty,true,nil", l, reported, err)
	}
}

func TestEncodeDecode_RoundTrip(t *testing.T) {
	in := List{live(), lab()}
	raw, err := Encode(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"scenario_prefixes"`) || !strings.Contains(string(raw), `"namespace"`) {
		t.Fatalf("wire JSON does not carry the declared keys: %s", raw)
	}
	out, reported, err := Decode(raw)
	if err != nil || !reported {
		t.Fatalf("Decode: %v %v", reported, err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("round trip changed the list:\n in %#v\nout %#v", in, out)
	}
}

func TestEncode_EmptyListIsEmptyArrayNotNull(t *testing.T) {
	raw, err := Encode(nil)
	if err != nil || string(raw) != "[]" {
		t.Fatalf("Encode(nil) = %s, %v; want []", raw, err)
	}
}

func TestDecode_RefusesAnInvalidDeclaration(t *testing.T) {
	// Defence for the CP: it stores only what Validate would have let an executor load.
	if _, _, err := Decode(json.RawMessage(`[{"name":"Bad Name","match":{"tags":["x"]}}]`)); err == nil {
		t.Fatal("Decode accepted an invalid name")
	}
	if _, _, err := Decode(json.RawMessage(`{"not":"an array"}`)); err == nil {
		t.Fatal("Decode accepted a non-array")
	}
}

// UI-6 front end: the web's `?target=` filter checks a value with the SAME rule a declared
// name must pass, so the filter can never accept a value no declaration could hold. `unassigned` passes the
// shape rule on purpose: it is a real filter value, only not a declarable name (Validate refuses that).
func TestValidName_IsTheDeclarationNameRule(t *testing.T) {
	for _, ok := range []string{"live", "lab", "msgbus-lab", "a", Unassigned, "a" + strings.Repeat("b", 31)} {
		if !ValidName(ok) {
			t.Errorf("ValidName(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"", "Live", "1lab", "-lab", "lab_1", "lab 1", "lab;drop", "a" + strings.Repeat("b", 32), "lab\n"} {
		if ValidName(bad) {
			t.Errorf("ValidName(%q) = true, want false", bad)
		}
	}
	l := List{{Name: "live", Match: Match{Tags: []string{"x"}}}, {Name: "lab-2", Match: Match{Tags: []string{"y"}}}}
	if err := l.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, tt := range l {
		if !ValidName(tt.Name) {
			t.Errorf("Validate accepted %q but ValidName refuses it", tt.Name)
		}
	}
}

func TestDecode_IgnoresKeysAThirdExecutorAdds(t *testing.T) {
	// Forward compatibility: a LATER executor may add a per-target key; this control plane must keep
	// working on the keys it knows (no DisallowUnknownFields on the wire).
	l, reported, err := Decode(json.RawMessage(`[{"name":"live","match":{"tags":["x"]},"future_key":1}]`))
	if err != nil || !reported || len(l) != 1 || l[0].Name != "live" {
		t.Fatalf("Decode with an unknown wire key = %v %v %v", l, reported, err)
	}
}

// ── load_test: never ───────────────────────────────────────────────────────────

func neverLive() Target {
	t := live()
	t.LoadTest = LoadTestNever
	return t
}

func TestLoadTest_NeverIsValid_AbsentIsNotNever(t *testing.T) {
	if err := (List{neverLive(), lab()}).Validate(); err != nil {
		t.Fatalf("load_test: never refused: %v", err)
	}
	if !neverLive().NeverLoadTested() {
		t.Error("a target with load_test: never must report NeverLoadTested")
	}
	if lab().NeverLoadTested() {
		t.Error("a target without the key must NOT report NeverLoadTested (absent = today's behaviour)")
	}
}

func TestLoadTest_AnyOtherValueIsRefusedByName(t *testing.T) {
	for _, v := range []string{"always", "Never", "NEVER", "no", "true", " never", "never "} {
		tt := live()
		tt.LoadTest = v
		err := (List{tt}).Validate()
		if err == nil {
			t.Errorf("load_test %q accepted", v)
			continue
		}
		if !strings.Contains(err.Error(), "load_test") || !strings.Contains(err.Error(), "live") || !strings.Contains(err.Error(), "never") {
			t.Errorf("load_test %q: refusal %q does not name the key, the target and the one accepted value", v, err)
		}
	}
}

func TestLoadTest_WireRoundTripKeepsTheKey_AndAbsentStaysAbsent(t *testing.T) {
	raw, err := Encode(List{neverLive(), lab()})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"load_test":"never"`) {
		t.Fatalf("the wire form lacks the key: %s", raw)
	}
	l, reported, err := Decode(raw)
	if err != nil || !reported || len(l) != 2 || !l[0].NeverLoadTested() || l[1].NeverLoadTested() {
		t.Fatalf("round trip = %+v %v %v", l, reported, err)
	}
	if strings.Count(string(raw), "load_test") != 1 {
		t.Errorf("a target that does not declare the key must not carry it on the wire: %s", raw)
	}
}

// The other direction: an OLDER executor reports a declaration with no such key. It must read as "not
// declared", never as never.
func TestLoadTest_ADeclarationReportedWithoutTheKeyReadsAsNotDeclared(t *testing.T) {
	l, reported, err := Decode(json.RawMessage(`[{"name":"live","match":{"tags":["x"]}},{"name":"lab","match":{"tags":["y"]}}]`))
	if err != nil || !reported || len(l) != 2 {
		t.Fatalf("Decode = %+v %v %v", l, reported, err)
	}
	for _, tt := range l {
		if tt.NeverLoadTested() || tt.LoadTest != "" {
			t.Errorf("%s read as load_test=%q from a wire form without the key", tt.Name, tt.LoadTest)
		}
	}
}

func TestDecode_RefusesAWireValueOtherThanNever(t *testing.T) {
	if _, _, err := Decode(json.RawMessage(`[{"name":"live","match":{"tags":["x"]},"load_test":"sometimes"}]`)); err == nil || !strings.Contains(err.Error(), "load_test") {
		t.Fatalf("a wire load_test other than never was stored: %v", err)
	}
}

func TestByName(t *testing.T) {
	l := List{neverLive(), lab()}
	if got, ok := l.ByName("live"); !ok || got.Name != "live" {
		t.Errorf("ByName(live) = %+v %v", got, ok)
	}
	if _, ok := l.ByName("nope"); ok {
		t.Error("ByName(nope) found something")
	}
}

// `outside_cluster: true` on a test_targets[] entry.

func hosted() Target {
	t := live()
	t.Name = "hosted"
	t.Namespace = ""
	t.OutsideCluster = true
	return t
}

func TestOutsideCluster_AloneIsValid_AbsentIsFalse(t *testing.T) {
	if err := (List{hosted(), lab()}).Validate(); err != nil {
		t.Fatalf("outside_cluster: true refused: %v", err)
	}
	if lab().OutsideCluster {
		t.Error("a target without the key must not be outside the cluster (absent = today's behaviour)")
	}
}

func TestOutsideCluster_RefusedWithANamespace_NamesBothKeys(t *testing.T) {
	tt := hosted()
	tt.Namespace = "msgbus"
	err := (List{tt}).Validate()
	if err == nil {
		t.Fatal("outside_cluster together with namespace was accepted")
	}
	for _, w := range []string{"outside_cluster", "namespace", "hosted"} {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("refusal %q lacks %q", err, w)
		}
	}
}

func TestOutsideCluster_WireRoundTripKeepsTheKey_AndAbsentStaysAbsent(t *testing.T) {
	raw, err := Encode(List{hosted(), lab()})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"outside_cluster":true`) || strings.Count(string(raw), "outside_cluster") != 1 {
		t.Fatalf("the wire form must carry the key on the declaring target only: %s", raw)
	}
	l, reported, err := Decode(raw)
	if err != nil || !reported || len(l) != 2 || !l[0].OutsideCluster || l[1].OutsideCluster {
		t.Fatalf("round trip = %+v %v %v", l, reported, err)
	}
	if _, _, err := Decode(json.RawMessage(`[{"name":"x","namespace":"n","outside_cluster":true,"match":{"tags":["t"]}}]`)); err == nil {
		t.Error("a wire declaration with both keys was stored")
	}
}
