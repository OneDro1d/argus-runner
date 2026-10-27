package router

import (
	"encoding/json"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/mcpserver"
)

// VR2-13 / TS2-12 (V15-007) — the router answers with EXACTLY ONE envelope.
//
// ── WHAT WAS WRONG ────────────────────────────────────────────────────────────────────────────────
//
// The router used to hand an upstream's COMPLETE JSON-RPC envelope to mcpserver.Ok, which marshals whatever it is given
// into the text of a content block — an envelope inside an envelope, and every caller unwrapped twice:
//
//	{"result":{"content":[{"text":"{\"result\":{\"content\":[{\"text\":\"{...the real answer...}\"}]}}"}]}}
//
// Measured live 2026-08-12: exactly 2 envelopes deep on BOTH compose and k3d. Since AC-D70 (decision 14) the router no
// longer wraps a runner answer at all: it passes the executor's own result back as ITS result — so the answer through the
// router is byte for byte the executor's, one envelope deep, with the tool's own isError.
//
// ── WHY THE DEPTH IS COUNTED AND NOT EYEBALLED ────────────────────────────────────────────────────────────────────
//
// "Looks unwrapped" is not a measurement. envelopeDepth counts the nesting the way the live probe did — decode, look for
// another envelope inside, repeat — so a change that reintroduces a layer fails here.

// envelopeDepth counts how many JSON-RPC envelopes are nested in v, the same way the live probe did.
func envelopeDepth(v any) int {
	depth := 0
	for {
		m, ok := v.(map[string]any)
		if !ok {
			return depth
		}
		res, ok := m["result"].(map[string]any)
		if !ok {
			return depth
		}
		depth++
		c, ok := res["content"].([]any)
		if !ok || len(c) == 0 {
			return depth
		}
		blk, ok := c[0].(map[string]any)
		if !ok {
			return depth
		}
		txt, ok := blk["text"].(string)
		if !ok {
			return depth
		}
		var next any
		if err := json.Unmarshal([]byte(txt), &next); err != nil {
			return depth // the text is not itself an envelope — this is the bottom
		}
		v = next
	}
}

// answerTool answers with a fixed outcome — a JSON object, a tool error, or prose.
func answerTool(name string, out mcpserver.Outcome) mcpserver.Tool {
	t := execTool(name, "answers "+name)
	t.Handler = func(json.RawMessage, mcpserver.Principal) mcpserver.Outcome { return out }
	return t
}

func TestEnvelope_TheRoutersAnswerIsTheExecutorsOwnOneEnvelopeDeep(t *testing.T) {
	ex := newStubExec(t, "0.3.73", "runner-a", "author-a")
	ex.serve("0.3.73", "runner-a", "author-a", []mcpserver.Tool{
		answerTool("runner_object", mcpserver.Ok(map[string]any{"dashboard_url": "http://localhost:3000/d/x", "note": "n"})),
		answerTool("runner_failed", mcpserver.ToolErr(map[string]any{"error": "scenario not found"})),
		answerTool("runner_prose", mcpserver.Ok("plain words, not json")),
	})
	r := newRig(t, Options{}, productFolder(pTok, map[string]Upstream{"a": {URL: ex.URL(), Token: "runner-a"}}))
	r.initialize("s", pTok)
	for _, c := range []struct {
		tool    string
		isError bool
		inner   string
	}{
		{"runner_object", false, `{"dashboard_url":"http://localhost:3000/d/x","note":"n"}`},
		// isError must NOT be flattened away: a tool that was REACHED and answered "failed" arriving as a success is
		// the silent-wrong-answer class this codebase exists to remove.
		{"runner_failed", true, `{"error":"scenario not found"}`},
		// A tool is entitled to answer in prose; it comes back as the text, not as a parse failure or a new envelope.
		{"runner_prose", false, `"plain words, not json"`},
	} {
		routed := r.call("s", pTok, 1, c.tool, map[string]any{"instance_id": "a"})
		direct := ex.direct(t, "runner-a", 1, "tools/call", map[string]any{"name": c.tool, "arguments": map[string]any{"instance_id": "local"}})
		if string(routed) != string(direct) {
			t.Fatalf("%s: the router's answer is not the executor's own\nrouter:   %s\nexecutor: %s", c.tool, routed, direct)
		}
		var v any
		_ = json.Unmarshal(routed, &v)
		if d := envelopeDepth(v); d != 1 {
			t.Fatalf("%s: %d JSON-RPC envelopes deep, want exactly 1: %s", c.tool, d, routed)
		}
		var res struct {
			Result mcpserver.ToolResult `json:"result"`
		}
		_ = json.Unmarshal(routed, &res)
		if res.Result.IsError != c.isError || len(res.Result.Content) != 1 || res.Result.Content[0].Text != c.inner {
			t.Fatalf("%s: isError %v, content %+v — want isError %v and the tool's own text %s", c.tool, res.Result.IsError, res.Result.Content, c.isError, c.inner)
		}
	}
}
