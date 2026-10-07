package argus

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// The results dir is bundled by allow-list: report.json and runs/<id>.json leave the environment,
// JMeter's raw samples, run logs, properties files and the outbox never do.
func TestPublishableResult_AllowListOnly(t *testing.T) {
	allowed := []string{"report.json", "runs/tr-abc.json", "./runs/tr-abc.json", filepath.Join("runs", "x.json")}
	for _, rel := range allowed {
		if !PublishableResult(rel) {
			t.Errorf("%q must be publishable", rel)
		}
	}
	denied := []string{
		"http-ingestion__ORD-001.jtl", "http-ingestion__ORD-001.jtl.log", "cleanup__ORD-001.jtl",
		"argus-run-123.properties", "runs/tr-abc.jtl.log", "runs/nested/tr-abc.json", "runs/.json",
		"outbox/tr-abc.json", "report.json.bak", "../report.json", "REPORT.JSON", "",
	}
	for _, rel := range denied {
		if PublishableResult(rel) {
			t.Errorf("%q must NOT be publishable", rel)
		}
	}
}

func TestPublishableResults_WalksAndFilters(t *testing.T) {
	dir := t.TempDir()
	for _, rel := range []string{
		"report.json", "runs/tr-1.json", "runs/tr-2.json",
		"http-ingestion__ORD-001.jtl", "http-ingestion__ORD-001.jtl.log", "runs/tr-1.jtl.log",
		"outbox/tr-1.json", "argus-run-9.properties",
	} {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := PublishableResults(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"report.json", "runs/tr-1.json", "runs/tr-2.json"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("publishable set:\n got %v\nwant %v", got, want)
	}
}
