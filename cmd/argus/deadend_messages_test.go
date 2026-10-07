package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// VR9-H4 rule 1 / VR9-O1 atomicity check 5 — THE TWO REFUSALS MUST NOT ADVISE AN ACTION THE PAGE
// WITHHOLDS.
//
// ── WHY THIS IS AN ATOMICITY CONSTRAINT AND NOT A WORDING PREFERENCE ──────────────────────────────
//
// VR9-H1's gate (already shipped) DISABLES Rotate on a machine reporting zero holders. Both refusals
// below fire on exactly that machine. Until they are rewritten the product says:
//
//	CLI:  "…or press Rotate on the Tokens page, which mints a replacement and hands it to this
//	       machine's router on its next heartbeat."
//	page: the Rotate button, disabled, because this machine holds nothing.
//
// An operator following the instruction reaches a control that refuses. That is a NEW dead end created
// by the fix, which is why the SA made the pairing an atomicity check rather than a nicety, and why the
// texts are FIXED IN THE SPEC rather than left to whoever implements them.
//
// ⛔ THE ASSERTIONS ARE ON WHAT THE MESSAGES *ADVISE*, not on their prose. A refusal may still mention
// the page; what it may not do is send the operator to press a control this very build disables.
// Holdout S-X3 judges exactly that.
// advisesTheGatedControl answers the question the subtests below actually care about: does this text
// send the operator to a control VR9-H1's gate may have disabled?
//
// ⛔ IT IS NOT A SEARCH FOR ONE PHRASE. Every check here used to be Contains(msg, "press Rotate").
// An adversary gate wrote "use the Rotate control on the Tokens page" and "hit the Rotate button on
// the Tokens page" — both send the operator to the disabled button, neither contains the literal —
// and all three subtests passed, including the one named for forbidding exactly this. The test's own
// header claimed the assertions were on what the messages ADVISE. They were on their prose.
//
// Rotate and Generate are the two controls holderGate withholds. A refusal that NAMES either as an
// action is a dead end on the machine the refusal is being printed on.
// gatedControlRx matches the NAMES of the two controls holderGate withholds, as WHOLE WORDS.
//
// ⚠ THE WORD BOUNDARY IS LOAD-BEARING. A first version used a substring search and matched
// `Rotated` inside applyRotatedToken and OnRotatedToken — flagging identifiers and
// comments that have nothing to do with advising anyone.
var gatedControlRx = regexp.MustCompile(`\b(Rotate|Generate)\b`)

func advisesTheGatedControl(msg string) string {
	// ⛔ NO VERB LIST. A first version looked for one of {press, hit, use, click, tap, choose, select}
	// within 40 characters of the control. An adversary gate wrote:
	//
	//	"The Rotate button on the Tokens page issues a fresh one; Generate replaces it outright."
	//
	// — no verb from the list, and the subtest named for forbidding exactly this passed. A keyword
	// list is a guess about phrasing; the property is about WHAT THE MESSAGE POINTS AT.
	//
	// A refusal printed on a machine where those controls are disabled has no legitimate reason to
	// name either, so naming one at all is the defect — whatever verb surrounds it.
	if loc := gatedControlRx.FindStringIndex(msg); loc != nil {
		lo, hi := loc[0]-45, loc[1]+45
		if lo < 0 {
			lo = 0
		}
		if hi > len(msg) {
			hi = len(msg)
		}
		return strings.TrimSpace(msg[lo:hi])
	}
	return ""
}

// operatorStrings returns the double-quoted string literals in a Go source file — the only place a
// message an operator can read is written. ⛔ SCANNING A WHOLE FILE FLAGS IDENTIFIERS AND COMMENTS;
// this scan is about what gets PRINTED.
func operatorStrings(src string) string {
	var b strings.Builder
	// ⚠ TWO LITERAL BACKSLASHES PER ESCAPE, NOT FOUR. A first version doubled them inside a RAW
	// string, so the regex required two literal backslashes to recognise an escape — and any message
	// carrying a single backslash escape ended the match early and was invisible to this scan.
	// Measured on main.go: 1090 fragments returned instead of 1094 literals, with 36 escape-carrying
	// literals skipped. The scan looked wider than it was.
	for _, m := range regexp.MustCompile(`"(?:[^"\\]|\\.)*"`).FindAllString(src, -1) {
		b.WriteString(m)
		b.WriteString(" ")
	}
	return b.String()
}

func max0(i int) int {
	if i < 0 {
		return 0
	}
	return i
}

