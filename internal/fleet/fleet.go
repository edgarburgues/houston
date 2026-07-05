// Package fleet applies configuration across every Houston account at once.
// The shared store already propagates FILES (skills, plugin installs, agents…)
// via junctions; what never propagated was per-account CONFIG: user-scoped MCP
// servers live in each account's .claude.json and plugin enablement in each
// account's settings.json. fleet's pattern for add-operations is
// passthrough-then-propagate: the real `claude` CLI runs ONCE against a source
// account (full flag parity, real validation and downloads), and the resulting
// config diff is copied surgically into every other account.
package fleet

import (
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"

	"houston/internal/accounts"
	"houston/internal/jsonedit"
	"houston/internal/launch"
)

func claudeJSONPath(a accounts.Account) string {
	return filepath.Join(a.ResolveConfigDir(), ".claude.json")
}

func settingsPath(a accounts.Account) string {
	return filepath.Join(a.ResolveConfigDir(), "settings.json")
}

// RunClaude runs the real claude CLI once against the account's config dir
// with inherited stdio, so its own prompts/errors reach the user directly.
func RunClaude(a accounts.Account, args ...string) error {
	return launch.Cmd(a.ResolveConfigDir(), args, "").Run()
}

// --- user-scope MCP servers (.claude.json → mcpServers) ---------------------

// MCPServers reads an account's user-scope MCP servers (empty map if none —
// including when the account was never initialized).
func MCPServers(a accounts.Account) (map[string]json.RawMessage, error) {
	b, err := readJSON(claudeJSONPath(a))
	if err != nil {
		return map[string]json.RawMessage{}, nil // never logged in / no file yet
	}
	return jsonedit.SubObject(b, "mcpServers")
}

// PatchMCP upserts the given server entries into an account's user scope and
// removes the named ones. The account's .claude.json is created if missing so
// a not-yet-logged-in account still receives the config.
func PatchMCP(a accounts.Account, set map[string]json.RawMessage, remove []string) error {
	if len(set) == 0 && len(remove) == 0 {
		return nil
	}
	return jsonedit.Patch(claudeJSONPath(a), true, func(obj map[string]json.RawMessage) error {
		sub, err := jsonedit.SubObject(obj, "mcpServers")
		if err != nil {
			return err
		}
		for k, v := range set {
			sub[k] = v
		}
		for _, k := range remove {
			delete(sub, k)
		}
		return jsonedit.SetSubObject(obj, "mcpServers", sub)
	})
}

// --- plugin enablement (settings.json → enabledPlugins) ---------------------

// EnabledPlugins reads an account's plugin-enablement map (empty if none).
func EnabledPlugins(a accounts.Account) (map[string]json.RawMessage, error) {
	b, err := readJSON(settingsPath(a))
	if err != nil {
		return map[string]json.RawMessage{}, nil
	}
	return jsonedit.SubObject(b, "enabledPlugins")
}

// PatchPlugins upserts/removes entries in an account's enabledPlugins.
func PatchPlugins(a accounts.Account, set map[string]json.RawMessage, remove []string) error {
	if len(set) == 0 && len(remove) == 0 {
		return nil
	}
	return jsonedit.Patch(settingsPath(a), true, func(obj map[string]json.RawMessage) error {
		sub, err := jsonedit.SubObject(obj, "enabledPlugins")
		if err != nil {
			return err
		}
		for k, v := range set {
			sub[k] = v
		}
		for _, k := range remove {
			delete(sub, k)
		}
		return jsonedit.SetSubObject(obj, "enabledPlugins", sub)
	})
}

// MatchPluginKeys returns the keys matching a user-supplied plugin spec:
// exact "name@marketplace" first, else every key whose name part equals spec.
func MatchPluginKeys(spec string, keys []string) []string {
	var out []string
	for _, k := range keys {
		if k == spec || strings.SplitN(k, "@", 2)[0] == spec {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// --- diffing -----------------------------------------------------------------

// Diff compares two config maps: changed holds keys whose value in after is
// new or different from before; removed holds keys that disappeared.
func Diff(before, after map[string]json.RawMessage) (changed map[string]json.RawMessage, removed []string) {
	changed = map[string]json.RawMessage{}
	for k, v := range after {
		if old, ok := before[k]; !ok || string(old) != string(v) {
			changed[k] = v
		}
	}
	for k := range before {
		if _, ok := after[k]; !ok {
			removed = append(removed, k)
		}
	}
	sort.Strings(removed)
	return changed, removed
}

// Keys returns the sorted union of the maps' keys.
func Keys(ms ...map[string]json.RawMessage) []string {
	seen := map[string]bool{}
	for _, m := range ms {
		for k := range m {
			seen[k] = true
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func readJSON(path string) (map[string]json.RawMessage, error) {
	var obj map[string]json.RawMessage
	err := jsonedit.Read(path, &obj)
	return obj, err
}
