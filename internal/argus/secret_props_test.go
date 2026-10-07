package argus

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

// The credential-bearing property set is closed and pinned: a new one is added HERE, deliberately.
func TestIsSecretProp_PinnedSet(t *testing.T) {
	for _, name := range []string{"auth.header", "db.password", "mgmt.auth.header", "mq.amqp.password", "mq.amqp.username",
		"trigger.header_1_value", "trigger.header_12_value"} {
		if !isSecretProp(name) {
			t.Errorf("%q carries a credential and must be a secret property", name)
		}
	}
	for _, name := range []string{"scenario.id", "correlation.id", "http.host", "db.url", "db.user", "db.query",
		"mgmt.host", "mgmt.port", "mq.queue", "mq.amqp.uri", "mq.user_id", "expect.refusal_code",
		"trigger.header_n", "trigger.header_1_name", "expect.status"} {
		if isSecretProp(name) {
			t.Errorf("%q is not a credential and must stay a -J property", name)
		}
	}
	if got, want := len(secretPropNames), 5; got != want {
		t.Errorf("the named secret set changed (%d names, expected %d) — update this test on purpose", got, want)
	}
}

func TestSplitSecretProps(t *testing.T) {
	in := map[string]string{"scenario.id": "S-1", "auth.header": "Bearer x", "db.password": "pw", "db.user": "u"}
	public, secret := splitSecretProps(in)
	if want := map[string]string{"scenario.id": "S-1", "db.user": "u"}; !reflect.DeepEqual(public, want) {
		t.Errorf("public = %v, want %v", public, want)
	}
	if want := map[string]string{"auth.header": "Bearer x", "db.password": "pw"}; !reflect.DeepEqual(secret, want) {
		t.Errorf("secret = %v, want %v", secret, want)
	}
	public["new"] = "x"
	if _, ok := in["new"]; ok {
		t.Error("the split must not alias the input")
	}
}

// The local runner never emits a secret as -J, whatever it is handed; the properties file rides -q.
func TestLocalJMeterRunner_BuildArgs_NeverEmitsASecretAsJ(t *testing.T) {
	r := &LocalJMeterRunner{TemplatesDir: "/templates"}
	props := map[string]string{
		"scenario.id": "S-1", "auth.header": "Bearer SECRET", "db.password": "PW-SECRET",
		"mgmt.auth.header": "Basic SECRET", "trigger.header_1_name": "X-Api-Key", "trigger.header_1_value": "KEY-SECRET",
	}
	joined := strings.Join(r.buildArgs("database-state", "/results/d.jtl", "/tmp/argus-run-1.properties", props), " ")
	if strings.Contains(joined, "SECRET") {
		t.Fatalf("a credential is on the command line:\n  %s", joined)
	}
	for _, want := range []string{"-q /tmp/argus-run-1.properties", "-Jscenario.id=S-1", "-Jtrigger.header_1_name=X-Api-Key"} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q:\n  %s", want, joined)
		}
	}
	if strings.Contains(strings.Join(r.buildArgs("database-state", "/results/d.jtl", "", props), " "), "-q") {
		t.Error("no properties file, no -q")
	}
}

// The compose runner: the same split, with the properties file written INSIDE the executor
// container from stdin — never on the shared results volume, never on a command line.
func TestDockerRunner_JMeterArgs_SecretsRideStdin(t *testing.T) {
	d := &DockerRunner{ComposeFile: "/c/compose.yaml"}
	props := map[string]string{"scenario.id": "S-1", "auth.header": "Bearer SECRET"}
	args, stdin := d.jmeterArgs("jmeter", "http-ingestion", "/results/x.jtl", "/results/x.jtl.log", props)
	joined := strings.Join(args, "\x00")
	if strings.Contains(joined, "SECRET") {
		t.Fatalf("a credential is on the command line:\n  %q", args)
	}
	if !strings.HasPrefix(joined, strings.Join([]string{"compose", "-f", "/c/compose.yaml", "exec", "-T", "jmeter", "sh", "-c", secretsWrapper, "jmeter", "-n", "-t", "/templates/http-ingestion.jmx", "-l", "/results/x.jtl", "-j", "/results/x.jtl.log", "-Jscenario.id=S-1"}, "\x00")) {
		t.Errorf("unexpected argv: %q", args)
	}
	if string(stdin) != "auth.header=Bearer SECRET\n" {
		t.Errorf("stdin = %q", stdin)
	}
	// No credential: the plain exec it always was, nothing on stdin.
	args, stdin = d.jmeterArgs("jmeter", "cleanup-sql", "/results/c.jtl", "", map[string]string{"scenario.id": "S-1"})
	if stdin != nil || !reflect.DeepEqual(args, []string{"compose", "-f", "/c/compose.yaml", "exec", "-T", "jmeter", "jmeter", "-n", "-t", "/templates/cleanup-sql.jmx", "-l", "/results/c.jtl", "-Jscenario.id=S-1"}) {
		t.Errorf("plain argv: %q stdin=%q", args, stdin)
	}
}

// java.util.Properties escaping: what Properties.load reads back must equal what was written.
func TestEncodeProperties_JavaEscapes(t *testing.T) {
	got := string(encodeProperties(map[string]string{
		"auth.header":      "Bearer a=b:c#d!e\\f",
		"db.password":      " lead\ttab\nnl\u00e9\u4e16",
		"trigger.header_1": "plain",
	}))
	want := "auth.header=Bearer a\\=b\\:c\\#d\\!e\\\\f\n" +
		"db.password=\\ lead\\ttab\\nnl\\u00E9\\u4E16\n" +
		"trigger.header_1=plain\n"
	if got != want {
		t.Errorf("encodeProperties:\n got %q\nwant %q", got, want)
	}
}

func TestWriteSecretPropsFile_PrivateAndRemoved(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	p, remove, err := writeSecretPropsFile(map[string]string{"auth.header": "Bearer x"})
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o, want 0600", st.Mode().Perm())
	}
	if b, _ := os.ReadFile(p); string(b) != "auth.header=Bearer x\n" {
		t.Errorf("content = %q", b)
	}
	remove()
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Error("the properties file must be removed after the run")
	}
	if p, _, err := writeSecretPropsFile(nil); err != nil || p != "" {
		t.Errorf("no secrets: no file, got %q %v", p, err)
	}
}

// The run log is created by the runner itself, 0600, before JMeter opens it.
func TestLocalJMeterRunner_RunLogAndResultsDirArePrivate(t *testing.T) {
	dir := t.TempDir()
	bin := writeFakeJMeter(t, dir)
	jtl := dir + "/x.jtl"
	r := &LocalJMeterRunner{TemplatesDir: "/templates", JMeterBin: bin}
	if err := r.Run("http-ingestion", jtl, map[string]string{"scenario.id": "S-1"}, 0); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(jtl + ".log")
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("run log mode = %o, want 0600", st.Mode().Perm())
	}
}
