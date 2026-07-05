package jsonedit

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// compact normalizes whitespace: MarshalIndent re-indents RawMessage bodies,
// so equality checks must be structural, not byte-for-byte.
func compact(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		t.Fatalf("invalid JSON %q: %v", raw, err)
	}
	return buf.String()
}

func TestPatchPreservesUnrelatedFields(t *testing.T) {
	p := filepath.Join(t.TempDir(), "cfg.json")
	seed := `{"userID":"abc","weird":{"nested":[1,2,{"x":null}]},"num":1e3}`
	if err := os.WriteFile(p, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	err := Patch(p, false, func(obj map[string]json.RawMessage) error {
		sub, err := SubObject(obj, "mcpServers")
		if err != nil {
			return err
		}
		sub["srv"] = json.RawMessage(`{"type":"stdio"}`)
		return SetSubObject(obj, "mcpServers", sub)
	})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := Read(p, &got); err != nil {
		t.Fatal(err)
	}
	if compact(t, got["userID"]) != `"abc"` {
		t.Errorf("userID altered: %s", got["userID"])
	}
	if compact(t, got["weird"]) != `{"nested":[1,2,{"x":null}]}` {
		t.Errorf("nested raw value altered: %s", got["weird"])
	}
	if compact(t, got["num"]) != "1e3" {
		t.Errorf("exotic number literal should survive as raw JSON: %s", got["num"])
	}
	sub, _ := SubObject(got, "mcpServers")
	if compact(t, sub["srv"]) != `{"type":"stdio"}` {
		t.Errorf("patch not applied: %v", sub)
	}
}

func TestPatchCreatesFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "new.json")
	err := Patch(p, true, func(obj map[string]json.RawMessage) error {
		obj["a"] = json.RawMessage(`1`)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := Read(p, &got); err != nil || string(got["a"]) != "1" {
		t.Fatalf("created file wrong: %v err=%v", got, err)
	}
}

func TestPatchMissingWithoutCreate(t *testing.T) {
	p := filepath.Join(t.TempDir(), "absent.json")
	if err := Patch(p, false, func(map[string]json.RawMessage) error { return nil }); err == nil {
		t.Fatal("missing file without create should error")
	}
}

func TestSetSubObjectRemovesEmpty(t *testing.T) {
	obj := map[string]json.RawMessage{"enabledPlugins": json.RawMessage(`{"x":true}`)}
	if err := SetSubObject(obj, "enabledPlugins", map[string]json.RawMessage{}); err != nil {
		t.Fatal(err)
	}
	if _, ok := obj["enabledPlugins"]; ok {
		t.Error("empty sub-object should remove the key")
	}
}

func TestPatchReleasesLock(t *testing.T) {
	p := filepath.Join(t.TempDir(), "cfg.json")
	os.WriteFile(p, []byte(`{}`), 0o600)
	for i := 0; i < 3; i++ { // a leaked lock would wedge the second pass
		if err := Patch(p, false, func(map[string]json.RawMessage) error { return nil }); err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
	}
	if _, err := os.Stat(p + ".lock"); !os.IsNotExist(err) {
		t.Error("lock file leaked")
	}
}
