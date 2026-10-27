package argus

import (
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// The example check shipped in the docs (a msgbus-shaped two-call write) is a VALID scenario.
const signedExampleScenario = "# Scenario: A member sends a message with a per-call signature\n\n" +
	"## Metadata\n- **ID**: SIGN-001\n- **Layer**: Permissions\n- **Tags**: chain\n\n" +
	"## TRIGGER\nPOST `chain`\n\n```json\n" +
	`{"steps":[
 {"type":"mcp","name":"dry","tool":"send_message","args":{"messageId":"m-${cid}","toInbox":"agent.b","prompt":"hello","dryRun":true,"keyId":"key-a"},
  "save":{"canonical":"canonical"}},
 {"type":"mcp","name":"send","tool":"send_message","args":{"messageId":"m-${cid}","toInbox":"agent.b","prompt":"hello","signatures":{}},
  "sign":{"key":"MEMBER_A_KEY","key_id":"key-a","key_id_header":"x-msgbus-key-id","value":"${saved.canonical}","arg":"/signatures/agent.b"}},
 {"type":"mcp","name":"send-as-b","tool":"send_message","args":{"messageId":"n-${cid}","toInbox":"agent.a","prompt":"hi"},
  "sign":{"key":"MEMBER_B_KEY","key_id":"key-b","key_id_header":"x-msgbus-key-id","separator":"|",
          "fields":[{"literal":"v4"},{"path":"messageId"},{"path":"toInbox"},{"path":"prompt","sha256":"hex"}],"arg":"/signatures/agent.a"}}]}` +
	"\n```\n\n## EXPECT\n### Runnable\n- step dry: body contains canonical\n- step send: body contains accepted\n- step send-as-b: body contains accepted\n\n" +
	"### Non-runnable\nThe system accepts a message only when the signature verifies against the key id in the header.\n\n" +
	"## TIMEOUT\n60s\n\n## CLEANUP\nN/A - the messages are addressed to test inboxes and expire on their own.\n"

func TestSignedExampleScenarioIsValid(t *testing.T) {
	s, errs := scenario.Validate(signedExampleScenario)
	if len(errs) != 0 {
		t.Fatalf("the documented example must validate: %+v", errs)
	}
	if got := CheckSigningKeys(s, signCfg(t, "http://127.0.0.1:1", "MEMBER_A_KEY", "MEMBER_B_KEY")); len(got) != 0 {
		t.Fatalf("declared keys: %v", got)
	}
	if got := CheckSigningKeys(s, signCfg(t, "http://127.0.0.1:1", "MEMBER_A_KEY")); len(got) != 1 || !strings.Contains(got[0], "MEMBER_B_KEY") {
		t.Fatalf("an undeclared second key must be named: %v", got)
	}
}
