// Package jsonedit applies surgical edits to JSON config files that other
// programs own (Claude Code's .claude.json and settings.json): read → patch
// the top-level object → atomic write, under an advisory lock. Unrelated
// fields are carried as raw JSON, so their VALUES survive exactly (numbers
// keep their literal form, unknown fields are never dropped); the file is
// re-indented and keys re-sorted, which matches how Claude Code itself
// rewrites these files.
package jsonedit

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"houston/internal/flock"
)

// lockWait caps how long a patch waits for another writer of the same file.
const lockWait = 5 * time.Second

// Patch edits the top-level object of the JSON file at path under an advisory
// lock. If the file is missing and create is true, fn starts from an empty
// object; a missing file with create=false is an error. fn mutates the map in
// place. The result is written via unique-temp+rename, keeping the original
// file mode (0600 for new files — these configs can embed tokens).
func Patch(path string, create bool, fn func(obj map[string]json.RawMessage) error) error {
	lk, err := flock.Acquire(path+".lock", lockWait)
	if err != nil {
		return fmt.Errorf("config busy in another process: %w", err)
	}
	defer lk.Release()

	perm := fs.FileMode(0o600)
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		if fi, serr := os.Stat(path); serr == nil {
			perm = fi.Mode().Perm()
		}
	case os.IsNotExist(err) && create:
		b = []byte("{}")
	default:
		return err
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(b, &obj); err != nil {
		return fmt.Errorf("%s is not a JSON object: %w", path, err)
	}
	if obj == nil {
		obj = map[string]json.RawMessage{}
	}
	if err := fn(obj); err != nil {
		return err
	}
	out, err := json.MarshalIndent(obj, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(path, append(out, '\n'), perm)
}

// Read unmarshals the JSON file at path into v (no lock: reads are atomic
// thanks to the rename-based writers).
func Read(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// SubObject decodes the object stored under key ({} if the key is absent or
// null). Errors if the key holds a non-object.
func SubObject(obj map[string]json.RawMessage, key string) (map[string]json.RawMessage, error) {
	sub := map[string]json.RawMessage{}
	raw, ok := obj[key]
	if !ok || string(raw) == "null" {
		return sub, nil
	}
	if err := json.Unmarshal(raw, &sub); err != nil {
		return nil, fmt.Errorf("%q is not a JSON object: %w", key, err)
	}
	return sub, nil
}

// SetSubObject stores sub under key (removing the key entirely when sub is
// empty keeps configs tidy).
func SetSubObject(obj map[string]json.RawMessage, key string, sub map[string]json.RawMessage) error {
	if len(sub) == 0 {
		delete(obj, key)
		return nil
	}
	raw, err := json.Marshal(sub)
	if err != nil {
		return err
	}
	obj[key] = raw
	return nil
}

// writeAtomic writes b via a uniquely-named same-dir temp file + rename.
func writeAtomic(path string, b []byte, perm fs.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}
