package tui

// Module mission-list transforms and preview sections: async dispatch,
// generation bookkeeping, patch retention and the debounced preview fetch.
// All state mutation happens inside Update (Bubble Tea's single-goroutine
// guarantee); the goroutines here only ever talk back through messages.

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"houston/internal/module"
)

// xformMsg carries one transform generation's merged patches back to Update.
// gen lets a stale reply from a cancelled run be dropped wholesale.
type xformMsg struct {
	gen      int
	patches  map[string]module.Patch
	warnings []string
}

// modPreviewMsg carries the module preview sections fetched for one mission.
type modPreviewMsg struct {
	key      string
	sections []module.Section
	warnings []string
}

// previewTickMsg is the debounce timer for the preview fetch: it fires with
// the key that was selected when it was armed, and the handler execs only if
// that key is still the selection.
type previewTickMsg struct{ key string }

// previewDebounce delays the module preview fetch after a selection change:
// holding j/k must not spawn a handler per row — only where the cursor rests.
const previewDebounce = 150 * time.Millisecond

// maxXformFails is how many consecutive failed generations keep a module's
// previous patches alive: one blip must not flash every badge off, but a
// persistently broken module must eventually stop decorating the list.
const maxXformFails = 3

// hasTransforms reports whether any enabled module contributes a missions
// transform — without one there is nothing to dispatch and no message churn.
func hasTransforms(mods []module.Module) bool {
	for _, mod := range mods {
		if mod.Manifest.Transforms.Missions != nil {
			return true
		}
	}
	return false
}

// hasPreviews is hasTransforms for the preview surface.
func hasPreviews(mods []module.Module) bool {
	for _, mod := range mods {
		if mod.Manifest.Transforms.Preview != nil {
			return true
		}
	}
	return false
}

// transformCmd runs every missions-transform handler off the event loop —
// sequentially in lexicographic module order, each against the original
// projection — and reports the merged patches tagged with their generation.
// Like every module-spawned goroutine it recovers: a bug reachable through a
// module path must never crash the TUI. A nil patches map in the recovery
// message means "no data" — the handler keeps the current patches.
func transformCmd(ctx context.Context, mods []module.Module, gen int, rows []module.MissionRow) tea.Cmd {
	return func() (msg tea.Msg) {
		defer func() {
			if r := recover(); r != nil {
				msg = xformMsg{gen: gen, warnings: []string{fmt.Sprintf("module transform panic: %v", r)}}
			}
		}()
		patches, warns := module.RunTransforms(ctx, mods, gen, rows)
		return xformMsg{gen: gen, patches: patches, warnings: warns}
	}
}

// previewCmd fetches the module preview sections for one mission off the
// event loop. No generation here: stale replies are dropped by comparing the
// key against the live selection. ctx is the model's root module context, so
// the quit-time cancel reaches an in-flight handler.
func previewCmd(ctx context.Context, mods []module.Module, key string, row module.MissionRow) tea.Cmd {
	return func() (msg tea.Msg) {
		defer func() {
			if r := recover(); r != nil {
				msg = modPreviewMsg{key: key, warnings: []string{fmt.Sprintf("module preview panic: %v", r)}}
			}
		}()
		secs, warns := module.RunPreviews(ctx, mods, row)
		return modPreviewMsg{key: key, sections: secs, warnings: warns}
	}
}

// dispatchTransforms cancels the in-flight transform run and starts one for
// the current scan set under a fresh generation. Only scan-set changes come
// through here (r rescan, refresh-after-action) — meta edits like pin/tag
// don't invalidate key-addressed patches. Update-only: receiver mutations
// survive via the returned model.
func (m *Model) dispatchTransforms() tea.Cmd {
	if !hasTransforms(m.mods) {
		return nil
	}
	m.xformGen++
	m.xformStop() // never nil: set in New
	ctx, cancel := context.WithCancel(m.modCtx)
	m.xformStop = cancel
	return transformCmd(ctx, m.mods, m.xformGen, module.ProjectRows(m.missions, m.st))
}

// transformFailed matches the warning RunTransforms emits for a failed
// module, distinguishing failures from notices (which share the "[name] "
// shape).
func transformFailed(name string, warns []string) bool {
	pfx := "[" + name + "] transform: "
	for _, w := range warns {
		if strings.HasPrefix(w, pfx) {
			return true
		}
	}
	return false
}

// acceptPatches installs a generation's merged patches. The merged map has
// no per-module attribution, so failure retention is per key: keys the new
// merge no longer covers are carried over while any transform module is
// inside its failure window (≤ maxXformFails consecutive failures), then
// dropped. A clean generation replaces the map outright, so a module that
// legitimately stops patching a key takes effect immediately.
func (m *Model) acceptPatches(patches map[string]module.Patch, warns []string) {
	retain := false
	for _, mod := range m.mods {
		if mod.Manifest.Transforms.Missions == nil {
			continue
		}
		if !transformFailed(mod.Name, warns) {
			delete(m.xformFails, mod.Name)
			continue
		}
		m.xformFails[mod.Name]++
		if m.xformFails[mod.Name] <= maxXformFails {
			retain = true
		}
	}
	if retain {
		for k, old := range m.modPatches {
			if _, ok := patches[k]; !ok {
				patches[k] = old
			}
		}
	}
	m.modPatches = patches
}

// noteModWarnings surfaces passive-surface warnings (transform/preview
// failures and notices) once per module per session — explicit actions
// always report, but a broken transform must not spam the footer on every
// rescan. The module name is the "[name] " prefix RunTransforms/RunPreviews
// put on every warning.
func (m *Model) noteModWarnings(warns []string) {
	for _, w := range warns {
		key := w
		if i := strings.IndexByte(w, ']'); strings.HasPrefix(w, "[") && i > 1 {
			key = w[1:i]
		}
		if m.warned[key] {
			continue
		}
		m.warned[key] = true
		m.note(w)
	}
}

// armPreview is the single selection-identity choke point, run at the end of
// every Update: selection changes come from many paths (cursor keys, rescans
// replacing the mission set, live search keystrokes, hide patches removing
// rows above the cursor), so hooking individual key handlers would miss most
// of them. When the selected key differs from the last armed one, a
// debounced tick is scheduled; rescans reset lastPreviewKey so this re-fires
// even when the selected key is unchanged (the cache died with the scan).
func (m *Model) armPreview() tea.Cmd {
	if !hasPreviews(m.mods) {
		return nil
	}
	key := ""
	if ms, ok := m.selected(); ok {
		key = ms.Key()
	}
	if key == m.lastPreviewKey {
		return nil
	}
	m.lastPreviewKey = key
	if key == "" {
		return nil
	}
	if _, ok := m.prevCache[key]; ok {
		return nil // already fetched; updatePreview rendered it from the cache
	}
	return tea.Tick(previewDebounce, func(time.Time) tea.Msg { return previewTickMsg{key: key} })
}