func TestRefusals_DoNotAdviseAnActionTheGateWithholds(t *testing.T) {
	mainGo := readSrc(t, "main.go")
	rotateack := readSrc(t, "rotateack.go")

	// ── The cloud-mint-token refusal (main.go) ────────────────────────────────────────
	//
	// V26-002 measured a refusal that sent the operator to a disabled button. Since V27-009 (0.3.29) the
	// token is per machine, so "the account already has one" is no longer a refusal at all; the one that
	// remains is "this machine holds no record" — and it must name the step that creates one.
	t.Run("cloud-mint-token's no-record refusal names the step that creates the record", func(t *testing.T) {
		msg := extractRefusal(t, mainGo, "cloud-mint-token: this machine holds no record for")
		if hit := advisesTheGatedControl(msg); hit != "" {
			t.Fatalf("the refusal sends the operator to a control VR9-H1 disables (%q).\n\ngot: %s", msg, hit)
		}
		low := strings.ToLower(msg)
		if !strings.Contains(low, "router register") {
			t.Errorf("the refusal does not name `router register` (onboarding step 8a).\n\ngot: %s", msg)
		}
		if !strings.Contains(low, "nothing was minted") {
			t.Errorf("the refusal does not say that nothing was minted — the operator must know no orphan token exists.\n\ngot: %s", msg)
		}
	})

	// ── The rotation-acknowledgement refusal (rotateack.go) ───────────────────────────────────────
	//
	// ⚠ A DIFFERENT MESSAGE ON A DIFFERENT PATH. An earlier revision of the PO confused the two and
	// told the build to delete this one. It refuses to ACK a rotation nothing can hold — correctly —
	// and its advice is what needs to change, not its existence.
	//
	// V27-009 redesign: the one place a replacement can land is the machine's record for (control
	// plane, user), and the one action that creates a record is `router register` (onboarding step
	// 8a). `router wire --cloud-url` no longer creates a holder — it REFERS to a record and is itself
	// refused without one — so the escape the refusal names is the register step.
	t.Run("the rotation refusal names an escape that works on a machine with no record", func(t *testing.T) {
		msg := extractRefusal(t, rotateack, "refusing to acknowledge the rotation")
		if !strings.Contains(msg, "router register") {
			t.Fatalf("the refusal does not name `router register` (onboarding step 8a), the one action "+
				"that creates the record the replacement would be stored on — the only escape available "+
				"on a machine with folders but no record.\n\ngot: %s", msg)
		}
		if hit := advisesTheGatedControl(msg); hit != "" {
			t.Errorf("the refusal advises the button VR9-H1 disables here.\n\ngot: %s", msg)
		}
		// Re-onboarding remains a valid answer and must not be dropped for the newer one.
		if !strings.Contains(strings.ToLower(msg), "re-onboard") {
			t.Errorf("the rewrite dropped re-onboarding, which is still a correct remedy.\n\ngot: %s", msg)
		}
	})

	// ⛔ AND NOWHERE ELSE IN THE CLI MAY ADVISE IT EITHER. Fixing the two sites the spec names while a
	// third carries the same sentence is the "assert on the SOURCE, not one transcript" rule the PO
	// wrote for VR9-H2, and it applies just as well here.
	// ⛔ A SCAN LIST THAT NAMES A FILE WHICH DOES NOT EXIST IS A SILENT HOLE. The first version
	// listed "cloud.go", which is not in this package at all, and `continue`d past it without a word.
	// A reader counting four names would believe four files were searched. Two guards now: every name
	// must resolve, and the loop must have actually read something.
	t.Run("no CLI refusal anywhere advises pressing Rotate", func(t *testing.T) {
		names := []string{"main.go", "rotateack.go", "router_wire.go"}
		read := 0
		for _, f := range names {
			src, err := os.ReadFile(f)
			if err != nil {
				t.Errorf("%s is named in this scan but could not be read (%v). A name that does not "+
					"resolve makes the scan look wider than it is — remove it or fix it, never skip it.", f, err)
				continue
			}
			read++
			if hit := advisesTheGatedControl(operatorStrings(string(src))); hit != "" {
				t.Errorf("%s advises a control the gate withholds: %q", f, hit)
			}
		}
		if read != len(names) {
			t.Fatalf("read %d of %d files; this scan did not cover what it claims", read, len(names))
		}
	})
}

func readSrc(t *testing.T, name string) string {
	t.Helper()
	blob, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(blob)
}

// extractRefusal returns the source text of the message starting at needle, up to the end of the Go
// string concatenation that builds it. Crude on purpose: it reads the SOURCE of one message rather
// than the whole file, so an assertion cannot be satisfied by a matching phrase somewhere unrelated.
func extractRefusal(t *testing.T, src, needle string) string {
	t.Helper()
	i := strings.Index(src, needle)
	if i < 0 {
		t.Fatalf("could not find the refusal starting %q — it was renamed or removed; this test must be "+
			"re-pointed deliberately rather than left passing vacuously", needle)
	}
	rest := src[i:]
	if j := strings.Index(rest, ")\n"); j > 0 {
		return rest[:j]
	}
	return rest
}
