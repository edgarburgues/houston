package launch

import "testing"

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
