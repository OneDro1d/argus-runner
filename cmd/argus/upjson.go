package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"

	"github.com/OneDro1d/argus-runner/internal/updatecmd"
)

// upjson.go is the `--json` protocol `argus up` speaks, and the translator from onboard.sh's own
// fd-5 wire (onboarding/lib/json-step.sh) into it.

// jsonEvent is one line of the protocol: exactly one JSON object per line on stdout.
type jsonEvent struct {
	Step   string `json:"step"`
	State  string `json:"state"` // start|ok|fail|skip|question
	Detail string `json:"detail,omitempty"`
	// Note is the run's own sentence, when it wrote one: on a second run that ended rollback-unreachable, what that
	// update saw and what it advises (apply.sh's $STAGE/outcome-note) — AC-D53. Detail stays the outcome word.
	Note     string        `json:"note,omitempty"`
	Question *jsonQuestion `json:"question,omitempty"`
	Result   *upResult     `json:"result,omitempty"` // only on the closing {"step":"result"} event (T2.4)
}

// upResult is T2.4's "no screen-scraping to learn what happened": the LAST line `up --json` writes on
// a completed onboard, one object carrying what an agent otherwise had to dig out of a discarded
// transcript — the instance id actually registered (onboarding can rename it), where it lives, and
// every URL onboarding resolved. No run id: onboarding starts no run; `run`/`cloud-run` return theirs.
//
// ⛔ URLs and ids only. Nothing here is a credential, and nothing may become one: the tokens onboarding
// mints stay in the files and Secrets it writes them to.
type upResult struct {
	InstanceID string `json:"instance_id"`
	Tier       string `json:"tier"`
	// Namespace is the executor's namespace on a Kubernetes tier; ComposeProject its compose project on
	// compose. Exactly one is set — the same argus-inst-<id> name, addressed the way that tier addresses it.
	Namespace      string            `json:"namespace,omitempty"`
	ComposeProject string            `json:"compose_project,omitempty"`
	Executor       string            `json:"executor,omitempty"`
	WorkspaceID    string            `json:"workspace_id,omitempty"`
	URLs           map[string]string `json:"urls,omitempty"` // control_plane, web, grafana, executor_mcp
	// Ready is false on onboard.sh's exit 3: everything is registered and wired, but a runtime object
	// was observed not Ready yet. Not a failure — the reason says what to wait for.
	Ready          bool   `json:"ready"`
	NotReadyReason string `json:"not_ready_reason,omitempty"`
	// RecordedByOnboarding is false when onboard.sh wrote no record (an older script, or one that died
	// before its record block): the fields then come from `up`'s own argv and may be what was ASKED FOR
	// rather than what was installed. Said, not hidden.
	RecordedByOnboarding bool `json:"recorded_by_onboarding"`
}

// buildUpResult assembles the closing result from onboarding's own record, falling back to argv.
func buildUpResult(runnerID string, a upArgs, code int, recordPath string) upResult {
	r := upResult{InstanceID: runnerID, Tier: a.Tier, Executor: a.Image, Ready: code == exitOK}
	if r.Tier == "" || r.Tier == "auto" {
		r.Tier = "compose"
	}
	if rec, ok := readUpRecord(recordPath); ok {
		r.RecordedByOnboarding = true
		r.InstanceID, r.Tier, r.Executor, r.WorkspaceID = rec.InstanceID, rec.Tier, rec.Executor, rec.WorkspaceID
		urls := map[string]string{}
		for k, v := range map[string]string{"control_plane": rec.ControlPlane, "web": rec.Web, "grafana": rec.Grafana, "executor_mcp": rec.ExecutorMCP} {
			if v != "" {
				urls[k] = v
			}
		}
		if len(urls) > 0 {
			r.URLs = urls
		}
	}
	if r.InstanceID != "" {
		if r.Tier == "compose" {
			r.ComposeProject = updatecmd.InstanceProject(r.InstanceID)
		} else {
			r.Namespace = updatecmd.InstanceNamespace(r.InstanceID)
		}
	}
	if code == onboardNotReadyExit {
		r.NotReadyReason = "registered and wired, but a runtime object was observed not Ready yet (onboard.sh exit 3) — re-check with `kubectl get pods` / `docker ps`; this is not a failed onboard"
	}
	return r
}

// jsonQuestion accompanies a `question` event: something `up` needs from the operator (or agent)
// before it can continue.
type jsonQuestion struct {
	Key     string `json:"key"`
	Prompt  string `json:"prompt"`
	Default string `json:"default,omitempty"`
	Secret  bool   `json:"secret"`
}

