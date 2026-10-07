package artifactmeasure

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/envcapture"
)

func k8sClient(t *testing.T, status int, pods []any) *envcapture.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		if r.URL.Path != "/api/v1/namespaces/sut/pods" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": pods})
	}))
	t.Cleanup(srv.Close)
	return envcapture.NewClient(srv.URL, "fake-token", srv.Client())
}

func pod(name string, statuses ...map[string]any) map[string]any {
	return map[string]any{
		"metadata": map[string]any{"name": name},
		"status":   map[string]any{"phase": "Running", "containerStatuses": statuses},
	}
}

func cs(name, imageID string) map[string]any { return map[string]any{"name": name, "imageID": imageID} }

func TestK8s_ReadsTheDigestsOfTheContainersActuallyRunning(t *testing.T) {
	cl := k8sClient(t, 200, []any{
		pod("app-1", cs("app", "ghcr.io/x/app@"+dA), cs("proxy", "docker-pullable://envoy@"+dB)),
		pod("app-2", cs("app", "ghcr.io/x/app@"+dA)),
	})
	r := K8s(cl, "sut")(context.Background())
	if r.Reason != "" || r.Unresolved != 0 || r.Source != "k8s" {
		t.Fatalf("reading %+v", r)
	}
	m := Evaluate(dA, nil, r)
	if m.State != StateMatched || len(m.Running) != 2 {
		t.Fatalf("measurement %+v, want matched over 2 distinct digests", m)
	}
}

func TestK8s_TagOnlyImageID_IsUnresolved_NotADigest(t *testing.T) {
	cl := k8sClient(t, 200, []any{pod("app-1", cs("app", "app:latest"), cs("b", "sha256:"+strings.Repeat("d", 64)))})
	r := K8s(cl, "sut")(context.Background())
	if len(r.Digests) != 0 || r.Unresolved != 2 {
		t.Fatalf("reading %+v, want 0 digests and 2 unresolved (a tag and a bare config id are not registry digests)", r)
	}
	m := Evaluate(dA, nil, r)
	if m.State != StateNotMeasured || m.Reason == "" {
		t.Fatalf("measurement %+v, want not_measured with a reason", m)
	}
}

func TestK8s_Forbidden_And_Unreachable_AreNamedReasons(t *testing.T) {
	r := K8s(k8sClient(t, http.StatusForbidden, nil), "sut")(context.Background())
	if !strings.Contains(r.Reason, "forbidden") || len(r.Digests) != 0 {
		t.Fatalf("forbidden reading %+v", r)
	}
	r = K8s(nil, "sut")(context.Background())
	if r.Reason == "" {
		t.Fatalf("no client must give a reason: %+v", r)
	}
}

func TestSystem_TierRouting_AndEveryWayOfNotKnowingWhereTheSUTRuns(t *testing.T) {
	ctx := context.Background()
	noClient := func() (*envcapture.Client, error) { return nil, errors.New("no service account token") }
	cases := map[string]SystemConfig{
		"unknown tier":            {Tier: "", Namespace: "sut", NewClient: noClient},
		"bogus tier":              {Tier: "vm", Namespace: "sut", NewClient: noClient},
		"k3d without a namespace": {Tier: "k3d", NewClient: noClient},
		"k3d without credentials": {Tier: "k3d", Namespace: "sut", NewClient: noClient},
		"managed, no credentials": {Tier: "managed", Namespace: "sut", NewClient: noClient},
		"compose without project": {Tier: "compose", ComposeProject: func() string { return "" }},
		"compose without docker": {Tier: "compose", ComposeProject: func() string { return "shop" },
			Run: func(context.Context, string, ...string) ([]byte, error) {
				return nil, errors.New(`exec: "docker": executable file not found in $PATH`)
			}},
	}
	for name, cfg := range cases {
		r := System(cfg)(ctx)
		if r.Reason == "" || len(r.Digests) != 0 {
			t.Errorf("%s: reading %+v, want a reason and no digests", name, r)
		}
		if m := Evaluate(dA, nil, r); m.State != StateNotMeasured {
			t.Errorf("%s: state %q, want not_measured", name, m.State)
		}
	}
	// the k8s tiers go through the API
	cl := k8sClient(t, 200, []any{pod("a", cs("a", "r/a@"+dA))})
	for _, tier := range []string{"k3d", "managed"} {
		r := System(SystemConfig{Tier: tier, Namespace: "sut", NewClient: func() (*envcapture.Client, error) { return cl, nil }})(ctx)
		if r.Reason != "" || len(r.Digests) != 1 {
			t.Errorf("%s: reading %+v", tier, r)
		}
	}
}

// fakeDocker answers the three docker calls Compose makes, from canned data.
func fakeDocker(ps, inspect string, repoDigests map[string]string) CmdRunner {
	return func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name != "docker" {
			return nil, errors.New("only docker is allowed")
		}
		switch {
		case len(args) > 0 && args[0] == "ps":
			return []byte(ps), nil
		case len(args) > 1 && args[0] == "image" && args[1] == "inspect":
			return []byte(repoDigests[args[len(args)-1]]), nil
		case len(args) > 0 && args[0] == "inspect":
			return []byte(inspect), nil
		}
		return nil, errors.New("unexpected docker call: " + strings.Join(args, " "))
	}
}

func TestCompose_ReadsRepoDigestsOfTheProjectsContainers(t *testing.T) {
	run := fakeDocker("c1\nc2\n", "sha256:img1\nsha256:img2\n", map[string]string{
		"sha256:img1": `["ghcr.io/x/app@` + dA + `"]`,
		"sha256:img2": `["postgres@` + dB + `"]`,
	})
	r := Compose(run, "shop")(context.Background())
	if r.Reason != "" || r.Source != "compose" || r.Unresolved != 0 {
		t.Fatalf("reading %+v", r)
	}
	if m := Evaluate(dB, nil, r); m.State != StateMatched {
		t.Fatalf("measurement %+v", m)
	}
}

func TestCompose_LocallyBuiltImage_HasNoRepoDigest_IsUnresolved(t *testing.T) {
	run := fakeDocker("c1\n", "sha256:img1\n", map[string]string{"sha256:img1": `[]`})
	r := Compose(run, "shop")(context.Background())
	if len(r.Digests) != 0 || r.Unresolved != 1 {
		t.Fatalf("reading %+v, want the locally built image counted unresolved", r)
	}
	if m := Evaluate(dA, nil, r); m.State != StateNotMeasured {
		t.Fatalf("state %q: a locally built image can never prove a mismatch", m.State)
	}
}

func TestCompose_NoContainersForTheProject(t *testing.T) {
	r := Compose(fakeDocker("", "", nil), "shop")(context.Background())
	if r.Reason == "" || !strings.Contains(r.Reason, "shop") {
		t.Fatalf("reading %+v, want a reason naming the project", r)
	}
}
