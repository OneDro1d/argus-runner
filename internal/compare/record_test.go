package compare

import (
	"encoding/json"
	"strings"
	"testing"
)

func okRecord(step string, sample int, hash string) ScenarioOutput {
	return ScenarioOutput{ScenarioID: "S-1", OutputRecord: OutputRecord{
		V: 1, Step: step, Sample: sample, State: StateRecorded, Status: 200,
		Parts: Parts{Status: strings.Repeat("a", 64), Body: strings.Repeat("b", 64)},
		Hash:  hash, BodyKind: KindJSON, BodyBytes: 10,
	}}
}

var h1 = strings.Repeat("1", 64)
var h2 = strings.Repeat("2", 64)

func TestReasonVocabularyIsClosed(t *testing.T) {
	all := AllReasons()
	want := []string{
		ReasonMaskNeedsJSON, ReasonPathRuleNeedsJSON, ReasonBodyTooLarge, ReasonLayerNotSupported,
		ReasonHeaderNotAllowed, ReasonNoResponse, ReasonTooManySamples, ReasonTooManyValues, ReasonLoadNumbersOnly,
	}
	if len(all) != len(want) {
		t.Fatalf("AllReasons() = %v, want %d entries", all, len(want))
	}
	for _, r := range want {
		if !ValidReason(r) {
			t.Errorf("%q must be valid", r)
		}
		if ReasonText(r) == "" {
			t.Errorf("%q has no sentence", r)
		}
	}
	for _, bad := range []string{"", "something else", "body_too_large ", "Body_Too_Large", "larger than 1 MiB"} {
		if ValidReason(bad) {
			t.Errorf("%q must not be a reason", bad)
		}
	}
	if ReasonText("free text") != "" {
		t.Error("an unknown reason has no sentence: nothing is built from a response")
	}
	if ReasonText(ReasonBodyTooLarge) != "larger than 1 MiB" || ReasonText(ReasonMaskNeedsJSON) != "a field mask needs a JSON body" ||
		ReasonText(ReasonLayerNotSupported) != "layer not supported for comparison yet" {
		t.Error("the sentences of the design are pinned")
	}
}

func TestOutputRecordWireShape(t *testing.T) {
	r := okRecord("", 1, h1).OutputRecord
	b, _ := json.Marshal(r)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	for _, k := range []string{"v", "step", "sample", "state", "reason", "status", "parts", "hash", "body_kind", "body_bytes", "truncated", "masks_applied"} {
		if _, ok := m[k]; !ok {
			t.Errorf("record JSON lacks %q: %s", k, b)
		}
	}
	for _, k := range []string{"values", "load"} {
		if _, ok := m[k]; ok {
			t.Errorf("%q is omitempty and must be absent when unset: %s", k, b)
		}
	}
	w, _ := json.Marshal(okRecord("create", 2, h1))
	if !strings.HasPrefix(string(w), `{"scenario_id":"S-1","v":1,`) {
		t.Errorf("the wire row carries scenario_id first, then the record flat: %s", w)
	}
}

func TestOutputsRoot(t *testing.T) {
	a := []ScenarioOutput{okRecord("", 1, h1), {ScenarioID: "S-0", OutputRecord: okRecord("", 1, h2).OutputRecord}}
	b := []ScenarioOutput{a[1], a[0]}
	if OutputsRoot(a) != OutputsRoot(b) || len(OutputsRoot(a)) != 64 {
		t.Fatal("the root is over sorted rows, not arrival order")
	}
	c := []ScenarioOutput{okRecord("", 1, h2), a[1]}
	if OutputsRoot(a) == OutputsRoot(c) {
		t.Fatal("a changed hash must change the root")
	}
	d := []ScenarioOutput{okRecord("x", 1, h1), a[1]}
	e := []ScenarioOutput{okRecord("", 2, h1), a[1]}
	if OutputsRoot(a) == OutputsRoot(d) || OutputsRoot(a) == OutputsRoot(e) {
		t.Fatal("step and sample are committed to")
	}
	// ("a","b1") and ("ab","1") must not collide
	x := ScenarioOutput{ScenarioID: "a", OutputRecord: OutputRecord{Step: "b1", Sample: 1, Hash: h1}}
	y := ScenarioOutput{ScenarioID: "ab", OutputRecord: OutputRecord{Step: "1", Sample: 1, Hash: h1}}
	if OutputsRoot([]ScenarioOutput{x}) == OutputsRoot([]ScenarioOutput{y}) {
		t.Fatal("field boundaries must be unambiguous")
	}
	if OutputsRoot(nil) == "" {
		t.Fatal("empty set has a root")
	}
}

