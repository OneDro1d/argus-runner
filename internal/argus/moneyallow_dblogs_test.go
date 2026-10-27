package argus

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/chain"
	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/report"
)

// MEETS / -02, ON THE TREE WHERE ALL THREE EXIST. The allow-list branch was built on
// a base that had no `db` or `logs` step (its entries were tested with hand-written payloads, which stop at "unknown
// chain step type"), and the two step branches were built on a base whose money guard could not be told anything
// (their pins assert "refused, nothing dialled"). This is the one place the three are run together: with the owner's
// entry the REAL step is built and reaches its database / its Loki on a money-handling run, and without it nothing
// is dialled. Both directions, so a matcher that names the wrong field of the step fails here.

func moneyDBConfig(entries ...config.MoneyWriteAllowEntry) *config.Config {
	c := dbConfig()
	c.MoneyHandling = true
	c.Project.Name = "p"
	if len(entries) > 0 {
		c.MoneyWrites = &config.MoneyWritesConfig{Allow: entries}
	}
	return c
}

func TestMoneyAllow_ADBEntryLetsTheRealDBStepRun_AndOnlyForItsTarget(t *testing.T) {
	for _, tc := range []struct {
		name      string
		entries   []config.MoneyWriteAllowEntry
		wantRun   bool
		wantDials int
	}{
		{"no entry: refused, nothing dialled", nil, false, 0},
		{"an entry for the unnamed target: the step runs", []config.MoneyWriteAllowEntry{{Engine: "db"}}, true, 1},
		{"an entry for ANOTHER target does not cover the unnamed one", []config.MoneyWriteAllowEntry{{Engine: "db", Target: "ledger"}}, false, 0},
		{"a logs entry does not allow a db step", []config.MoneyWriteAllowEntry{{Engine: "logs"}}, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &argusFakeDB{rows: chain.DBRows{Columns: []string{"status"}, Rows: [][]any{{"PAID"}}}}
			withFakeDB(t, f)
			dir := t.TempDir()
			scDir := filepath.Join(dir, "scenarios")
			writeDBChainScenario(t, scDir, "MONEY-DB") // a db step on the UNNAMED target (targets.database)
			rr, err := RunAll(moneyDBConfig(tc.entries...), scDir, filepath.Join(dir, "results"), "p", "", "", "", "", &fakeRunner{})
			if err != nil {
				t.Fatalf("RunAll: %v", err)
			}
			got, _ := rr.Report.Find("MONEY-DB")
			if got == nil {
				t.Fatal("the scenario is missing from the report")
			}
			if tc.wantRun {
				if got.Status != "passed" {
					t.Fatalf("the owner listed the db step, so it must run: %q %+v", got.Status, got.Failure)
				}
			} else if got.Status != report.StatusError || got.Failure == nil || !strings.HasPrefix(got.Failure.Observed, MoneyGuardRefusal) ||
				!strings.Contains(got.Failure.Observed, `chain step q is a "db" step`) {
				t.Fatalf("not listed, so it must be the money refusal naming the db step: %q %+v", got.Status, got.Failure)
			}
			if f.dials != tc.wantDials {
				t.Fatalf("connections opened: got %d want %d", f.dials, tc.wantDials)
			}
		})
	}
}

func TestMoneyAllow_ALogsEntryLetsTheRealLogsStepRun(t *testing.T) {
	for _, tc := range []struct {
		name        string
		entries     []config.MoneyWriteAllowEntry
		wantRun     bool
		wantQueries int
	}{
		{"no entry: refused, Loki not queried", nil, false, 0},
		{"a logs entry: the step runs", []config.MoneyWriteAllowEntry{{Engine: "logs"}}, true, 1},
		{"a db entry does not allow a logs step", []config.MoneyWriteAllowEntry{{Engine: "db"}}, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			loki := newTestLoki(t, 0)
			dir := t.TempDir()
			scDir := filepath.Join(dir, "scenarios")
			writeLogsChainScenario(t, scDir, "MONEY-LOGS") // claims count == 0, and the test Loki holds no line
			rr, err := RunAllWithLogs(moneyDBConfig(tc.entries...), scDir, filepath.Join(dir, "results"), "p", "", "", "", "", &fakeRunner{}, nil, "", "", nil, loki.reader())
			if err != nil {
				t.Fatalf("RunAllWithLogs: %v", err)
			}
			got, _ := rr.Report.Find("MONEY-LOGS")
			if got == nil {
				t.Fatal("the scenario is missing from the report")
			}
			if tc.wantRun {
				if got.Status != "passed" {
					t.Fatalf("the owner listed the logs step, so it must run: %q %+v", got.Status, got.Failure)
				}
			} else if got.Status != report.StatusError || got.Failure == nil || !strings.HasPrefix(got.Failure.Observed, MoneyGuardRefusal) ||
				!strings.Contains(got.Failure.Observed, `chain step l is a "logs" step`) {
				t.Fatalf("not listed, so it must be the money refusal naming the logs step: %q %+v", got.Status, got.Failure)
			}
			if loki.requests() != tc.wantQueries {
				t.Fatalf("Loki queries: got %d want %d", loki.requests(), tc.wantQueries)
			}
		})
	}
}
