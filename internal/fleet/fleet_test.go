package fleet

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"houston/internal/accounts"
)

// compact normalizes whitespace: the patcher re-indents RawMessage bodies, so
// equality checks must be structural.
func compact(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		t.Fatalf("invalid JSON %q: %v", raw, err)
	}
	return buf.String()
}

func testAccount(t *testing.T) accounts.Account {
	t.Helper()
	return accounts.Account{ID: "t", ConfigDir: t.TempDir()}
}

func TestPatchMCPRoundTrip(t *testing.T) {
	a := testAccount(t)
	// .claude.json with unrelated state that must survive
	os.WriteFile(claudeJSONPath(a), []byte(`{"userID":"u1","hasCompletedOnboarding":true}`), 0o600)

	srv := json.RawMessage(`{"type":"stdio","command":"cmd"}`)
	if err := PatchMCP(a, map[string]json.RawMessage{"gh": srv}, nil); err != nil {
		t.Fatal(err)
	}
	got, err := MCPServers(a)
	if err != nil || compact(t, got["gh"]) != string(srv) {
		t.Fatalf("server not stored: %v err=%v", got, err)
	}
	var full map[string]json.RawMessage
	b, _ := os.ReadFile(claudeJSONPath(a))
	json.Unmarshal(b, &full)
	if compact(t, full["userID"]) != `"u1"` {
		t.Errorf("unrelated .claude.json state altered: %s", full["userID"])
	}

	if err := PatchMCP(a, nil, []string{"gh"}); err != nil {
		t.Fatal(err)
	}
	got, _ = MCPServers(a)
	if len(got) != 0 {
		t.Errorf("server should be removed: %v", got)
	}
}

func TestPatchMCPCreatesClaudeJSON(t *testing.T) {
	a := testAccount(t) // no .claude.json: account never logged in
	if err := PatchMCP(a, map[string]json.RawMessage{"x": json.RawMessage(`{}`)}, nil); err != nil {
		t.Fatal(err)
	}
	got, _ := MCPServers(a)
	if _, ok := got["x"]; !ok {
		t.Fatal("server should be written into a fresh .claude.json")
	}
}

func TestPatchPluginsKeepsSettings(t *testing.T) {
	a := testAccount(t)
	os.WriteFile(settingsPath(a), []byte(`{"model":"m","statusLine":{"type":"command","command":"houston statusline"}}`), 0o644)

	if err := PatchPlugins(a, map[string]json.RawMessage{"tg@mk": json.RawMessage(`true`)}, nil); err != nil {
		t.Fatal(err)
	}
	got, _ := EnabledPlugins(a)
	if string(got["tg@mk"]) != "true" {
		t.Fatalf("plugin not enabled: %v", got)
	}
	var full map[string]json.RawMessage
	b, _ := os.ReadFile(settingsPath(a))
	json.Unmarshal(b, &full)
	if string(full["model"]) != `"m"` {
		t.Errorf("model setting altered: %s", full["model"])
	}
	if _, ok := full["statusLine"]; !ok {
		t.Error("statusLine setting lost")
	}
}

func TestDiff(t *testing.T) {
	before := map[string]json.RawMessage{"a": json.RawMessage(`1`), "b": json.RawMessage(`2`)}
	after := map[string]json.RawMessage{"a": json.RawMessage(`1`), "b": json.RawMessage(`3`), "c": json.RawMessage(`4`)}
	changed, removed := Diff(before, after)
	if len(changed) != 2 || string(changed["b"]) != "3" || string(changed["c"]) != "4" {
		t.Errorf("changed wrong: %v", changed)
	}
	if len(removed) != 0 {
		t.Errorf("removed wrong: %v", removed)
	}
	changed, removed = Diff(after, before)
	if len(changed) != 1 || string(changed["b"]) != "2" || !reflect.DeepEqual(removed, []string{"c"}) {
		t.Errorf("reverse diff wrong: %v %v", changed, removed)
	}
}

func TestMatchPluginKeys(t *testing.T) {
	keys := []string{"telegram@official", "telegram@fork", "other@official"}
	if got := MatchPluginKeys("telegram", keys); !reflect.DeepEqual(got, []string{"telegram@fork", "telegram@official"}) {
		t.Errorf("name match wrong: %v", got)
	}
	if got := MatchPluginKeys("telegram@official", keys); !reflect.DeepEqual(got, []string{"telegram@official"}) {
		t.Errorf("exact match wrong: %v", got)
	}
	if got := MatchPluginKeys("nope", keys); got != nil {
		t.Errorf("no match expected: %v", got)
	}
}

func TestInstallSkillFromLocalDir(t *testing.T) {
	t.Setenv("HOUSTON_SHARED_DIR", t.TempDir())
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "SKILL.md"), []byte("# demo"), 0o644)
	os.MkdirAll(filepath.Join(src, "assets"), 0o755)
	os.WriteFile(filepath.Join(src, "assets", "a.txt"), []byte("x"), 0o644)

	dst, err := InstallSkill(SkillSource{Local: src, Name: "demo"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dst, "assets", "a.txt")); err != nil || string(b) != "x" {
		t.Fatalf("tree not copied: %v", err)
	}
	// second install without --force must refuse
	if _, err := InstallSkill(SkillSource{Local: src, Name: "demo"}, false); err == nil {
		t.Fatal("reinstall without force should error")
	}
	if _, err := InstallSkill(SkillSource{Local: src, Name: "demo"}, true); err != nil {
		t.Fatalf("force reinstall failed: %v", err)
	}

	names, _ := ListSkills()
	if !reflect.DeepEqual(names, []string{"demo"}) {
		t.Fatalf("ListSkills = %v", names)
	}
	if err := RemoveSkill("demo"); err != nil {
		t.Fatal(err)
	}
	if names, _ := ListSkills(); len(names) != 0 {
		t.Fatalf("skill should be gone, got %v", names)
	}
}

func TestInstallSkillRequiresSkillMD(t *testing.T) {
	t.Setenv("HOUSTON_SHARED_DIR", t.TempDir())
	if _, err := InstallSkill(SkillSource{Local: t.TempDir(), Name: "x"}, false); err == nil {
		t.Fatal("dir without SKILL.md should be rejected")
	}
}

func TestSkillNameValidation(t *testing.T) {
	for _, bad := range []string{"", ".", "..", `a/b`, `a\b`, "../evil"} {
		if err := validateSkillName(bad); err == nil {
			t.Errorf("name %q should be rejected", bad)
		}
	}
	if err := validateSkillName("good-skill"); err != nil {
		t.Errorf("valid name rejected: %v", err)
	}
	if err := RemoveSkill("../evil"); err == nil {
		t.Error("traversal in RemoveSkill should be rejected")
	}
}

func TestValidateGitSource(t *testing.T) {
	for _, bad := range []string{"", "-oProxyCommand=calc", "ext::sh -c calc", "fd::7", "file:///etc", "FILE://x"} {
		if err := validateGitSource(bad); err == nil {
			t.Errorf("source %q should be rejected", bad)
		}
	}
	for _, ok := range []string{"https://github.com/x/y", "git@github.com:x/y.git"} {
		if err := validateGitSource(ok); err != nil {
			t.Errorf("source %q should be allowed: %v", ok, err)
		}
	}
}