func TestDecodeOutputs_ReencodesOnlyTheTypedRecord(t *testing.T) {
	raw := []byte(`[{"scenario_id":"S-1","v":1,"step":"","sample":1,"state":"recorded","reason":"","status":200,
	 "parts":{"status":"` + strings.Repeat("a", 64) + `","body":"` + strings.Repeat("b", 64) + `"},"hash":"` + h1 + `",
	 "body_kind":"json","body_bytes":5,"truncated":false,"masks_applied":0,
	 "body":"SECRET-BODY","headers":{"x":"SECRET-HEADER"},"claim":"SECRET-CLAIM"}]`)
	outs, norm, err := DecodeOutputs(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(outs) != 1 || outs[0].ScenarioID != "S-1" || outs[0].Hash != h1 {
		t.Fatalf("outs = %+v", outs)
	}
	for _, leak := range []string{"SECRET-BODY", "SECRET-HEADER", "SECRET-CLAIM"} {
		if strings.Contains(string(norm), leak) {
			t.Errorf("the re-encoded outputs still carry %q: %s", leak, norm)
		}
	}
}

func TestDecodeOutputs_RefusesWhatTheTypeCannotSay(t *testing.T) {
	good := okRecord("", 1, h1)
	mut := func(f func(o *ScenarioOutput)) []byte {
		o := good
		f(&o)
		b, _ := json.Marshal([]ScenarioOutput{o})
		return b
	}
	bad := map[string][]byte{
		"version":             mut(func(o *ScenarioOutput) { o.V = 2 }),
		"state":               mut(func(o *ScenarioOutput) { o.State = "maybe" }),
		"free text reason":    mut(func(o *ScenarioOutput) { o.State = StateNotRecorded; o.Hash = ""; o.Reason = "the body was: abc" }),
		"recorded + reason":   mut(func(o *ScenarioOutput) { o.Reason = ReasonBodyTooLarge }),
		"not recorded + hash": mut(func(o *ScenarioOutput) { o.State = StateNotRecorded; o.Reason = ReasonBodyTooLarge }),
		"short hash":          mut(func(o *ScenarioOutput) { o.Hash = "abc" }),
		"upper-case hash":     mut(func(o *ScenarioOutput) { o.Hash = strings.Repeat("A", 64) }),
		"part not hex":        mut(func(o *ScenarioOutput) { o.Parts.Body = "not hex" }),
		"status range":        mut(func(o *ScenarioOutput) { o.Status = 99 }),
		"sample range":        mut(func(o *ScenarioOutput) { o.Sample = MaxSamples + 1 }),
		"body kind":           mut(func(o *ScenarioOutput) { o.BodyKind = "xml" }),
		"empty scenario id":   mut(func(o *ScenarioOutput) { o.ScenarioID = "" }),
		"long step":           mut(func(o *ScenarioOutput) { o.Step = strings.Repeat("s", 129) }),
		"negative bytes":      mut(func(o *ScenarioOutput) { o.BodyBytes = -1 }),
		"not json":            []byte(`{`),
		"not an array":        []byte(`{"a":1}`),
	}
	for name, raw := range bad {
		if _, _, err := DecodeOutputs(raw); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	over := make([]byte, 0, MaxOutputsBytes+10)
	over = append(over, '[')
	for len(over) < MaxOutputsBytes+2 {
		over = append(over, ' ')
	}
	over = append(over, ']')
	if _, _, err := DecodeOutputs(over); err == nil {
		t.Error("an oversize value must be refused")
	}
	for _, r := range AllReasons() {
		raw := mut(func(o *ScenarioOutput) { o.State = StateNotRecorded; o.Hash = ""; o.Parts = Parts{}; o.Reason = r })
		if _, _, err := DecodeOutputs(raw); err != nil {
			t.Errorf("reason %q: %v", r, err)
		}
	}
	if _, _, err := DecodeOutputs([]byte(`[]`)); err != nil {
		t.Errorf("an empty array is valid: %v", err)
	}
}

func TestToleranceValueAndLoadDecode(t *testing.T) {
	o := okRecord("", 1, h1)
	o.Values = []ToleranceValue{{Path: "$.t", Rule: 0, Value: 1.5}}
	o.Load = &LoadNumbers{Samples: 100, P50Ms: 10, P95Ms: 20, P99Ms: 30, ErrorRate: 0.01}
	b, _ := json.Marshal([]ScenarioOutput{o})
	outs, _, err := DecodeOutputs(b)
	if err != nil {
		t.Fatal(err)
	}
	if outs[0].Load == nil || outs[0].Load.P95Ms != 20 || len(outs[0].Values) != 1 {
		t.Fatalf("%+v", outs[0])
	}
	o.Values = make([]ToleranceValue, MaxToleranceValues+1)
	b, _ = json.Marshal([]ScenarioOutput{o})
	if _, _, err := DecodeOutputs(b); err == nil {
		t.Error("more than 64 tolerant values must be refused")
	}
}
