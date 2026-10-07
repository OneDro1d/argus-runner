// Package doctor is the verdict logic behind `argus doctor`: the read-only diagnosis a machine gets
// BEFORE its first run, in a shape an agent can act on — per check, what was examined, what was found,
// what it means, and the literal next command.
//
// Why it exists. Over a month of first runs on two products (Memstore and Hub, 2026-08-27 → 09-25),
// 169 friction incidents were recorded, 38 of them in the credentials phase. Some were confusions the CLI
// could have named in one line — a 15-minute access token pasted where a long-lived one was needed, a
// runner token presented to author verbs, the local hat map mistaken for a credential — and none were,
// because every command discovered the fault at the moment it needed the thing and reported the
// symptom (`401`, `denied`, `not permitted outside the builder scope`), not the cause.
//
// Design rules, inherited from internal/preflight:
//   - READ-ONLY: it writes nothing, except a rotated session token (saving that is right; dropping it
//     would sign the machine out). A doctor that changes anything else is the wrong thing to run when
//     you do not yet know whether it is safe to act.
//   - Subject says WHAT WAS EXAMINED. A check whose subject is not stated cannot be trusted to have
//     answered the question asked rather than a neighbouring one that happens to be true.
//   - `unknown` is never a pass. A check that could not run blocks like a failed one; the two have
//     different fixes, so they are different statuses.
//   - Credential VALUES never enter a Check. Facts about them (kind, scope, expiry) do. A transcript is
//     stored, so a printed credential counts as leaked.
package doctor

// Status is a check's outcome.
type Status string

const (
	// StatusOK: examined, and fine.
	StatusOK Status = "ok"
	// StatusWarn: examined; it will work, but almost certainly not the way you meant — read Detail.
	StatusWarn Status = "warn"
	// StatusFail: examined, and it will not work. Fix says what to do.
	StatusFail Status = "fail"
	// StatusUnknown: the check COULD NOT RUN. Never treat this as ok — it counts as failing.
	StatusUnknown Status = "unknown"
	// StatusSkip: deliberately not run, because what it depends on is not there yet (`doctor --tester`
	// with no token skips the checks that need one). Neutral: it neither fails nor warns — the check it
	// depends on already did.
	StatusSkip Status = "skip"
)

// Check is one diagnosis.
type Check struct {
	ID string `json:"id"`
	// What is the question this check answers, in one human sentence.
	What string `json:"what"`
	// Subject is WHAT WAS EXAMINED — the env var name, the file path, the directory, the kind of
	// token found. Never a credential value.
	Subject string `json:"subject"`
	Status  Status `json:"status"`
	// Phase names the onboarding phase a `doctor --tester` check evidences ("P3", "P4", "P5"); "" otherwise.
	Phase string `json:"phase,omitempty"`
	// Detail explains a non-ok status in the reader's terms — the cause, not the symptom.
	Detail string `json:"detail,omitempty"`
	// Fix is the LITERAL next command or action.
	Fix string `json:"fix,omitempty"`
}

// Verdicts. Derived from the checks, never set by hand.
const (
	VerdictOK   = "ok"
	VerdictWarn = "warn"
	VerdictFail = "fail"
)

// Report is the whole answer.
type Report struct {
	// Verdict is "ok", "warn" or "fail". An `unknown` check makes the verdict "fail": not knowing is
	// not a reason to proceed.
	Verdict string `json:"verdict"`
	// Failing lists the ids of every check with status fail or unknown, in report order.
	Failing []string `json:"failing,omitempty"`
	// Warning lists the ids of every check with status warn, in report order.
	Warning []string `json:"warning,omitempty"`
	Checks  []Check  `json:"checks"`
	// Lines is the same checks as one PASS/WARN/FAIL/SKIP line each, with the evidence (`doctor --tester`).
	Lines []string `json:"lines,omitempty"`
}

// Line renders a check as `PASS|WARN|FAIL|SKIP <phase> <id>: <subject> — <detail>`. unknown reads as FAIL:
// not knowing is not a pass. warn reads as WARN, because it does not fail the verdict or the exit code, and a
// FAIL line on a run that exits 0 teaches the reader to ignore FAIL.
func (c Check) Line() string {
	word := "FAIL"
	switch c.Status {
	case StatusOK:
		word = "PASS"
	case StatusWarn:
		word = "WARN"
	case StatusSkip:
		word = "SKIP"
	}
	s := word + " "
	if c.Phase != "" {
		s += c.Phase + " "
	}
	s += c.ID + ": " + c.Subject
	if c.Detail != "" {
		s += " — " + c.Detail
	}
	if c.Fix != "" && c.Status != StatusOK {
		s += " [fix: " + c.Fix + "]"
	}
	return s
}

// Summarize derives the verdict. It never short-circuits: a first-run operator wants the WHOLE list,
// because fixing one thing per round-trip is exactly the attention cost this command exists to remove.
func Summarize(checks []Check) Report {
	rep := Report{Verdict: VerdictOK, Checks: append([]Check{}, checks...)}
	for _, c := range checks {
		switch c.Status {
		case StatusFail, StatusUnknown:
			rep.Failing = append(rep.Failing, c.ID)
		case StatusWarn:
			rep.Warning = append(rep.Warning, c.ID)
		}
	}
	switch {
	case len(rep.Failing) > 0:
		rep.Verdict = VerdictFail
	case len(rep.Warning) > 0:
		rep.Verdict = VerdictWarn
	}
	return rep
}

func quote(s string) string {
	if s == "" {
		return `""`
	}
	return `"` + s + `"`
}
