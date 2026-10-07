package argus

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/config"
)

// a load user is a persistent client. The templates read
// http.keepalive on EVERY HTTP sampler, DeriveProps sets it only for a `## LOAD` check, and no template
// resets the HTTP state per iteration (ThreadGroup.same_user_on_next_iteration=false would).
//
// The templates are found by globbing templates/*.jmx, never from a fixed list, so a template added later
// that has an HTTP sampler cannot be missed (the first cut listed two of the six).
func TestHTTPTemplatesReadTheKeepAliveProperty(t *testing.T) {
	samplerRe := regexp.MustCompile(`<HTTPSamplerProxy `)
	kaRe := regexp.MustCompile(`<stringProp name="HTTPSampler\.use_keepalive">\$\{__P\(http\.keepalive,false\)\}</stringProp>`)
	files, err := filepath.Glob(filepath.Join("..", "..", "templates", "*.jmx"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no templates found (err=%v)", err)
	}
	withHTTP := 0
	for _, f := range files {
		name := filepath.Base(f)
		jmx := readTemplate(t, name)
		samplers, ka := len(samplerRe.FindAllString(jmx, -1)), len(kaRe.FindAllString(jmx, -1))
		if samplers > 0 {
			withHTTP++
		}
		if ka != samplers {
			t.Errorf("%s: %d HTTP samplers but %d read http.keepalive (default false, a stringProp: JMeter evaluates no function in a boolProp)", name, samplers, ka)
		}
		if strings.Contains(jmx, "same_user_on_next_iteration") {
			t.Errorf("%s sets same_user_on_next_iteration: a load user must keep its client state across loops (JMeter's default)", name)
		}
	}
	if withHTTP < 6 {
		t.Errorf("only %d templates with an HTTP sampler were discovered; the glob is not finding templates/*.jmx", withHTTP)
	}
}

func TestDeriveProps_LoadProfileKeepsTheConnectionAndNoLoadDoesNot(t *testing.T) {
	c := &config.Config{}
	c.Targets.HTTP = &config.HTTPTarget{BaseURL: "http://sut.invalid"}
	body := "- **Users**: 5\n- **Ramp Seconds**: 0\n- **Duration Seconds**: 3\n- **Target P95 Ms**: 300\n- **Max Error Rate**: 0.05"
	props, err := DeriveProps(c, minimalLoadScenario(t, body), "tr-t")
	if err != nil {
		t.Fatal(err)
	}
	if props["http.keepalive"] != "true" {
		t.Errorf("a ## LOAD check must set http.keepalive=true, got %q", props["http.keepalive"])
	}
	props, err = DeriveProps(c, minimalLoadScenario(t, ""), "tr-t")
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := props["http.keepalive"]; ok {
		t.Errorf("a check without ## LOAD must emit no http.keepalive (its wire stays as it was), got %q", v)
	}
}

// jtlRunner writes a .jtl of raw rows: "<ts>,<elapsed>,<code>,<message>".
type jtlRunner struct{ rows [][4]string }

func (f *jtlRunner) Run(templateBase, jtlPath string, props map[string]string, _ time.Duration) error {
	var b strings.Builder
	b.WriteString("timeStamp,elapsed,label,responseCode,responseMessage,threadName,success,failureMessage\n")
	for i, r := range f.rows {
		fmt.Fprintf(&b, "%s,%s,%s,%s,%s,t 1-%d,false,\n", r[0], r[1], props["scenario.id"], r[2], r[3], i+1)
	}
	return os.WriteFile(jtlPath, []byte(b.String()), 0o644)
}

func TestRunAll_LoadRecordCarriesTransportErrorsTimelineAndTheTopReasonInTheBreach(t *testing.T) {
	dir := t.TempDir()
	scDir := filepath.Join(dir, "scenarios")
	writeScenarioMD(t, scDir, "http-ingestion", "LOAD-ERR", loadScenarioMD("LOAD-ERR", loadBodyGenerous))
	secret := "tok-9f8e7d6c5b4a-SECRET"
	rows := [][4]string{{"1781024939000", "20", "202", "Accepted"}}
	for i := 0; i < 9; i++ {
		rows = append(rows, [4]string{fmt.Sprint(1781024940000 + int64(i)*1000), "60030",
			"Non HTTP response code: java.net.SocketException", "Non HTTP response message: Network is unreachable via " + secret})
	}
	c := &config.Config{}
	c.Project.Name = "order-service"
	c.Targets.HTTP = &config.HTTPTarget{BaseURL: "http://order-api:8080"}
	c.Targets.Auth = &config.AuthTarget{Type: "bearer", BearerToken: secret}

	rr, err := RunAll(c, scDir, filepath.Join(dir, "results"), "order-service", "", "", "", "", &jtlRunner{rows: rows})
	if err != nil {
		t.Fatal(err)
	}
	res, _ := rr.Report.Find("LOAD-ERR")
	if res == nil || res.Load == nil {
		t.Fatalf("no load record: %+v", res)
	}
	if len(res.Load.Errors) != 1 || res.Load.Errors[0].Count != 9 ||
		!strings.HasPrefix(res.Load.Errors[0].Reason, "java.net.SocketException: Network is unreachable") {
		t.Fatalf("errors = %+v", res.Load.Errors)
	}
	if len(res.Load.Timeline) < 2 || res.Load.TimelineBucketS < 1 {
		t.Errorf("timeline = %d buckets of %ds", len(res.Load.Timeline), res.Load.TimelineBucketS)
	}
	if res.Status != "failed" || res.Failure == nil || !strings.Contains(res.Failure.Observed, "load target breached") ||
		!strings.Contains(res.Failure.Observed, "java.net.SocketException: Network is unreachable") {
		t.Errorf("the breach sentence must name the top reason: %+v", res.Failure)
	}
	b, _ := json.Marshal(res)
	if strings.Contains(string(b), secret) {
		t.Errorf("a configured credential reached the report row: %s", b)
	}
}

func TestRunAll_LoadRecordWithoutTransportFailuresHasNoErrors(t *testing.T) {
	dir := t.TempDir()
	scDir := filepath.Join(dir, "scenarios")
	writeScenarioMD(t, scDir, "http-ingestion", "LOAD-OK", loadScenarioMD("LOAD-OK", loadBodyGenerous))
	var rows [][4]string
	for i := 0; i < 5; i++ {
		rows = append(rows, [4]string{fmt.Sprint(1781024940000 + int64(i)*100), "10", "202", "Accepted"})
	}
	c := &config.Config{}
	c.Project.Name = "order-service"
	c.Targets.HTTP = &config.HTTPTarget{BaseURL: "http://order-api:8080"}
	rr, err := RunAll(c, scDir, filepath.Join(dir, "results"), "order-service", "", "", "", "", &jtlRunner{rows: rows})
	if err != nil {
		t.Fatal(err)
	}
	res, _ := rr.Report.Find("LOAD-OK")
	if res == nil || res.Load == nil || res.Load.Errors != nil || res.Load.Samples != 5 {
		t.Fatalf("load = %+v", res)
	}
}
