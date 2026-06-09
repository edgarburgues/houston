package launch

import "testing"

func TestStripEnvRemovesInheritedConfigDir(t *testing.T) {
	in := []string{"PATH=x", "CLAUDE_CONFIG_DIR=old", "claude_config_dir=alsoOld", "FOO=1"}
	out := stripEnv(in, "CLAUDE_CONFIG_DIR")
	for _, e := range out {
		if len(e) >= len("CLAUDE_CONFIG_DIR=") && (e == "CLAUDE_CONFIG_DIR=old" || e == "claude_config_dir=alsoOld") {
			t.Fatalf("debería haber quitado CLAUDE_CONFIG_DIR (cualquier caja): %q", e)
		}
	}
	if len(out) != 2 {
		t.Fatalf("esperaba 2 (PATH, FOO), hay %d: %v", len(out), out)
	}
}
