package tui

// The command registry: every key-driven capability of the TUI described as
// data instead of scattered switch cases and footer literals. The builtin
// key tables (module conflict enforcement), the which-key help overlay
// (help.go) and the footer hints all derive from this single source, so a
// binding added here is advertised everywhere at once — and the drift test
// that compares the key switches against the derived tables catches a switch
// case added without its registry entry, or the reverse.

import "strings"

// Registry screen ids. scrMissions and scrAccounts match the manifest's
// Action.Screen values so module entries map one-to-one; scrModView is the
// full-screen module-view page, which only core bindings live on. scrGlobal
// commands (tab switching) dispatch before any screen handler and join every
// screen's conflict table and overlay.
const (
	scrMissions = "missions"
	scrAccounts = "accounts"
	scrModView  = "modview"
	scrGlobal   = "global"
)

type cmdOrigin int

const (
	originCore cmdOrigin = iota
	originModule
)

// command describes one binding as data.
type command struct {
	keys     []string  // every msg.String() form that triggers it: {"up", "k"}
	label    string    // display form for help: "↑/k"
	title    string    // what it does: "move up"
	screen   string    // scrMissions | scrAccounts | scrModView
	category string    // overlay section; module commands use the module name
	origin   cmdOrigin // core binding or module contribution
	module   string    // owning module when origin == originModule
	hint     bool      // include in the one-line footer hint for its screen
}

// coreCommands returns every built-in binding in display order: the overlay
// keeps this order inside each category and orders core categories by first
// appearance. The hint flags curate the footer one-liners — the full list
// lives in the overlay, which is the fix for help outgrowing one line.
func coreCommands() []command {
	return []command{
		// global: tab switching and the palette, dispatched before any screen
		// handler. First in the slice so the overlay's Tabs section leads on
		// every screen; the palette entry merges into each screen's System
		// section.
		{keys: []string{"1", "2", "3", "4", "5", "6", "7", "8", "9"}, label: "1-9", title: "switch tab", screen: scrGlobal, category: "Tabs"},
		{keys: []string{"[", "]"}, label: "[/]", title: "previous / next tab", screen: scrGlobal, category: "Tabs"},
		{keys: []string{":", "ctrl+p"}, label: ":", title: "command palette", screen: scrGlobal, category: "System", hint: true},

		// missions screen
		{keys: []string{"up", "k"}, label: "↑/k", title: "move up", screen: scrMissions, category: "Navigate"},
		{keys: []string{"down", "j"}, label: "↓/j", title: "move down", screen: scrMissions, category: "Navigate"},
		{keys: []string{"tab"}, label: "tab", title: "switch pane", screen: scrMissions, category: "Navigate"},
		{keys: []string{"left", "h"}, label: "←/h", title: "focus programs", screen: scrMissions, category: "Navigate"},
		{keys: []string{"right", "l"}, label: "→/l", title: "focus missions", screen: scrMissions, category: "Navigate"},
		{keys: []string{"/"}, label: "/", title: "search", screen: scrMissions, category: "Navigate", hint: true},
		{keys: []string{"esc"}, label: "esc", title: "clear filter", screen: scrMissions, category: "Navigate"},
		{keys: []string{"pgup", "b"}, label: "pgup/b", title: "preview up", screen: scrMissions, category: "Navigate"},
		{keys: []string{"pgdown", "f"}, label: "pgdn/f", title: "preview down", screen: scrMissions, category: "Navigate"},
		{keys: []string{"enter"}, label: "enter", title: "resume", screen: scrMissions, category: "Mission", hint: true},
		{keys: []string{"e"}, label: "e", title: "export transcript", screen: scrMissions, category: "Mission"},
		{keys: []string{"m"}, label: "m", title: "remap cwd", screen: scrMissions, category: "Mission"},
		{keys: []string{"*"}, label: "*", title: "pin / unpin", screen: scrMissions, category: "Organize"},
		{keys: []string{"a"}, label: "a", title: "archive / unarchive", screen: scrMissions, category: "Organize"},
		{keys: []string{"t"}, label: "t", title: "tag", screen: scrMissions, category: "Organize"},
		{keys: []string{"n"}, label: "n", title: "note", screen: scrMissions, category: "Organize"},
		{keys: []string{"p"}, label: "p", title: "add to program", screen: scrMissions, category: "Organize"},
		{keys: []string{"P"}, label: "P", title: "new program", screen: scrMissions, category: "Organize"},
		{keys: []string{"x"}, label: "x", title: "remove from program", screen: scrMissions, category: "Organize"},
		{keys: []string{"A"}, label: "A", title: "accounts", screen: scrMissions, category: "System"},
		{keys: []string{"r"}, label: "r", title: "reindex", screen: scrMissions, category: "System"},
		{keys: []string{"?"}, label: "?", title: "help", screen: scrMissions, category: "System", hint: true},
		{keys: []string{"q", "ctrl+c"}, label: "q", title: "quit", screen: scrMissions, category: "System"},

		// accounts screen
		{keys: []string{"up", "k"}, label: "↑/k", title: "move up", screen: scrAccounts, category: "Navigate"},
		{keys: []string{"down", "j"}, label: "↓/j", title: "move down", screen: scrAccounts, category: "Navigate"},
		{keys: []string{"enter"}, label: "enter", title: "launch session", screen: scrAccounts, category: "Account", hint: true},
		{keys: []string{"d", "x"}, label: "d/x", title: "remove account", screen: scrAccounts, category: "Account", hint: true},
		{keys: []string{"r"}, label: "r", title: "probe usage", screen: scrAccounts, category: "System"},
		{keys: []string{"esc", "A", "tab"}, label: "esc", title: "back to missions", screen: scrAccounts, category: "System"},
		{keys: []string{"?"}, label: "?", title: "help", screen: scrAccounts, category: "System", hint: true},
		{keys: []string{"q", "ctrl+c"}, label: "q", title: "quit", screen: scrAccounts, category: "System"},

		// full-screen module view
		{keys: []string{"up", "k"}, label: "↑/k", title: "scroll up", screen: scrModView, category: "Navigate"},
		{keys: []string{"down", "j"}, label: "↓/j", title: "scroll down", screen: scrModView, category: "Navigate"},
		{keys: []string{"pgup", "b"}, label: "pgup/b", title: "half page up", screen: scrModView, category: "Navigate"},
		{keys: []string{"pgdown", "f", " "}, label: "pgdn/f", title: "half page down", screen: scrModView, category: "Navigate"},
		{keys: []string{"g", "G"}, label: "g/G", title: "top / bottom", screen: scrModView, category: "Navigate"},
		{keys: []string{"/"}, label: "/", title: "filter rows", screen: scrModView, category: "Navigate"},
		{keys: []string{"r"}, label: "r", title: "refresh", screen: scrModView, category: "System", hint: true},
		{keys: []string{"esc", "backspace"}, label: "esc", title: "back", screen: scrModView, category: "System", hint: true},
		{keys: []string{"?"}, label: "?", title: "help", screen: scrModView, category: "System", hint: true},
		{keys: []string{"q", "ctrl+c"}, label: "q", title: "quit", screen: scrModView, category: "System"},
	}
}

