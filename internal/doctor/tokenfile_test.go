package doctor

import (
	"strings"
	"testing"
	"time"
)

// tokenFile is a readable file holding one value, classified the way the caller classifies it.
func tokenFile(path string, f TokenFacts, forSide string, now time.Time) TokenFileInput {
	return TokenFileInput{Path: path, Values: 1, Facts: f, For: forSide, ControlPlaneURL: "https://argus-dev.onedroid.ai", Now: now}
}

// REPLAY: one case per recorded incident this check claims (verify/2026-09-25-friction-investigation,
// incidents-B-tester.tsv; tagging in verify/2026-09-28-doctor-incident-tagging, extend:control-plane-credential).
// B:16 is claimed by the credential check, not this one: see TestCheckCredential_ReplayB16.
func TestCheckTokenFile_Replay(t *testing.T) {
	now := time.Date(2026, 9, 25, 13, 22, 40, 0, time.UTC)
	// The recorded state: a Tokens-page author PAT (13:22:33Z), wrapped for the builder, whose side needs a runner token.
	mixUp := tokenFile("/home/user/.config/argus/new-token.txt", Classify("odts_minted-on-the-tokens-page"), "runner", now)
	cases := []struct {
		row, source string
		in          TokenFileInput
	}{
		{"B:66", `handoffs/2026-09-25-hub-runner-window-mixup.md:19-20 "a Tokens-page revoke (13:22:19Z) + new author PAT (13:22:33Z) went out in the wrap instead of runner-token.txt"`, mixUp},
		{"B:71", `handoffs/2026-09-25-hub-runner-window-mixup.md:8 "Hub runner window lost to a token mix-up" (the consequence of B:66)`, mixUp},
		{"B:78", `NOTES.md:13-14 (09-25) "a Tokens-page revoke + new author PAT went out instead of runner-token.txt" (recurrence of B:66)`, mixUp},
	}
	for _, tc := range cases {
		t.Run(tc.row, func(t *testing.T) {
			c := CheckTokenFile(tc.in)
			if c.ID != "token-file" {
				t.Fatalf("id = %q", c.ID)
			}
			// Not ok: the file is not what `cloud-login --scope runner --token-out` writes, and nothing local can
			// say it is a builder token. Not fail either: a builder token IS an odts_ PAT (#259).
			if c.Status != StatusWarn {
				t.Errorf("%s: status = %q, want warn\nsource: %s\ndetail: %s", tc.row, c.Status, tc.source, c.Detail)
			}
			if !strings.Contains(c.Subject, tc.in.Path) || !strings.Contains(c.Subject, "odts_") {
				t.Errorf("%s: subject does not say which file and what it holds: %s", tc.row, c.Subject)
			}
			for _, s := range []string{"not the runner access token", "#259", "Tokens page", "author PAT"} {
				if !strings.Contains(c.Detail, s) {
					t.Errorf("%s: detail does not say %q\nsource: %s\ndetail: %s", tc.row, s, tc.source, c.Detail)
				}
			}
			for _, s := range []string{"--scope runner", "--token-out /home/user/.config/argus/runner-token.txt", "ARGUS_SESSION_FILE="} {
				if !strings.Contains(c.Fix, s) {
					t.Errorf("%s: fix does not say %q\nfix: %s", tc.row, s, c.Fix)
				}
			}
			// The Tokens page shows a PAT once: a fix that writes over the file would destroy it.
			if strings.Contains(c.Fix, "--token-out "+tc.in.Path) {
				t.Errorf("%s: fix writes over the PAT it has just found: %s", tc.row, c.Fix)
			}
		})
	}
	// The control: the file that SHOULD have gone out that afternoon — runner-token.txt, minted by
	// `cloud-login --scope runner --token-out`, 15 minutes to live.
	t.Run("control: runner-token.txt for the runner side is ok", func(t *testing.T) {
		right := tokenFile("/home/user/.config/argus/runner-token.txt", Classify(jwt(t, map[string]any{"scope": "runner", "exp": now.Add(12 * time.Minute).Unix()})), "runner", now)
		c := CheckTokenFile(right)
		if c.Status != StatusOK {
			t.Fatalf("status = %q, want ok; detail: %s", c.Status, c.Detail)
		}
		for _, s := range []string{"runner__run accepts it", "12m", "nothing renews"} {
			if !strings.Contains(c.Detail, s) {
				t.Errorf("detail does not say %q: %s", s, c.Detail)
			}
		}
	})
}

