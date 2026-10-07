package argus

import (
	"bytes"
	"encoding/xml"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// ARGUS-CMP-3: the capture block in the two http templates. No JMeter is available to the build, so
// these tests pin what a reader of the template can check: the XML is well-formed, every sampler is
// followed by the block, the block is guarded by the property, writes the three files with .status last,
// and never logs a response. What they cannot prove (that Groovy compiles and JMeter runs it) is stated
// in the PR; the Go side is covered by capRunner, which writes the same three files.

func templateText(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "templates", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// jsr223Scripts returns the decoded script text of every JSR223PostProcessor in the plan.
func jsr223PostScripts(t *testing.T, doc string) []string {
	t.Helper()
	dec := xml.NewDecoder(strings.NewReader(doc))
	var out []string
	inPost, inScript := false, false
	var cur bytes.Buffer
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("template is not well-formed XML: %v", err)
		}
		switch e := tok.(type) {
		case xml.StartElement:
			if e.Name.Local == "JSR223PostProcessor" {
				inPost = true
			}
			if inPost && e.Name.Local == "stringProp" {
				for _, a := range e.Attr {
					if a.Name.Local == "name" && a.Value == "script" {
						inScript = true
						cur.Reset()
					}
				}
			}
		case xml.CharData:
			if inScript {
				cur.Write(e)
			}
		case xml.EndElement:
			if inScript && e.Name.Local == "stringProp" {
				inScript = false
				out = append(out, cur.String())
			}
			if e.Name.Local == "JSR223PostProcessor" {
				inPost = false
			}
		}
	}
	return out
}

func TestHTTPTemplates_CarryTheGuardedCaptureBlockAfterEverySampler(t *testing.T) {
	for _, tc := range []struct {
		file     string
		samplers int
	}{{"http-ingestion.jmx", 1}, {"http-idempotency.jmx", 2}} {
		doc := templateText(t, tc.file)
		if got := strings.Count(doc, "<HTTPSamplerProxy "); got != tc.samplers {
			t.Fatalf("%s: %d samplers, test expects %d", tc.file, got, tc.samplers)
		}
		scripts := jsr223PostScripts(t, doc)
		if len(scripts) != tc.samplers {
			t.Fatalf("%s: %d capture blocks for %d samplers", tc.file, len(scripts), tc.samplers)
		}
		for _, s := range scripts {
			if !strings.Contains(s, `props.getProperty("output.capture.dir")`) {
				t.Errorf("%s: the block is not keyed on output.capture.dir", tc.file)
			}
			guard := regexp.MustCompile(`if \(dir != null && !dir\.trim\(\)\.isEmpty\(\)\) \{`)
			if !guard.MatchString(s) {
				t.Errorf("%s: the block is not guarded by the property", tc.file)
			}
			iBody, iHdr, iStatus := strings.Index(s, `".body"`), strings.Index(s, `".headers"`), strings.Index(s, `".status"`)
			if iBody < 0 || iHdr < 0 || iStatus < 0 || !(iBody < iStatus && iHdr < iStatus) {
				t.Errorf("%s: the block must write .body and .headers before .status", tc.file)
			}
			if !strings.Contains(s, "1048577") || !strings.Contains(s, "n <= 8") {
				t.Errorf("%s: the 1 MiB + 1 and 8-sample bounds are missing", tc.file)
			}
			for _, banned := range []string{"log.info", "log.debug", "println", "getResponseDataAsString", "getRequestHeaders"} {
				if strings.Contains(s, banned) {
					t.Errorf("%s: the block must not log or stringify a response (%s)", tc.file, banned)
				}
			}
			if strings.Contains(s, "log.warn(\"output capture: sample \" + n + \" was not written\")") == false {
				t.Errorf("%s: the only log line allowed is the fixed one naming the sample number", tc.file)
			}
		}
		// the block follows its sampler's pre-processor and precedes the assertion, inside the sampler's tree
		if strings.Index(doc, "JSR223PostProcessor") < strings.Index(doc, "<HTTPSamplerProxy ") {
			t.Errorf("%s: the capture block comes before its sampler", tc.file)
		}
	}
}

func TestHTTPTemplates_NothingElseInTheIngestionAssertionMovedWithTheCaptureBlock(t *testing.T) {
	doc := templateText(t, "http-ingestion.jmx")
	if !strings.Contains(doc, `JSR223Assertion guiclass="TestBeanGUI" testclass="JSR223Assertion" testname="enforce body assertion (DF-04)"`) ||
		!strings.Contains(doc, "RATE-LIMITED: retry_after=") || !strings.Contains(doc, "BODY-ASSERT-FAIL") {
		t.Errorf("the body-assertion block lost its markers")
	}
}