// builtinKeyTable collects the dispatch keys of every core command on ONE
// screen — the drift test compares each key switch against exactly this.
func builtinKeyTable(screen string) map[string]bool {
	out := map[string]bool{}
	for _, c := range coreCommands() {
		if c.screen != screen {
			continue
		}
		for _, k := range c.keys {
			out[k] = true
		}
	}
	return out
}

// unionKeys merges key tables — the exported conflict tables are a screen's
// own keys plus the global ones, so a module can never claim a tab key.
func unionKeys(tables ...map[string]bool) map[string]bool {
	out := map[string]bool{}
	for _, t := range tables {
		for k := range t {
			out[k] = true
		}
	}
	return out
}

// moduleCommands projects the surviving module actions and views into
// registry entries, in the deterministic accepted order. Each module becomes
// its own overlay section (category = module name).
func moduleCommands(acts []moduleActionRef, views []moduleViewRef) []command {
	var out []command
	for _, r := range acts {
		scr := scrMissions
		if r.act.Screen == "accounts" {
			scr = scrAccounts
		}
		out = append(out, command{
			keys: []string{r.act.Key}, label: keyLabel(r.act.Key), title: r.act.Title,
			screen: scr, category: r.mod.Name, origin: originModule, module: r.mod.Name,
		})
	}
	for _, r := range views {
		out = append(out, command{
			keys: []string{r.view.Key}, label: keyLabel(r.view.Key), title: r.view.Title,
			screen: scrMissions, category: r.mod.Name, origin: originModule, module: r.mod.Name,
		})
	}
	return out
}

// hintFor joins a screen's hint-flagged commands into the one-line footer
// hint — the screen's own first, then flagged globals (the palette). Module
// commands never set the flag: the footer stays short by design and the
// overlay carries the full list.
func hintFor(reg []command, screen string) string {
	var parts []string
	for _, c := range reg {
		if c.screen == screen && c.hint {
			parts = append(parts, c.label+" "+c.title)
		}
	}
	for _, c := range reg {
		if c.screen == scrGlobal && c.hint {
			parts = append(parts, c.label+" "+c.title)
		}
	}
	return strings.Join(parts, " · ")
}
