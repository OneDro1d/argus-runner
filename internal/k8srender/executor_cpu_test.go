package k8srender

import (
	"strings"
	"testing"
)

// the executor container's CPU LIMIT is an onboarding setting. Default "1" (byte-identical to every
// earlier render); a CPU quantity at or above the request is rendered as given; anything else is refused before a
// byte is written. The instance quota's limits.cpu follows it (quota_scaling_test.go).

// The constant the floor uses is the request the template actually renders. If the template's request moves and the
// constant does not, the floor would refuse or admit the wrong values: this is the test that catches it.
func TestExecutorCPU_RequestConstantMatchesTheTemplate(t *testing.T) {
	got, err := RenderExecutor(sampleInstance())
	if err != nil {
		t.Fatal(err)
	}
	req, lim := execResources(t, got)
	if req["cpu"] != ExecutorCPURequest {
		t.Fatalf("the template renders requests.cpu %v but ExecutorCPURequest is %q: the --executor-cpu floor would be wrong", req["cpu"], ExecutorCPURequest)
	}
	if lim["cpu"] != DefaultExecutorCPULimit {
		t.Fatalf("the template renders limits.cpu %v but DefaultExecutorCPULimit is %q", lim["cpu"], DefaultExecutorCPULimit)
	}
}

func TestRenderExecutor_ExecutorCPUSetsTheLimitOnly(t *testing.T) {
	for _, v := range []string{"2", "1.5", "0.5", ".5", "1500m", "150m", "0.15", "4", "2.250", " 2 ", "16"} {
		in := sampleInstance()
		in.ExecutorCPU = v
		got, err := RenderExecutor(in)
		if err != nil {
			t.Fatalf("%q: %v", v, err)
		}
		req, lim := execResources(t, got)
		if want := strings.TrimSpace(v); lim["cpu"] != want {
			t.Errorf("%q: limit = %v (%T), want the string %s", v, lim["cpu"], lim["cpu"], want)
		}
		if req["cpu"] != ExecutorCPURequest || req["memory"] != "384Mi" {
			t.Errorf("%q: the requests must stay %s / 384Mi, got %v", v, ExecutorCPURequest, req)
		}
		if lim["memory"] != "1536Mi" {
			t.Errorf("%q: the memory limit must be untouched, got %v", v, lim["memory"])
		}
	}
}

// With the setting absent or blank the render is byte-identical, and the CPU limit line is exactly the old literal.
func TestRenderExecutor_ExecutorCPUEmptyIsByteIdentical(t *testing.T) {
	a, err := RenderExecutor(sampleInstance())
	if err != nil {
		t.Fatal(err)
	}
	const oldLine = `            limits:   {cpu: "1", memory: "1536Mi"}     # JMeter (JVM) headroom`
	if strings.Count(a, oldLine) != 1 {
		t.Fatalf("the default executor resources line must be exactly the pre-setting literal %q", oldLine)
	}
	for _, v := range []string{"  ", "1"} {
		in := sampleInstance()
		in.ExecutorCPU = v
		b, err := RenderExecutor(in)
		if err != nil {
			t.Fatal(err)
		}
		if a != b {
			t.Fatalf("--executor-cpu %q (the default, or blank) changed the render", v)
		}
	}
	// and a real value changes the one line, nothing else (the quota is sized from it: that is the second line)
	in := sampleInstance()
	in.ExecutorCPU = "2"
	b, _ := RenderExecutor(in)
	diff := 0
	al, bl := strings.Split(a, "\n"), strings.Split(b, "\n")
	if len(al) != len(bl) {
		t.Fatalf("--executor-cpu 2 changed the line count: %d vs %d", len(al), len(bl))
	}
	for i := range al {
		if al[i] != bl[i] {
			diff++
		}
	}
	if diff != 2 {
		t.Errorf("--executor-cpu 2 must change exactly two lines (the executor's limit and the quota's limits.cpu), changed %d", diff)
	}
}

