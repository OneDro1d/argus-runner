package config

import (
	"strings"
	"testing"
)

// only the exact value `never` is accepted. A key that is WRITTEN with any other value,
// including an empty one, is refused at load: a guard its author typed and left blank must not read as
// "no guard".
func TestLoadTest_EveryOtherWrittenValueIsRefusedAtLoad(t *testing.T) {
	for _, v := range []string{"Never", "NEVER", "no", "false", "off", `""`, "", "~", "[never]", "{a: b}", " never x"} {
		_, err := loadFrom(t, ttBase+"test_targets:\n  - name: live\n    load_test: "+v+"\n    match: {tags: [x]}\n")
		if err == nil {
			t.Errorf("load_test: %q was accepted", v)
			continue
		}
		if !strings.Contains(err.Error(), "load_test") {
			t.Errorf("load_test: %q: the refusal does not name the key: %v", v, err)
		}
	}
}

// A load check that belongs to a never target by TAG (no prefix matches) is refused like one that belongs by prefix.
func TestLoadTest_NeverRefusalByTagMapping(t *testing.T) {
	c, err := loadFrom(t, ttBase+"test_targets:\n  - name: live\n    load_test: never\n    match: {tags: [http]}\n")
	if err != nil {
		t.Fatal(err)
	}
	if r := c.NeverLoadRefusal(httpLoadScenario("Z-001")); !strings.Contains(r, `"live"`) {
		t.Errorf("a load check mapped by tag to a never target was not refused: %q", r)
	}
	if r := c.NeverLoadRefusal(plainScenario("Z-002")); r != "" {
		t.Errorf("a non-load check was refused: %s", r)
	}
}
