package k8supgrade

import (
	"strings"
	"testing"
)

// The restart line is for EVERY workload the kit renders, not only the executor: a loki or pushgateway
// Deployment and the promtail DaemonSet roll their pods on a pod-template change just the same. Only the
// executor gets the clause about a run in flight. (Restricting the line to the executor left every other
// test green, so this pins it.)
func TestRestartNote_EveryWorkloadNotOnlyTheExecutor(t *testing.T) {
	cs := []Change{{Path: "spec.template.spec.containers[loki].args", Live: []any{"a"}, Desired: []any{"b"}}}
	for _, o := range []Object{
		{APIVersion: "apps/v1", Kind: "Deployment", Name: "loki"},
		{APIVersion: "apps/v1", Kind: "Deployment", Name: "pushgateway"},
		{APIVersion: "apps/v1", Kind: "DaemonSet", Name: "promtail"},
		{APIVersion: "apps/v1", Kind: "StatefulSet", Name: "anything"},
	} {
		note := RestartNote(o, cs, false)
		if !strings.Contains(note, "POD RESTART") || !strings.Contains(note, o.Ref()) || !strings.Contains(note, cs[0].Path) {
			t.Errorf("%s: note = %q; want a restart line naming the object and the template path", o.Ref(), note)
		}
		if strings.Contains(note, "a run in flight") {
			t.Errorf("%s: note = %q; the run-in-flight clause belongs to the executor only", o.Ref(), note)
		}
	}
	ex := Object{APIVersion: "apps/v1", Kind: "Deployment", Name: ExecutorName}
	if note := RestartNote(ex, cs, false); !strings.Contains(note, "a run in flight is cut") {
		t.Errorf("executor: note = %q; want the run-in-flight clause", note)
	}
}