func TestCheckTokenFile(t *testing.T) {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	const path = "/home/x/token.txt"
	jwtIn := func(scope string, left time.Duration) TokenFacts {
		return Classify(jwt(t, map[string]any{"scope": scope, "exp": now.Add(left).Unix()}))
	}
	pat := Classify("odts_x")
	cases := []struct {
		name   string
		in     TokenFileInput
		want   Status
		detail []string // every one must appear in Detail
		fix    []string // every one must appear in Fix
	}{
		{"unreadable is unknown, never ok", TokenFileInput{Path: path, Err: "open /home/x/token.txt: permission denied", Now: now},
			StatusUnknown, []string{"permission denied"}, []string{path}},
		{"empty file", TokenFileInput{Path: path, Values: 0, Now: now}, StatusFail, []string{"empty"}, []string{"cloud-login"}},
		{"two values", TokenFileInput{Path: path, Values: 2, Facts: TokenFacts{Kind: KindUnrecognised}, Now: now},
			StatusFail, []string{"2 values"}, []string{"one token"}},
		{"runner token, expired", tokenFile(path, jwtIn("runner", -20*time.Minute), "runner", now),
			StatusFail, []string{"expired 20m ago", "401"}, []string{"--scope runner", "--token-out " + path}},
		{"runner token, two minutes left", tokenFile(path, jwtIn("runner", 2*time.Minute), "", now),
			StatusWarn, []string{"expires in 2m", "Vault wrap"}, []string{"--scope runner"}},
		{"author-scope access token meant for the runner side", tokenFile(path, jwtIn("author", 10*time.Minute), "runner", now),
			StatusFail, []string{"AUTHOR-scope", "runner__run refuses it"}, []string{"--scope runner", "--token-out /home/x/runner-token.txt"}},
		{"runner token meant for the author side", tokenFile(path, jwtIn("runner", 10*time.Minute), "author", now),
			StatusFail, []string{"RUNNER-scope", "author__* tools refuse it"}, []string{"Tokens page", "--token-for author", "/home/x/author-token.txt"}},
		{"author-scope access token for the author side lasts 15 minutes", tokenFile(path, jwtIn("author", 10*time.Minute), "author", now),
			StatusOK, []string{"author__* tools accept it", "10m", "Tokens page"}, nil},
		{"PAT for the author side", tokenFile(path, pat, "author", now),
			StatusOK, []string{"author PAT", "builder token", "Tokens page"}, nil},
		{"PAT, side not said", tokenFile(path, pat, "", now),
			StatusOK, []string{"author PAT", "builder token", "#259"}, nil},
		{"no scope claim", tokenFile(path, Classify(jwt(t, map[string]any{"exp": now.Add(time.Hour).Unix()})), "", now),
			StatusWarn, []string{"no scope claim"}, []string{"cloud-login"}},
		{"unrecognised", tokenFile(path, Classify("wksp_123"), "runner", now),
			StatusWarn, []string{"neither"}, []string{"--scope runner", "--token-out /home/x/runner-token.txt"}},
		{"unreadable claims are unknown", tokenFile(path, Classify(enc([]byte(`{"alg":"RS256"}`))+".!!!.sig"), "", now),
			StatusUnknown, []string{"could not be read"}, []string{"cloud-login"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := CheckTokenFile(tc.in)
			if c.Status != tc.want {
				t.Fatalf("status = %q, want %q\n  subject: %s\n  detail: %s", c.Status, tc.want, c.Subject, c.Detail)
			}
			if !strings.Contains(c.Subject, path) {
				t.Errorf("subject does not name the file: %s", c.Subject)
			}
			for _, s := range tc.detail {
				if !strings.Contains(c.Detail, s) {
					t.Errorf("detail lacks %q: %s", s, c.Detail)
				}
			}
			for _, s := range tc.fix {
				if !strings.Contains(c.Fix, s) {
					t.Errorf("fix lacks %q: %s", s, c.Fix)
				}
			}
			if c.Status != StatusOK && c.Fix == "" {
				t.Errorf("a non-ok check has no Fix: %+v", c)
			}
		})
	}
}

// A PAT saved as runner-token.txt (the B:66 mix-up under the right name) must not be written over either.
func TestKeepAsideNeverNamesTheFileItKeeps(t *testing.T) {
	for path, want := range map[string]string{
		"/home/x/new-token.txt":             "/home/x/runner-token.txt",
		"/home/x/runner-token.txt":          "/home/x/runner-token.txt.new",
		"/home/x/./sub/../runner-token.txt": "/home/x/runner-token.txt.new",
	} {
		if got := keepAside(path, runnerTokenFile); got != want {
			t.Errorf("keepAside(%q) = %q, want %q", path, got, want)
		}
	}
}
