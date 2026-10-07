package runner

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/registryhost"
)

// V19-007 wiring tests: prove the guard is exercised on the REAL ImageUpdate path — through a real
// Deployment read (its containers, its imagePullSecrets, and its
// registryhost.PullSecretRegistriesAnnotation annotation) — not only through the pure
// guardImageObtainable function above. This is the same seam internal/envcapture/client.go uses for
// the identical problem (an httptest.Server has no ServiceAccount to read from disk):
// newK8sImageUpdateFunc takes the connection and the patch call as injectable parameters, and
// NewK8sImageUpdate (the production entry point) is the only caller that leaves both nil so each
// request resolves a fresh in-cluster connection and calls the real patch.
//
// V19-007 withdrew V19-006's Secret read entirely — these tests fetch NOTHING from a
// `/api/v1/namespaces/.../secrets/...` path; fakeImageUpdateServer below serves only the Deployment
// GET/PATCH. A test that still needed a Secret endpoint would itself be proof the guard regressed
// back to reading one.

// fakeImageUpdateServer serves the ONE k8s API endpoint newK8sImageUpdateFunc's closure hits: GET/
// PATCH the executor Deployment, carrying `pullSecretNames` as its imagePullSecrets and
// `annotationValue` as its registryhost.PullSecretRegistriesAnnotation annotation (omitted entirely
// when ""). patchCalled reports whether the PATCH ever landed, so a refusal test can assert the
// running executor was NEVER touched.
func fakeImageUpdateServer(t *testing.T, ns, deployment, currentImage string, pullSecretNames []string, annotationValue string) (*httptest.Server, *bool) {
	t.Helper()
	patchCalled := false
	depPath := fmt.Sprintf("/apis/apps/v1/namespaces/%s/deployments/%s", ns, deployment)
	mux := http.NewServeMux()
	mux.HandleFunc(depPath, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			names := ""
			for i, n := range pullSecretNames {
				if i > 0 {
					names += ","
				}
				names += fmt.Sprintf(`{"name":%q}`, n)
			}
			metaJSON := "{}"
			if annotationValue != "" {
				metaJSON = fmt.Sprintf(`{"annotations":{%q:%q}}`, registryhost.PullSecretRegistriesAnnotation, annotationValue)
			}
			fmt.Fprintf(w, `{"metadata":%s,"spec":{"template":{"spec":{"containers":[{"name":"executor","image":%q}],"imagePullSecrets":[%s]}}}}`,
				metaJSON, currentImage, names)
		case http.MethodPatch:
			patchCalled = true
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `{}`)
		default:
			t.Fatalf("unexpected method %s on %s", r.Method, r.URL.Path)
		}
	})
	// V19-007 REGRESSION GUARD: if the guard ever reads a Secret again, this 500s loudly instead of
	// silently answering "not found" (which pullSecretCoverage's old design would have treated as "no
	// coverage" and refused anyway, masking the regression as a passing "refuses" test).
	mux.HandleFunc("/api/v1/namespaces/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("guard fetched a Secret over the API (%s) — V19-007 removed all Secret reads", r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &patchCalled
}

// fakePatch routes the closure's final patch call at the SAME fake server, over real HTTP — never at
// the production patchDeploymentImage (which resolves its own in-cluster connection independently of
// the injected conn and would, on a machine with a real mounted ServiceAccount, reach an ACTUAL
// cluster). Every wiring test below passes this explicitly: the "refuses" tests never reach it
// because the guard stops them first, and the "allows" test needs the patch to land somewhere that
// is not a live cluster.
func fakePatch(srv *httptest.Server) func(ctx context.Context, namespace, deployment, image string) error {
	return func(ctx context.Context, namespace, deployment, image string) error {
		path := fmt.Sprintf("/apis/apps/v1/namespaces/%s/deployments/%s", namespace, deployment)
		req, err := http.NewRequestWithContext(ctx, http.MethodPatch, srv.URL+path, nil)
		if err != nil {
			return err
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("fake patch returned %d", resp.StatusCode)
		}
		return nil
	}
}

