package k8srender

import (
	"strings"
	"testing"
)

// the executor container's memory LIMIT is an onboarding setting. Default 1536Mi (byte-identical
// to every earlier render); a quantity at or above the 384Mi request is rendered as given; anything else is refused
// before a byte is written.

func execResources(t *testing.T, manifest string) (requests, limits map[string]any) {
	t.Helper()
	d := obsDoc(t, manifest, "Deployment", "executor")
	res, _ := containerOf(d)["resources"].(map[string]any)
	requests, _ = res["requests"].(map[string]any)
	limits, _ = res["limits"].(map[string]any)
	return requests, limits
}

func TestRenderExecutor_MemoryLimitDefaultsTo1536Mi(t *testing.T) {
	got, err := RenderExecutor(sampleInstance())
	if err != nil {
		t.Fatal(err)
	}
	req, lim := execResources(t, got)
	if lim["memory"] != "1536Mi" || req["memory"] != "384Mi" || lim["cpu"] != "1" || req["cpu"] != "150m" {
		t.Fatalf("default resources: requests=%v limits=%v, want 384Mi/1536Mi and the unchanged cpu", req, lim)
	}
}

func TestRenderExecutor_ExecutorMemorySetsTheLimitOnly(t *testing.T) {
	for _, v := range []string{"3Gi", "3072Mi", "4G", "2147483648", "1536Mi", "384Mi", " 3Gi ", "1.5Gi", "3e9", "+3Gi"} {
		in := sampleInstance()
		in.ExecutorMemory = v
		got, err := RenderExecutor(in)
		if err != nil {
			t.Fatalf("%q: %v", v, err)
		}
		req, lim := execResources(t, got)
		if want := strings.TrimSpace(v); lim["memory"] != want {
			t.Errorf("%q: limit = %v (%T), want the string %s", v, lim["memory"], lim["memory"], want)
		}
		if req["memory"] != "384Mi" {
			t.Errorf("%q: the request must stay 384Mi, got %v", v, req["memory"])
		}
	}
	in := sampleInstance()
	in.ExecutorMemory = "3Gi"
	got, _ := RenderExecutor(in)
	if _, lim := execResources(t, got); lim["memory"] != "3Gi" {
		t.Fatalf("limit = %v, want 3Gi", lim["memory"])
	}
}

// With the setting absent or empty the render is byte-identical.
func TestRenderExecutor_ExecutorMemoryEmptyIsByteIdentical(t *testing.T) {
	a, err := RenderExecutor(sampleInstance())
	if err != nil {
		t.Fatal(err)
	}
	in := sampleInstance()
	in.ExecutorMemory = "  "
	b, err := RenderExecutor(in)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatal("a blank --executor-memory changed the render")
	}
	in.ExecutorMemory = "1536Mi"
	c, _ := RenderExecutor(in)
	if a != c {
		t.Fatal("--executor-memory 1536Mi (the default spelled out) must render exactly the default")
	}
}

func TestRenderExecutor_ExecutorMemoryRefusals(t *testing.T) {
	cases := []struct{ v, want string }{
		{"lots", "not a Kubernetes quantity"},
		{"3 Gi", "not a Kubernetes quantity"},
		{"3GiB", "not a Kubernetes quantity"},
		{"3gi", "not a Kubernetes quantity"},
		{"K", "not a Kubernetes quantity"},
		{"-3Gi", "not a Kubernetes quantity"},
		{"3Gi;rm", "not a Kubernetes quantity"},
		{"3\"}\n  evil: x", "not a Kubernetes quantity"},
		{"1e99", "exponent"},
		{"0", "below the executor's memory request"},
		{"383Mi", "below the executor's memory request"},
		{"100M", "below the executor's memory request"},    // 100,000,000 B < 384Mi
		{"400000k", "below the executor's memory request"}, // 400,000,000 B < 402,653,184 B
		{"1Ki", "below the executor's memory request"},
	}
	for _, c := range cases {
		in := sampleInstance()
		in.ExecutorMemory = c.v
		got, err := RenderExecutor(in)
		if err == nil {
			t.Errorf("%q: want a refusal, rendered %d bytes", c.v, len(got))
			continue
		}
		if !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), "--executor-memory") {
			t.Errorf("%q: error %q must name --executor-memory and say %q", c.v, err, c.want)
		}
		if got != "" {
			t.Errorf("%q: a refusal must write nothing, got %d bytes", c.v, len(got))
		}
	}
}

// The floor is the request, exactly: 384Mi itself is allowed; one byte less is not.
func TestValidateExecutorMemory_TheFloorIsTheRequestExactly(t *testing.T) {
	if err := ValidateExecutorMemory("402653184"); err != nil { // 384Mi in bytes
		t.Fatalf("384Mi spelled in bytes: %v", err)
	}
	if err := ValidateExecutorMemory("402653183"); err == nil {
		t.Fatal("one byte below the request must be refused")
	}
	if err := ValidateExecutorMemory(""); err != nil {
		t.Fatalf("empty means the default: %v", err)
	}
}
