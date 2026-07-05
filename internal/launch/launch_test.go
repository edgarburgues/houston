package launch

import (
	"strings"
	"testing"
)

func TestStripEnvRemovesInheritedConfigDir(t *testing.T) {
	in := []string{"PATH=x", "CLAUDE_CONFIG_DIR=old", "claude_config_dir=alsoOld", "FOO=1"}
	out := stripEnv(in, "CLAUDE_CONFIG_DIR")
	for _, e := range out {
		if len(e) >= len("CLAUDE_CONFIG_DIR=") && (e == "CLAUDE_CONFIG_DIR=old" || e == "claude_config_dir=alsoOld") {
			t.Fatalf("CLAUDE_CONFIG_DIR should have been stripped (any casing): %q", e)
		}
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 (PATH, FOO), got %d: %v", len(out), out)
	}
}

func TestCmdRespectsUserBrowser(t *testing.T) {
	// t.Setenv defines BROWSER in the inherited environment: Houston must
	// respect it instead of appending a second entry pointing at itself.
	t.Setenv("BROWSER", "my-opener")
	c := Cmd("", nil, "")
	for _, e := range c.Env {
		if strings.EqualFold(e, "BROWSER=my-opener") {
			return // user's opener survived
		}
		if strings.HasPrefix(strings.ToUpper(e), "BROWSER=") && strings.Contains(strings.ToLower(e), "houston") {
			t.Fatalf("user-set BROWSER should win, got %q", e)
		}
	}
}

func TestHasEnvCaseInsensitive(t *testing.T) {
	env := []string{"browser=x", "PATH=y"}
	if !hasEnv(env, "BROWSER") {
		t.Fatal("hasEnv should match case-insensitively")
	}
	if hasEnv([]string{"PATH=y"}, "BROWSER") {
		t.Fatal("hasEnv false positive")
	}
}
