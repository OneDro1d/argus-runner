package router

import "testing"

// TS-C5 — the two audiences. The AGENT types the doubled name it already knows from `runner__*`; the
// CLOUD publishes the bare one so a gateway that joins its namespace with "__" produces exactly one
// separator. The mapping happens at the forward boundary and NOWHERE else, so these tests pin both the
// translation and — just as important — the cases that must NOT be translated.
func TestCloudToolName_AuthorToolsGoBareToTheCloud(t *testing.T) {
	for _, tool := range AuthorTools {
		got := CloudToolName(tool, "author")
		if got == tool {
			t.Fatalf("%s was forwarded unchanged; the cloud publishes the bare name", tool)
		}
		if want := "author_" + tool[len("author__"):]; got != want {
			t.Fatalf("CloudToolName(%q) = %q, want %q", tool, got, want)
		}
		// The regression this exists to catch: a doubled separator surviving into the cloud call.
		for i := 0; i+1 < len(got); i++ {
			if got[i] == '_' && got[i+1] == '_' {
				t.Fatalf("CloudToolName(%q) = %q still contains a doubled separator", tool, got)
			}
		}
	}
}

func TestCloudToolName_RunnerToolsAreNeverRewritten(t *testing.T) {
	// The runner plane is in-env and has no gateway in front of it. Rewriting a runner name would
	// send the executor a tool it does not serve, which is a 404 the agent cannot diagnose.
	for _, tool := range RunnerTools {
		if got := CloudToolName(tool, "runner"); got != tool {
			t.Fatalf("CloudToolName(%q, runner) = %q, want it unchanged", tool, got)
		}
		// Even on the author plane, a runner name has no author__ prefix to rewrite.
		if got := CloudToolName(tool, "author"); got != tool {
			t.Fatalf("CloudToolName(%q, author) = %q, want it unchanged", tool, got)
		}
	}
}

func TestCloudToolName_RewritesOnlyTheLeadingPrefix(t *testing.T) {
	// A name that merely CONTAINS author__ later on must keep it: only the plane prefix is a
	// separator artifact. strings.Replace with n=1 is right for the leading case and wrong if it
	// ever starts scanning from anywhere else.
	if got := CloudToolName("author__write_author__scenario", "author"); got != "author_write_author__scenario" {
		t.Fatalf("got %q; only the LEADING prefix may be rewritten", got)
	}
	if got := CloudToolName("", "author"); got != "" {
		t.Fatalf("empty tool name became %q", got)
	}
}
