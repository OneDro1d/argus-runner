package envcapture

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func podJSON(name, phase string, deleting bool, statuses ...map[string]any) map[string]any {
	meta := map[string]any{"name": name}
	if deleting {
		meta["deletionTimestamp"] = "2026-09-30T10:00:00Z"
	}
	return map[string]any{
		"metadata": meta,
		"status":   map[string]any{"phase": phase, "containerStatuses": statuses},
	}
}

func cstatus(name, image, imageID string) map[string]any {
	return map[string]any{"name": name, "image": image, "imageID": imageID}
}

func TestReadRunningContainers_ReturnsOnlyLiveRegularContainers(t *testing.T) {
	cl := fakeCluster(t, jsonHandler(t, map[string]any{
		"/api/v1/namespaces/app/pods": map[string]any{"items": []any{
			podJSON("web-1", "Running", false, cstatus("web", "r/web:1", "r/web@sha256:aa"), cstatus("side", "r/side:1", "")),
			podJSON("web-old", "Running", true, cstatus("web", "r/web:0", "r/web@sha256:00")),  // terminating
			podJSON("job-1", "Succeeded", false, cstatus("job", "r/job:1", "r/job@sha256:11")), // finished
			podJSON("pend-1", "Pending", false),
		}},
	}, nil))
	got, reason := ReadRunningContainers(context.Background(), cl, "app")
	if reason != "" {
		t.Fatalf("reason %q, want a reading", reason)
	}
	if len(got) != 2 {
		t.Fatalf("got %+v, want exactly the two containers of the one live Running pod", got)
	}
	if got[0].Pod != "web-1" || got[0].ImageID != "r/web@sha256:aa" || got[1].Container != "side" || got[1].ImageID != "" {
		t.Fatalf("unexpected containers: %+v", got)
	}
}

func TestReadRunningContainers_ForbiddenIsAReasonNotAnEmptyReading(t *testing.T) {
	cl := fakeCluster(t, jsonHandler(t, nil, map[string]bool{"/api/v1/namespaces/app/pods": true}))
	got, reason := ReadRunningContainers(context.Background(), cl, "app")
	if len(got) != 0 || !strings.Contains(reason, "forbidden") || !strings.Contains(reason, "app") {
		t.Fatalf("got %v / %q, want no containers and a forbidden reason naming the namespace", got, reason)
	}
}

func TestReadRunningContainers_NoClientNoNamespaceNoPods(t *testing.T) {
	if _, r := ReadRunningContainers(context.Background(), nil, "app"); r == "" {
		t.Error("a nil client must give a reason")
	}
	cl := fakeCluster(t, func(http.ResponseWriter, *http.Request) {})
	if _, r := ReadRunningContainers(context.Background(), cl, ""); r == "" {
		t.Error("no namespace must give a reason")
	}
	empty := fakeCluster(t, jsonHandler(t, map[string]any{"/api/v1/namespaces/app/pods": map[string]any{"items": []any{}}}, nil))
	if cs, r := ReadRunningContainers(context.Background(), empty, "app"); len(cs) != 0 || !strings.Contains(r, "no running pod") {
		t.Errorf("an empty namespace = %v / %q, want a 'no running pod' reason", cs, r)
	}
}