// THE CENTRAL SAFETY PROPERTY, wired end-to-end: an executor onboarded against the private registry only, asked (by
// the control plane, through the real ImageUpdate path) to move to the released GHCR image, must be
// REFUSED before the Deployment is ever patched — proving the fix reads the real Deployment (its
// imagePullSecrets AND its annotation) over HTTP and the refusal happens BEFORE any termination, not
// just in the pure function. No pull-secret-registries annotation was ever recorded here.
func TestWiring_ImageUpdate_RefusesCrossRegistryBeforeAnyPatch(t *testing.T) {
	ns, deployment := "argus-inst-wiring", "executor"
	current := "registry.example.com/argus/runner@sha256:ccc"
	requested := "ghcr.io/onedro1d/argus-runner@sha256:ddd"
	srv, patchCalled := fakeImageUpdateServer(t, ns, deployment, current, []string{"acr-pull"}, "")
	conn := &k8sAPIConn{baseURL: srv.URL, token: "fake-token", hc: srv.Client()}
	fn := newK8sImageUpdateFunc(conn, fakePatch(srv), ns, deployment, nil)

	err := fn(context.Background(), requested)
	if err == nil {
		t.Fatal("wiring allowed a cross-registry re-image with no pull-secret-registries annotation recorded")
	}
	for _, want := range []string{"registry.example.com", "ghcr.io"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal missing %q: %q", want, err.Error())
		}
	}
	if *patchCalled {
		t.Fatal("the Deployment was PATCHED despite the guard refusing — this is the exact outage " +
			"the guard exists to prevent (maxSurge:0 terminates the running pod first)")
	}
}

// The allowed half of the same wiring: a Deployment whose own annotation records the requested
// registry as covered lets the update proceed all the way to the (fake) patch.
func TestWiring_ImageUpdate_AllowsCrossRegistryWhenTheAnnotationCoversIt(t *testing.T) {
	ns, deployment := "argus-inst-wiring2", "executor"
	current := "registry.example.com/argus/runner@sha256:ccc"
	requested := "ghcr.io/onedro1d/argus-runner@sha256:ddd"
	srv, patchCalled := fakeImageUpdateServer(t, ns, deployment, current, []string{"multi-pull"},
		"registry.example.com,ghcr.io") // the operator ran the guard's own remedy and recorded both
	conn := &k8sAPIConn{baseURL: srv.URL, token: "fake-token", hc: srv.Client()}
	fn := newK8sImageUpdateFunc(conn, fakePatch(srv), ns, deployment, nil)

	if err := fn(context.Background(), requested); err != nil {
		t.Fatalf("wiring refused a re-image the Deployment's own annotation covers: %v", err)
	}
	if !*patchCalled {
		t.Fatal("guard allowed the update but the Deployment was never patched")
	}
}

// An annotation that records a THIRD registry (neither the one currently running nor the one
// requested) must refuse exactly like no annotation at all — real, present content that simply does
// not vouch for the requested host, not a read failure and not "some coverage exists so allow".
func TestWiring_ImageUpdate_AnnotationForAThirdRegistryRefuses(t *testing.T) {
	ns, deployment := "argus-inst-wiring3", "executor"
	current := "registry.example.com/argus/runner@sha256:ccc"
	requested := "ghcr.io/onedro1d/argus-runner@sha256:ddd"
	srv, patchCalled := fakeImageUpdateServer(t, ns, deployment, current, []string{"quay-pull"}, "quay.io")
	conn := &k8sAPIConn{baseURL: srv.URL, token: "fake-token", hc: srv.Client()}
	fn := newK8sImageUpdateFunc(conn, fakePatch(srv), ns, deployment, nil)

	err := fn(context.Background(), requested)
	if err == nil {
		t.Fatal("an annotation covering an unrelated registry was treated as covering the requested one")
	}
	if *patchCalled {
		t.Fatal("patched despite the Deployment's annotation not covering the requested registry")
	}
}

// Same-registry updates (a digest bump within GHCR, the common case) stay allowed with only a pull
// secret referenced — no annotation needed — proving V19-007 did not narrow the case V19-006 already
// proved safe.
func TestWiring_ImageUpdate_AllowsSameRegistryWithNoAnnotation(t *testing.T) {
	ns, deployment := "argus-inst-wiring4", "executor"
	current := "ghcr.io/onedro1d/argus-runner@sha256:aaa"
	requested := "ghcr.io/onedro1d/argus-runner@sha256:bbb"
	srv, patchCalled := fakeImageUpdateServer(t, ns, deployment, current, []string{"ghcr-pull"}, "")
	conn := &k8sAPIConn{baseURL: srv.URL, token: "fake-token", hc: srv.Client()}
	fn := newK8sImageUpdateFunc(conn, fakePatch(srv), ns, deployment, nil)

	if err := fn(context.Background(), requested); err != nil {
		t.Fatalf("a same-registry update with a pull secret referenced was refused: %v", err)
	}
	if !*patchCalled {
		t.Fatal("guard allowed the update but the Deployment was never patched")
	}
}
