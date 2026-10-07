package argus

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/config"
)

// fakeJMeter is the stand-in JMeter binary the local runner is driven with here. It reproduces
// exactly what JMeter's option processing writes into the `-j` run log (JMeter rel/v5.6.3,
// src/core/src/main/java/org/apache/jmeter/JMeter.java, initializeProperties):
//
//	case JMETER_PROPERTY:  log.info("Setting JMeter property: {}={}", name, value);
//	case PROPFILE2_OPT:    log.info("Loading additional properties from: {}", name);
//
// i.e. every `-J` property is echoed NAME AND VALUE into the run log, while a `-q` properties file
// is mentioned by PATH only — its contents are loaded with Properties.load and never logged.
// The fake also records what the `-q` file carried, OUTSIDE the results dir, so the test can prove
// the credential still reaches JMeter (the templates keep working) without it reaching the disk
// JMeter leaves behind.
const fakeJMeter = `#!/bin/sh
log=""; jtl=""; qfile=""; props=""
while [ $# -gt 0 ]; do
  case "$1" in
    --version) echo "5.6.3"; exit 0;;
    -j) log="$2"; shift;;
    -l) jtl="$2"; shift;;
    -q) qfile="$2"; shift;;
    -J*) props="$props
INFO o.a.j.JMeter: Setting JMeter property: ${1#-J}";;
  esac
  shift
done
{
  echo "INFO o.a.j.JMeter: Version 5.6.3"
  if [ -n "$qfile" ]; then echo "INFO o.a.j.JMeter: Loading additional properties from: $qfile"; fi
  printf '%s\n' "$props"
} >> "$log"
if [ -n "$qfile" ] && [ -n "$FAKE_JMETER_SEEN" ]; then cat "$qfile" >> "$FAKE_JMETER_SEEN"; fi
printf 'timeStamp,elapsed,label,responseCode,responseMessage,threadName,dataType,success,failureMessage,bytes,sentBytes,grpThreads,allThreads,URL,Latency,IdleTime,Connect\n' > "$jtl"
printf '1700000000000,12,SUT trigger,202,Accepted,T 1-1,text,true,,10,10,1,1,http://x,10,0,5\n' >> "$jtl"
`

func writeFakeJMeter(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "jmeter")
	if err := os.WriteFile(p, []byte(fakeJMeter), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// grepTree returns every file under root whose bytes contain needle.
func grepTree(t *testing.T, root, needle string) []string {
	t.Helper()
	var hits []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		if strings.Contains(string(b), needle) {
			hits = append(hits, p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return hits
}

// The app-under-test's credentials are handed to JMeter for the templates to use, and JMeter
// echoes every `-J` property into its run log — so a bearer token passed as `-Jauth.header=…`
// lands in cleartext in <results>/<template>__<id>.jtl.log. This runs one scenario through the
// LOCAL runner with a config whose token is a marker and greps everything JMeter left on disk.
func TestRunAll_CredentialsNeverReachTheResultsDir(t *testing.T) {
	const marker = "SECRET-MARKER-9f3a"
	dir := t.TempDir()
	bin := writeFakeJMeter(t, filepath.Join(dir, "bin"))
	seen := filepath.Join(dir, "seen-by-jmeter.properties")
	t.Setenv("FAKE_JMETER_SEEN", seen)
	// The runner's private properties file goes to the temp dir; point it under `dir` so the final
	// sweep below can see whether it was left behind.
	tmp := filepath.Join(dir, "tmp")
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", tmp)

	scDir := filepath.Join(dir, "scenarios")
	writeScenario(t, scDir, "http-ingestion", "SEC-001", "status=202")
	c := httpConfig()
	c.Targets.Auth = &config.AuthTarget{BearerToken: marker}

	results := filepath.Join(dir, "results")
	r := &LocalJMeterRunner{TemplatesDir: filepath.Join("..", "..", "templates"), JMeterBin: bin}
	rr, err := RunAll(c, scDir, results, "p", "", "", "", "", r)
	if err != nil {
		t.Fatal(err)
	}
	if rr.Report.Summary.Errored != 0 {
		t.Fatalf("the fake executor must have run: %+v", rr.Report.Summary)
	}

	// The run log is there, and the NON-secret properties still travel as -J (the fake logs them
	// exactly as JMeter does) — the test is looking at a real run log, not an empty file.
	runLog := filepath.Join(results, "http-ingestion__SEC-001.jtl.log")
	logBytes, err := os.ReadFile(runLog)
	if err != nil {
		t.Fatalf("run log missing: %v", err)
	}
	if !strings.Contains(string(logBytes), "Setting JMeter property: scenario.id=SEC-001") {
		t.Fatalf("run log does not look like a JMeter run log:\n%s", logBytes)
	}

	// THE PROMISE: the credential is in NO file under the results dir.
	if hits := grepTree(t, results, marker); len(hits) > 0 {
		for _, h := range hits {
			t.Errorf("the app-under-test's credential is on disk in %s", h)
		}
		t.FailNow()
	}
	// …and JMeter still received it, so `${__P(auth.header)}` in the templates resolves as before.
	seenBytes, err := os.ReadFile(seen)
	if err != nil || !strings.Contains(string(seenBytes), "auth.header=Bearer "+marker) {
		t.Fatalf("JMeter never received the credential through the properties file (err=%v):\n%s", err, seenBytes)
	}
	// …and the properties file that carried it is gone once the run is over.
	if hits := grepTree(t, dir, marker); len(hits) != 1 || hits[0] != seen {
		t.Errorf("the credential must survive only in the test's own side channel, found in %v", hits)
	}
}
