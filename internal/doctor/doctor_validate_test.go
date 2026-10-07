package doctor

import (
	"strings"
	"testing"
	"time"
)

// F2 — a cloud tester holds a control-plane token in ARGUS_TOKEN and no role map. That is a configuration
// the cloud-* commands are happy with, not a failure; the local verbs just are not set up.
func TestCheckLocalHats_ControlPlaneTokenOnlyIsWarn(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	cpJWT := jwt(t, map[string]any{"iss": "https://argus-dev.onedroid.ai", "scope": "author", "exp": now.Add(time.Hour).Unix()})
	for name, tok := range map[string]string{"odts_ PAT": "odts_" + "planted", "control-plane JWT": cpJWT} {
		t.Run(name, func(t *testing.T) {
			c := CheckLocalHats(HatsFromValues(tok, "", ""))
			if c.Status != StatusWarn {
				t.Fatalf("status = %q, want warn; detail: %s", c.Status, c.Detail)
			}
			for _, s := range []string{"control-plane token", "cloud-*", "local verbs"} {
				if !strings.Contains(c.Detail, s) {
					t.Errorf("detail lacks %q: %s", s, c.Detail)
				}
			}
			if !strings.Contains(c.Fix, "ARGUS_RUNNER_TOKEN") || !strings.Contains(c.Fix, "ARGUS_EXECUTOR_SECRET") {
				t.Errorf("fix is not the role-map export: %s", c.Fix)
			}
			if strings.Contains(c.Subject+c.Detail+c.Fix, tok) {
				t.Errorf("the token value leaked: %+v", c)
			}
		})
	}
}

func TestCheckLocalHats_ControlPlaneTokenStillFailsWhenTheMapIsHalfSet(t *testing.T) {
	pat := "odts_" + "planted"
	for name, c := range map[string]Check{
		"runner hat only":  CheckLocalHats(HatsFromValues(pat, "r", "")),
		"author hat only":  CheckLocalHats(HatsFromValues(pat, "", "a")),
		"hats equal":       CheckLocalHats(HatsFromValues(pat, "same", "same")),
		"non-cp, no map":   CheckLocalHats(HatsFromValues("some-string", "", "")),
		"unrecognised dot": CheckLocalHats(HatsFromValues("a.b.c", "", "")),
	} {
		if c.Status != StatusFail {
			t.Errorf("%s: status = %q, want fail; detail: %s", name, c.Status, c.Detail)
		}
	}
	// a full map with the PAT presented still goes through the real verifier
	if c := CheckLocalHats(HatsFromValues(pat, "r", "a")); c.Status != StatusFail || !strings.Contains(c.Detail, "neither hat") {
		t.Errorf("a PAT presented against a full map must be judged by the verifier: %+v", c)
	}
}

// F3 — doctor's own GET /api/instances with the credential answered: the credential check says so.
func TestCheckCredential_AcceptedByTheControlPlane(t *testing.T) {
	const cp = "https://argus-dev.onedroid.ai"
	in := CredentialInput{Source: "ARGUS_CP_AUTHOR_TOKEN", Present: true, Facts: Classify("odts_" + "planted"), ControlPlaneURL: cp,
		Now: time.Now()}

	notAsked := CheckCredential(in)
	if !strings.Contains(notAsked.Detail, "Not verified against the control plane") {
		t.Errorf("when doctor did not ask, the wording must stay: %s", notAsked.Detail)
	}

	in.Accepted = true
	got := CheckCredential(in)
	if got.Status != StatusOK {
		t.Fatalf("status = %q: %s", got.Status, got.Detail)
	}
	if strings.Contains(strings.ToLower(got.Detail), "not verified") || strings.Contains(got.Detail, "first cloud call proves") {
		t.Errorf("an accepted credential must not be called unverified: %s", got.Detail)
	}
	for _, s := range []string{cp, "accepted", "GET /api/instances"} {
		if !strings.Contains(got.Detail, s) {
			t.Errorf("detail lacks %q: %s", s, got.Detail)
		}
	}
}

// A warn is not a failure: the verdict is "warn" and the exit code is 0. Printing it as FAIL (as Line did)
// told a cloud tester "FAIL local-hats" on a run that exits 0, a doctor crying wolf. unknown stays FAIL:
// not knowing is not a pass.
func TestLineWordMatchesTheVerdictItContributes(t *testing.T) {
	for st, want := range map[Status]string{
		StatusOK: "PASS ", StatusWarn: "WARN ", StatusFail: "FAIL ", StatusUnknown: "FAIL ", StatusSkip: "SKIP ",
	} {
		c := Check{ID: "x", Phase: "P5", Subject: "s", Status: st, Detail: "d", Fix: "f"}
		if got := c.Line(); !strings.HasPrefix(got, want) {
			t.Errorf("status %s: line %q, want prefix %q", st, got, want)
		}
	}
}
