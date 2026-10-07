package chain

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/compare"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// ARGUS-CMP-3: an http step records the response it JUDGED, only when asked.

func bodySpec() compare.Spec { return compare.Spec{Status: true, Body: true} }

func TestHTTPStepWithOutput_NilCaptureIsHTTPStepExactly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"a":1}`)) }))
	defer srv.Close()
	plain := HTTPStep("s", "GET", srv.URL, nil, "", 200, nil, nil, nil, false, nil, nil)
	with := HTTPStepWithOutput("s", "GET", srv.URL, nil, "", 200, nil, nil, nil, false, nil, nil, nil)
	a, b := plain.Run("c", map[string]string{}), with.Run("c", map[string]string{})
	if a.Status != b.Status || a.Observed != b.Observed || a.Output != nil || b.Output != nil {
		t.Fatalf("plain %+v with-nil %+v", a, b)
	}
}

func TestHTTPStepWithOutput_RecordsTheJudgedResponseWithSavedValuesScrubbed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"gen-id-123456","msg":"hi gen-id-123456"}`))
	}))
	defer srv.Close()
	save := map[string]scenario.SaveSpec{"gid": {Path: "id"}}
	oc := &OutputCapture{Spec: bodySpec(), Scrub: func(s string) string { return strings.ReplaceAll(s, "hi", "HELLO") }}
	st := HTTPStepWithOutput("create", "POST", srv.URL, nil, "", 200, nil, save, nil, false, nil, nil, oc)
	res := st.Run("c", map[string]string{})
	if res.Status != "passed" || res.Output == nil {
		t.Fatalf("step %+v", res)
	}
	file := string(res.Output.Stored.File())
	if strings.Contains(file, "gen-id-123456") || !strings.Contains(file, "${saved.gid}") {
		t.Errorf("the step's own saved value must be put back as its placeholder: %s", file)
	}
	if !strings.Contains(file, "HELLO") {
		t.Errorf("the caller's scrub was not applied: %s", file)
	}
	if res.Output.Record.Step != "create" || res.Output.Record.Sample != 1 || res.Output.Record.State != compare.StateRecorded {
		t.Errorf("record %+v", res.Output.Record)
	}
}

func TestHTTPStepWithOutput_BodyOverTheCapIsNotRecordedButStillJudgedAsBefore(t *testing.T) {
	big := strings.Repeat("x", compare.MaxBodyBytes+1)
	exact := strings.Repeat("y", compare.MaxBodyBytes)
	for _, tc := range []struct {
		name, body string
		state      string
		reason     string
	}{
		{"over the cap", big, compare.StateNotRecorded, compare.ReasonBodyTooLarge},
		{"exactly the cap", exact, compare.StateRecorded, ""},
	} {
		body := tc.body
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }))
		oc := &OutputCapture{Spec: bodySpec()}
		res := HTTPStepWithOutput("s", "GET", srv.URL, nil, "", 200, nil, nil, nil, false, nil, nil, oc).Run("c", map[string]string{})
		srv.Close()
		if res.Status != "passed" || res.Output == nil || res.Output.Record.State != tc.state || res.Output.Record.Reason != tc.reason {
			t.Errorf("%s: step %q record %+v", tc.name, res.Status, res.Output)
		}
		if tc.state == compare.StateNotRecorded && res.Output != nil && res.Output.Stored != nil {
			t.Errorf("%s: a not-recorded output must carry no stored body", tc.name)
		}
	}
}