// emitStep writes exactly one line — one JSON object, compact, newline-terminated — to w.
func emitStep(w io.Writer, ev jsonEvent) error {
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "%s\n", b)
	return err
}

// answerLine is the shape readAnswer expects on stdin: {"answer": "..."}.
type answerLine struct {
	Answer string `json:"answer"`
}

// readAnswer reads exactly one line from r and unmarshals it as {"answer": "..."}.
func readAnswer(r io.Reader) (string, error) {
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil && line == "" {
		return "", fmt.Errorf("no answer read: %w", err)
	}
	var a answerLine
	if uerr := json.Unmarshal([]byte(line), &a); uerr != nil {
		return "", fmt.Errorf("malformed answer line %q: %w", line, uerr)
	}
	return a.Answer, nil
}

// yesCanAnswer is the general guard every question site must call before letting --yes default a
// question instead of asking it: --yes may accept a NON-secret default, never a secret-shaped one.
// A credential's "sensible default" is never sensible — it would mean `up --yes` silently minting or
// reusing a secret the operator never saw asked for.
func yesCanAnswer(q jsonQuestion) bool { return !q.Secret }

// askOrDefault is the ONE call site every question in this pass goes through — resolveProductDir's
// product-folder question today, and any future one. It exists as a named function (rather than
// being inlined at that one call site) so yesCanAnswer's refusal is enforced in exactly one place,
// and so a test can drive it directly with a fake, secret-shaped question without needing a second
// real trigger to exist yet (up_secret_question_test.go).
func askOrDefault(w io.Writer, r io.Reader, yes bool, q jsonQuestion, jsonMode bool, defaultVal string) (string, error) {
	if yes {
		if !yesCanAnswer(q) {
			return "", fmt.Errorf("--yes cannot answer %q: it is secret-shaped and refuses to default a credential", q.Key)
		}
		return defaultVal, nil
	}
	if !jsonMode {
		return "", fmt.Errorf("cannot ask %q: no --json (nothing reads a prompt off plain stdout in this pass) and no --yes", q.Key)
	}
	if err := emitStep(w, jsonEvent{Step: q.Key, State: "question", Question: &q}); err != nil {
		return "", err
	}
	return readAnswer(r)
}

// onboardFD5Line is the contract onboard.sh's json_step (onboarding/lib/json-step.sh) writes to fd
// 5, one object per line. Field names are load-bearing and must stay in lockstep with the shell side.
type onboardFD5Line struct {
	Step   string `json:"step"`
	State  string `json:"state"` // onboard.sh only ever writes "start" or "fail" — never "ok"
	Detail string `json:"detail"`
}

// upStepTranslator is the "last open step" state machine that infers "ok". onboard.sh's own
// json_step never emits it (onboarding/lib/json-step.sh's header explains why: "ok"/"skip" are not
// its job) — a step is known to have finished successfully only when a DIFFERENT step starts, or the
// whole run exits 0 while one was still open. This works for any step name onboard.sh happens to
// emit; it never special-cases one, so a step-table change in the shell script cannot silently break
// it (see the exact requirement this guards: up_json_protocol_test.go's TestUp_OkIsInferred_...).
type upStepTranslator struct {
	w          io.Writer
	lastStep   string
	lastDetail string
	open       bool
}

// onLine translates one raw onboard.sh fd-5 line into the --json protocol, injecting the inferred
// "ok" for the PREVIOUS step when a new, different one starts.
func (t *upStepTranslator) onLine(step, state, detail string) {
	if state == "start" {
		if t.open && t.lastStep != step {
			_ = emitStep(t.w, jsonEvent{Step: t.lastStep, State: "ok"})
		}
		t.lastStep = step
		t.lastDetail = detail
		t.open = true
		_ = emitStep(t.w, jsonEvent{Step: step, State: "start", Detail: detail})
		return
	}
	// Every other state onboard.sh actually writes (today: "fail") closes out whatever step it
	// names — it is no longer "open" for the exit-success inference below.
	if step == t.lastStep {
		t.open = false
	}
	_ = emitStep(t.w, jsonEvent{Step: step, State: state, Detail: detail})
}

// onExitSuccess emits the final inferred "ok" for whichever step was still open when onboard.sh
// exited 0 — the second half of the inference rule (the first is in onLine). It carries that step's
// OWN start detail forward (#48c): the "done" step names the dashboard link there, and the inferred
// "ok" is the only line a --json caller ever sees for it — dropping the detail would drop the link.
func (t *upStepTranslator) onExitSuccess() {
	if t.open {
		_ = emitStep(t.w, jsonEvent{Step: t.lastStep, State: "ok", Detail: t.lastDetail})
		t.open = false
	}
}