func TestRenderExecutor_ExecutorCPURefusals(t *testing.T) {
	cases := []struct{ v, want string }{
		{"lots", "not a Kubernetes CPU quantity"},
		{"2 cpu", "not a Kubernetes CPU quantity"},
		{"2c", "not a Kubernetes CPU quantity"},
		{"2M", "not a Kubernetes CPU quantity"},
		{"2Gi", "not a Kubernetes CPU quantity"},
		{"m", "not a Kubernetes CPU quantity"},
		{".", "not a Kubernetes CPU quantity"},
		{"2.", "not a Kubernetes CPU quantity"},
		{"-1", "not a Kubernetes CPU quantity"},
		{"+2", "not a Kubernetes CPU quantity"},
		{"1e3", "not a Kubernetes CPU quantity"},
		{"1.5m", "not a Kubernetes CPU quantity"},
		{"0.1234", "not a Kubernetes CPU quantity"},
		{"2,5", "not a Kubernetes CPU quantity"},
		{"99999999999999999999", "not a Kubernetes CPU quantity"},
		{"1000000", "not a Kubernetes CPU quantity"},
		{"1000000000m", "not a Kubernetes CPU quantity"},
		{"2;rm", "not a Kubernetes CPU quantity"},
		{"2\"}\n  evil: x", "not a Kubernetes CPU quantity"},
		{"0", "below the executor's CPU request"},
		{"0.1", "below the executor's CPU request"},
		{"0.149", "below the executor's CPU request"},
		{"149m", "below the executor's CPU request"},
		{"0m", "below the executor's CPU request"},
	}
	for _, c := range cases {
		in := sampleInstance()
		in.ExecutorCPU = c.v
		got, err := RenderExecutor(in)
		if err == nil {
			t.Errorf("%q: want a refusal, rendered %d bytes", c.v, len(got))
			continue
		}
		if !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), "--executor-cpu") {
			t.Errorf("%q: error %q must name --executor-cpu and say %q", c.v, err, c.want)
		}
		if got != "" {
			t.Errorf("%q: a refusal must write nothing, got %d bytes", c.v, len(got))
		}
	}
}

// The floor is the request, exactly, read from what the template renders: the request itself is allowed, one
// millicore less is not.
func TestValidateExecutorCPU_TheFloorIsTheRenderedRequestExactly(t *testing.T) {
	out, err := RenderExecutor(sampleInstance())
	if err != nil {
		t.Fatal(err)
	}
	req, _ := execResources(t, out)
	reqStr, _ := req["cpu"].(string)
	reqMilli, err := parseCPUQuantity(reqStr)
	if err != nil {
		t.Fatalf("the rendered request %q does not parse: %v", reqStr, err)
	}
	if err := ValidateExecutorCPU(reqStr); err != nil {
		t.Fatalf("the request itself (%s) must be allowed: %v", reqStr, err)
	}
	below := strings.Replace(reqStr, "150", "149", 1)
	if m, _ := parseCPUQuantity(below); m != reqMilli-1 {
		t.Fatalf("test setup: %q is not one millicore below %q", below, reqStr)
	}
	if err := ValidateExecutorCPU(below); err == nil {
		t.Fatalf("one millicore below the request (%s) must be refused", below)
	}
	if err := ValidateExecutorCPU(""); err != nil {
		t.Fatalf("empty means the default: %v", err)
	}
}

func TestParseCPUQuantity_Millicores(t *testing.T) {
	for in, want := range map[string]int64{
		"1": 1000, "2": 2000, "1.5": 1500, "0.5": 500, ".5": 500, "0.15": 150, "2.250": 2250, "0.001": 1,
		"1500m": 1500, "150m": 150, "1000m": 1000, "007": 7000,
	} {
		got, err := parseCPUQuantity(in)
		if err != nil || got != want {
			t.Errorf("parseCPUQuantity(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
}
