// Houston — mission-control for Claude Code: balanced multi-account launching
// (one config dir per account, shared data) plus a TUI to browse, organize and
// resume conversations.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"houston/internal/accounts"
	"houston/internal/browse"
	"houston/internal/config"
	"houston/internal/fleet"
	"houston/internal/launch"
	"houston/internal/model"
	"houston/internal/module"
	"houston/internal/provision"
	"houston/internal/scan"
	"houston/internal/statusline"
	"houston/internal/store"
	"houston/internal/theme"
	"houston/internal/tui"
	"houston/internal/update"
	"houston/internal/usage"
)

// version is the build version, set via -ldflags "-X main.version=v0.5.0" in
// release builds. Local/source builds keep "dev" (which suppresses update nags).
var version = "dev"

// init stamps the module package's copy of the build version: envelopes and
// HOUSTON_VERSION must report the same value `houston version` prints, so
// handlers can gate on it.
func init() { module.HoustonVersion = version }

func main() {
	args := os.Args[1:]
	// Best-effort: clear a binary left aside by a previous Windows self-update
	// once it's no longer locked (see update.Swap / cmdUpdate).
	if exe, err := os.Executable(); err == nil {
		update.CleanupStale(exe)
	}
	switch {
	case len(args) == 1 && browse.IsHTTP(browse.CleanURL(args[0])):
		// $BROWSER mode: the claude child launched by Houston has BROWSER set
		// to this very binary (see launch.Cmd), so every link claude opens —
		// the OAuth login page included — arrives here as a single URL arg.
		cmdOpenURL(args[0])
	case len(args) == 2 && args[0] == "open-url":
		cmdOpenURL(args[1])
	case len(args) > 0 && args[0] == "account":
		cmdAccount(args[1:])
	case len(args) > 0 && args[0] == "mcp":
		cmdMCP(args[1:])
	case len(args) > 0 && (args[0] == "plugin" || args[0] == "plugins"):
		cmdPlugin(args[1:])
	case len(args) > 0 && (args[0] == "skill" || args[0] == "skills"):
		cmdSkill(args[1:])
	case len(args) > 0 && (args[0] == "module" || args[0] == "modules"):
		cmdModule(args[1:])
	case len(args) > 0 && args[0] == "run":
		cmdRun(args[1:])
	case len(args) > 0 && args[0] == "update":
		cmdUpdate(args[1:])
	case len(args) > 0 && (args[0] == "version" || args[0] == "--version" || args[0] == "-v"):
		fmt.Printf("houston %s\n", version)
		if n := update.Notice(version, 4*time.Second); n != "" {
			fmt.Println(n)
		}
	case len(args) > 0 && args[0] == "doctor":
		cmdDoctor(args[1:])
	case len(args) > 0 && args[0] == "statusline":
		// Reads the status-line JSON from stdin; prints the active account plus
		// every account's 5h/7d quota.
		fmt.Println(statusline.Line(os.Stdin, os.Getenv("CLAUDE_CONFIG_DIR")))
	default:
		cmdTUI(args)
	}
}

// cmdOpenURL implements $BROWSER mode / `houston open-url <url>`: Anthropic
// OAuth pages open in a private window of the default browser, anything else
// opens normally. HOUSTON_LOGIN_PRIVATE=off disables the private isolation.
func cmdOpenURL(raw string) {
	if err := browse.Open(raw); err != nil {
		fmt.Fprintln(os.Stderr, "houston:", err)
		os.Exit(1)
	}
}

// --- account management ---------------------------------------------------

func cmdAccount(args []string) {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "add":
		label := strings.Join(args[1:], " ")
		if label == "" {
			fmt.Fprintln(os.Stderr, "usage: houston account add <label>")
			os.Exit(1)
		}
		acc, err := accounts.Add(label, accounts.Now())
		if err != nil {
			fmt.Fprintln(os.Stderr, "houston:", err)
			os.Exit(1)
		}
		fmt.Printf("account added: %s (%s)\n", acc.ID, acc.Label)
		fmt.Println("next: 'houston run' — the first time it will open /login in the browser for this account.")
	case "ls", "list", "":
		accs, _ := accounts.Load()
		if len(accs) == 0 {
			fmt.Println("no accounts yet. Add one:  houston account add <label>")
			return
		}
		fmt.Fprintln(os.Stderr, "probing usage...")
		probes := usage.ProbeAll(accs, 8*time.Second)
		fmt.Printf("%-16s %-30s %-8s %s\n", "ID", "EMAIL / LABEL", "PRESSURE", "5h / 7d")
		for _, p := range probes {
			name := p.Account.Email()
			if name == "" {
				name = p.Account.Label
			}
			switch {
			case p.OK:
				fmt.Printf("%-16s %-30s %6.0f%%   %.0f%% / %.0f%%\n",
					p.Account.ID, trunc(name, 30), p.Pressure, p.U5, p.U7)
			case !p.Account.LoggedIn():
				fmt.Printf("%-16s %-30s   (not logged in yet)\n", p.Account.ID, trunc(name, 30))
			default:
				fmt.Printf("%-16s %-30s   (no usage: %s)\n", p.Account.ID, trunc(name, 30), p.Err)
			}
		}
	case "rm", "remove":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: houston account rm <id>")
			os.Exit(1)
		}
		if err := accounts.Remove(args[1]); err != nil {
			fmt.Fprintln(os.Stderr, "houston:", err)
			os.Exit(1)
		}
		fmt.Println("account removed:", args[1])
	default:
		fmt.Fprintln(os.Stderr, "usage: houston account [add <label> | ls | rm <id>]")
		os.Exit(1)
	}
}

// --- fleet: MCP / plugins / skills across every account --------------------

// loadFleet returns the registered accounts or exits with guidance.
func loadFleet() []accounts.Account {
	accs, err := accounts.Load()
	if err != nil || len(accs) == 0 {
		fmt.Fprintln(os.Stderr, "houston: no accounts; add one with 'houston account add'")
		os.Exit(1)
	}
	return accs
}

// cmdMCP manages user-scope MCP servers across every account. add/add-json
// pass through to the real `claude mcp` once (full flag parity, real
// validation) against the first account, then the resulting diff of its
// .claude.json mcpServers is copied into every other account.
//
//	houston mcp add <name> [claude mcp add flags/args...]
//	houston mcp add-json <name> '<json>'
//	houston mcp rm <name>
//	houston mcp ls
func cmdMCP(args []string) {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	accs := loadFleet()
	switch sub {
	case "add", "add-json":
		// The propagated scope is the USER scope (each account's .claude.json);
		// local/project scopes are per-directory and need no propagation.
		for i, a := range args {
			if (a == "-s" || a == "--scope") && i+1 < len(args) && args[i+1] != "user" {
				fmt.Fprintln(os.Stderr, "houston: only user-scope servers propagate across accounts; for local/project scope use claude directly")
				os.Exit(1)
			}
		}
		claudeArgs := append([]string{"mcp", sub, "--scope", "user"}, args[1:]...)
		src := accs[0]
		before, err := fleet.MCPServers(src)
		if err != nil {
			fmt.Fprintln(os.Stderr, "houston:", err)
			os.Exit(1)
		}
		if err := fleet.RunClaude(src, claudeArgs...); err != nil {
			os.Exit(1) // claude already printed why
		}
		after, _ := fleet.MCPServers(src)
		changed, removed := fleet.Diff(before, after)
		if len(changed) == 0 && len(removed) == 0 {
			fmt.Println("houston: claude made no user-scope change; nothing to propagate")
			return
		}
		for _, a := range accs[1:] {
			if err := fleet.PatchMCP(a, changed, removed); err != nil {
				fmt.Fprintf(os.Stderr, "houston: %s: %v\n", a.ID, err)
				os.Exit(1)
			}
		}
		for _, k := range fleet.Keys(changed) {
			fmt.Printf("✓ mcp server %q propagated to all %d accounts\n", k, len(accs))
		}
	case "rm", "remove":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: houston mcp rm <name>")
			os.Exit(1)
		}
		name := args[1]
		for _, a := range accs {
			if err := fleet.PatchMCP(a, nil, []string{name}); err != nil {
				fmt.Fprintf(os.Stderr, "houston: %s: %v\n", a.ID, err)
				os.Exit(1)
			}
		}
		fmt.Printf("✓ mcp server %q removed from all %d accounts\n", name, len(accs))
	case "ls", "list", "":
		perAcc := make([]map[string]json.RawMessage, len(accs))
		for i, a := range accs {
			perAcc[i], _ = fleet.MCPServers(a)
		}
		names := fleet.Keys(perAcc...)
		if len(names) == 0 {
			fmt.Println("no user-scope MCP servers. Add one:  houston mcp add <name> -- <command>")
			return
		}
		printFleetTable("MCP SERVER", names, accs, func(name string, i int) string {
			if _, ok := perAcc[i][name]; ok {
				return "✓"
			}
			return "—"
		})
	default:
		fmt.Fprintln(os.Stderr, "usage: houston mcp [add <name> ... | add-json <name> <json> | rm <name> | ls]")
		os.Exit(1)
	}
}

// cmdPlugin manages plugins across every account. Plugin FILES already land in
// the shared store (the plugins dir is junctioned), so install/uninstall runs
// once via the real `claude plugin`; what propagates is the ENABLEMENT
// (settings.json → enabledPlugins) per account.
//
//	houston plugin add <plugin>[@<marketplace>]
//	houston plugin rm <plugin>[@<marketplace>]
//	houston plugin ls
//	houston plugin marketplace <args...>   (runs once; marketplaces are shared)
func cmdPlugin(args []string) {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	accs := loadFleet()
	switch sub {
	case "add", "install", "i":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: houston plugin add <plugin>[@<marketplace>]")
			os.Exit(1)
		}
		spec := args[1]
		src := accs[0]
		before, _ := fleet.EnabledPlugins(src)
		if err := fleet.RunClaude(src, "plugin", "install", spec); err != nil {
			os.Exit(1)
		}
		after, _ := fleet.EnabledPlugins(src)
		changed, removed := fleet.Diff(before, after)
		if len(changed) == 0 && len(removed) == 0 {
			// already installed+enabled in the source account: propagate its
			// current entries for this spec so the others still catch up.
			for _, k := range fleet.MatchPluginKeys(spec, fleet.Keys(after)) {
				changed[k] = after[k]
			}
		}
		if len(changed) == 0 && len(removed) == 0 {
			fmt.Println("houston: nothing to propagate (claude did not enable the plugin?)")
			return
		}
		for _, a := range accs[1:] {
			if err := fleet.PatchPlugins(a, changed, removed); err != nil {
				fmt.Fprintf(os.Stderr, "houston: %s: %v\n", a.ID, err)
				os.Exit(1)
			}
		}
		for _, k := range fleet.Keys(changed) {
			fmt.Printf("✓ plugin %q enabled in all %d accounts (files live in the shared store)\n", k, len(accs))
		}
	case "rm", "uninstall", "remove":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: houston plugin rm <plugin>[@<marketplace>]")
			os.Exit(1)
		}
		spec := args[1]
		perAcc := make([]map[string]json.RawMessage, len(accs))
		for i, a := range accs {
			perAcc[i], _ = fleet.EnabledPlugins(a)
		}
		keys := fleet.MatchPluginKeys(spec, fleet.Keys(perAcc...))
		if len(keys) == 0 {
			fmt.Fprintf(os.Stderr, "houston: no account has a plugin matching %q\n", spec)
			os.Exit(1)
		}
		// Uninstall the files once (best effort — the entry may exist without
		// files), then drop the enablement everywhere.
		_ = fleet.RunClaude(accs[0], "plugin", "uninstall", spec)
		for _, a := range accs {
			if err := fleet.PatchPlugins(a, nil, keys); err != nil {
				fmt.Fprintf(os.Stderr, "houston: %s: %v\n", a.ID, err)
				os.Exit(1)
			}
		}
		for _, k := range keys {
			fmt.Printf("✓ plugin %q removed from all %d accounts\n", k, len(accs))
		}
	case "ls", "list", "":
		perAcc := make([]map[string]json.RawMessage, len(accs))
		for i, a := range accs {
			perAcc[i], _ = fleet.EnabledPlugins(a)
		}
		names := fleet.Keys(perAcc...)
		if len(names) == 0 {
			fmt.Println("no plugins enabled. Add one:  houston plugin add <plugin>@<marketplace>")
			return
		}
		printFleetTable("PLUGIN", names, accs, func(name string, i int) string {
			raw, ok := perAcc[i][name]
			switch {
			case !ok:
				return "—"
			case string(raw) == "true":
				return "✓"
			default:
				return "✗" // present but disabled
			}
		})
	case "marketplace":
		// Marketplaces live inside the shared plugins dir: adding one in any
		// account makes it visible everywhere, so a single passthrough is enough.
		if err := fleet.RunClaude(accs[0], append([]string{"plugin", "marketplace"}, args[1:]...)...); err != nil {
			os.Exit(1)
		}
	default:
		fmt.Fprintln(os.Stderr, "usage: houston plugin [add <plugin> | rm <plugin> | ls | marketplace <args...>]")
		os.Exit(1)
	}
}

// cmdSkill manages skills in the SHARED store — every account sees them
// through its junction, so installing once is already fleet-wide.
//
//	houston skill add <git-url|local-dir> [--path <subdir>] [--ref <ref>] [--name <n>] [--force]
//	houston skill rm <name>
//	houston skill ls
func cmdSkill(args []string) {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "add", "install":
		var s fleet.SkillSource
		force := false
		rest := args[1:]
		for i := 0; i < len(rest); i++ {
			switch rest[i] {
			case "--path":
				i++
				if i < len(rest) {
					s.Path = rest[i]
				}
			case "--ref":
				i++
				if i < len(rest) {
					s.Ref = rest[i]
				}
			case "--name":
				i++
				if i < len(rest) {
					s.Name = rest[i]
				}
			case "--force", "-f":
				force = true
			default:
				if s.URL == "" && s.Local == "" {
					if fi, err := os.Stat(rest[i]); err == nil && fi.IsDir() {
						s.Local = rest[i]
					} else {
						s.URL = rest[i]
					}
				}
			}
		}
		if s.URL == "" && s.Local == "" {
			fmt.Fprintln(os.Stderr, "usage: houston skill add <git-url|local-dir> [--path <subdir>] [--ref <ref>] [--name <n>] [--force]")
			os.Exit(1)
		}
		dst, err := fleet.InstallSkill(s, force)
		if err != nil {
			fmt.Fprintln(os.Stderr, "houston:", err)
			os.Exit(1)
		}
		fmt.Printf("✓ skill installed → %s (shared: every account sees it)\n", dst)
	case "rm", "remove":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: houston skill rm <name>")
			os.Exit(1)
		}
		if err := fleet.RemoveSkill(args[1]); err != nil {
			fmt.Fprintln(os.Stderr, "houston:", err)
			os.Exit(1)
		}
		fmt.Printf("✓ skill %q removed (fleet-wide: the dir is shared)\n", args[1])
	case "ls", "list", "":
		names, err := fleet.ListSkills()
		if err != nil {
			fmt.Fprintln(os.Stderr, "houston:", err)
			os.Exit(1)
		}
		if len(names) == 0 {
			fmt.Println("no skills in the shared store. Add one:  houston skill add <git-url|dir>")
			return
		}
		fmt.Printf("shared skills (%s):\n", fleet.SkillsDir())
		for _, n := range names {
			fmt.Println("  " + n)
		}
	default:
		fmt.Fprintln(os.Stderr, "usage: houston skill [add <source> | rm <name> | ls]")
		os.Exit(1)
	}
}

// printFleetTable renders one row per item with a ✓/—/✗ cell per account.
func printFleetTable(header string, names []string, accs []accounts.Account, cell func(name string, acc int) string) {
	fmt.Printf("%-28s", header)
	for _, a := range accs {
		fmt.Printf(" %-12s", trunc(a.ID, 12))
	}
	fmt.Println()
	for _, n := range names {
		fmt.Printf("%-28s", trunc(n, 28))
		for i := range accs {
			fmt.Printf(" %-12s", cell(n, i))
		}
		fmt.Println()
	}
}

// --- modules: exec'd extensions under <StoreDir>/modules -------------------

// cmdModule manages Houston modules — reviewable directories of exec'd
// handlers (internal/module). Installs are snapshots and always land
// DISABLED: `enable` is the consent point where module code starts running.
//
//	houston module ls
//	houston module add <path|git-url> [--name <n>] [--enable]
//	houston module rm <name> [--yes]
//	houston module enable <name>
//	houston module disable <name>
//	houston module test <name> [--event <e>] [--live] [--mission <key>]
//	houston module log [-f] [<name>]
func cmdModule(args []string) {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "ls", "list", "":
		cmdModuleLs()
	case "add", "install":
		cmdModuleAdd(args[1:])
	case "rm", "remove":
		cmdModuleRm(args[1:])
	case "enable", "disable":
		if len(args) < 2 {
			fmt.Fprintf(os.Stderr, "usage: houston module %s <name>\n", sub)
			os.Exit(1)
		}
		name := args[1]
		if err := module.SetEnabled(name, sub == "enable"); err != nil {
			fmt.Fprintln(os.Stderr, "houston:", err)
			os.Exit(1)
		}
		if sub == "enable" {
			printEnableNotice(name)
		} else {
			fmt.Printf("module %q disabled\n", name)
		}
	case "test":
		cmdModuleTest(args[1:])
	case "log":
		cmdModuleLog(args[1:])
	default:
		fmt.Fprintln(os.Stderr, "usage: houston module [ls | add <path|git-url> [--name <n>] [--enable] | rm <name> [--yes] | enable <name> | disable <name> | test <name> [--event <e>] [--live] [--mission <key>] | log [-f] [<name>]]")
		os.Exit(1)
	}
}

func cmdModuleLs() {
	mods, errs := module.LoadAll(config.Load())
	if len(mods) == 0 && len(errs) == 0 {
		fmt.Println("no modules installed. Add one:  houston module add <path|git-url>")
		return
	}
	if len(mods) > 0 {
		fmt.Printf("%-24s %-10s %-8s %s\n", "NAME", "VERSION", "ENABLED", "SURFACES")
		for _, m := range mods {
			version := m.Manifest.Version
			if version == "" {
				version = "—"
			}
			enabled := "no"
			if m.Enabled {
				enabled = "yes"
			}
			fmt.Printf("%-24s %-10s %-8s %s\n", trunc(m.Name, 24), trunc(version, 10), enabled, surfacesSummary(m.Manifest))
		}
	}
	for _, w := range shadowedKeys(mods) {
		fmt.Println("  ! " + w)
	}
	for _, e := range errs {
		fmt.Println("  ✗ " + e.Error())
	}
}

func cmdModuleAdd(args []string) {
	src, name := "", ""
	enable := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--name":
			i++
			if i < len(args) {
				name = args[i]
			}
		case "--enable":
			enable = true
		default:
			if src == "" {
				src = args[i]
			}
		}
	}
	if src == "" {
		fmt.Fprintln(os.Stderr, "usage: houston module add <path|git-url> [--name <n>] [--enable]")
		os.Exit(1)
	}
	fi, err := os.Stat(src)
	localDir := err == nil && fi.IsDir()
	if enable && !localDir {
		// Installing code from a URL must not arm it in the same breath;
		// --enable is for directories you wrote yourself.
		fmt.Fprintln(os.Stderr, "houston: --enable is refused for git sources; review the installed files first, then run 'houston module enable <name>'")
		os.Exit(1)
	}
	e, err := module.Add(src, name)
	if err != nil {
		fmt.Fprintln(os.Stderr, "houston:", err)
		os.Exit(1)
	}
	dir := filepath.Join(module.Dir(), e.Name)
	fmt.Printf("✓ module %q installed (disabled) → %s\n", e.Name, dir)
	// Re-read the landed manifest: what gets printed is what got installed.
	var man module.Manifest
	if b, err := os.ReadFile(filepath.Join(dir, "module.json")); err == nil {
		man, _ = module.ParseManifest(b)
	}
	if man.Version != "" {
		fmt.Println("  version:     " + man.Version)
	}
	if man.Description != "" {
		fmt.Println("  description: " + man.Description)
	}
	if cmds := manifestCommands(man); len(cmds) > 0 {
		fmt.Println("  commands it will run (review before enabling):")
		for _, c := range cmds {
			fmt.Printf("    %-32s %q\n", c.label, c.argv)
		}
	}
	for _, w := range commandWarnings(man, dir) {
		fmt.Println("  warning: " + w)
	}
	if enable {
		if err := module.SetEnabled(e.Name, true); err != nil {
			fmt.Fprintln(os.Stderr, "houston:", err)
			os.Exit(1)
		}
		printEnableNotice(e.Name)
		return
	}
	fmt.Printf("  enable with:  houston module enable %s\n", e.Name)
}

func cmdModuleRm(args []string) {
	name, yes := "", false
	for _, a := range args {
		switch a {
		case "--yes", "-y":
			yes = true
		default:
			if name == "" {
				name = a
			}
		}
	}
	if name == "" {
		fmt.Fprintln(os.Stderr, "usage: houston module rm <name> [--yes]")
		os.Exit(1)
	}
	// Validate before prompting; Remove re-validates before any filesystem op.
	if err := module.SafeName(name); err != nil {
		fmt.Fprintln(os.Stderr, "houston:", err)
		os.Exit(1)
	}
	if !yes {
		fmt.Printf("Remove module %q and its files? [y/N]: ", name)
		resp, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		switch strings.ToLower(strings.TrimSpace(resp)) {
		case "y", "yes":
		default:
			fmt.Println("Cancelled. Nothing was changed.")
			return
		}
	}
	if err := module.Remove(name); err != nil {
		fmt.Fprintln(os.Stderr, "houston:", err)
		os.Exit(1)
	}
	fmt.Printf("module %q removed\n", name)
}

// cmdModuleTest runs every contribution a module declares against synthetic
// (or --live) data through the real runner policy; the exit code is the
// verdict, so module repos can run it in CI.
func cmdModuleTest(args []string) {
	name := ""
	opts := module.TestOpts{}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--event":
			i++
			if i < len(args) {
				opts.Event = args[i]
			}
		case "--live":
			opts.Live = true
		case "--mission":
			i++
			if i < len(args) {
				opts.Mission = args[i]
				opts.Live = true // a real mission key only exists in live data
			}
		default:
			if name == "" {
				name = args[i]
			}
		}
	}
	if name == "" {
		fmt.Fprintln(os.Stderr, "usage: houston module test <name> [--event <e>] [--live] [--mission <key>]")
		os.Exit(1)
	}
	if err := module.SafeName(name); err != nil {
		fmt.Fprintln(os.Stderr, "houston:", err)
		os.Exit(1)
	}
	os.Exit(module.RunTest(name, opts))
}

// cmdModuleLog prints — or with -f follows — modules.log, optionally
// filtered to one module's stanzas.
func cmdModuleLog(args []string) {
	name, follow := "", false
	for _, a := range args {
		switch a {
		case "-f", "--follow":
			follow = true
		default:
			if name == "" {
				name = a
			}
		}
	}
	if name != "" {
		if err := module.SafeName(name); err != nil {
			fmt.Fprintln(os.Stderr, "houston:", err)
			os.Exit(1)
		}
	}
	filt := module.NewLogFilter(name)
	path := module.LogPath()
	off, _ := printLogChunk(path, 0, filt)
	if !follow {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			fmt.Println("no module log yet (" + path + ")")
		}
		return
	}
	for {
		time.Sleep(500 * time.Millisecond)
		fi, err := os.Stat(path)
		if err != nil {
			continue
		}
		if fi.Size() < off {
			// Trimmed or replaced under us: start over from the new file's top.
			off = 0
			filt = module.NewLogFilter(name)
		}
		if fi.Size() > off {
			off, _ = printLogChunk(path, off, filt)
		}
	}
}

// printLogChunk streams complete lines from off to EOF through the filter and
// returns the offset just past the last complete line — a partially written
// trailing line is left for the next poll.
func printLogChunk(path string, off int64, filt *module.LogFilter) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return off, err
	}
	defer f.Close()
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return off, err
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return off, err
	}
	end := bytes.LastIndexByte(b, '\n')
	if end < 0 {
		return off, nil
	}
	for _, line := range strings.Split(string(b[:end]), "\n") {
		if filt.Keep(strings.TrimSuffix(line, "\r")) {
			fmt.Println(line)
		}
	}
	return off + int64(end) + 1, nil
}

// printEnableNotice is the security-model notice shown at the consent point:
// everything after `enable` is advisory.
func printEnableNotice(name string) {
	fmt.Printf("module %q enabled\n\n", name)
	fmt.Println("Security notice — what enabling means:")
	fmt.Println("  • This module now runs arbitrary code as your user: at TUI start, on")
	fmt.Println("    every mission rescan, and on every statusline render.")
	fmt.Println("  • From here on enablement is advisory: an enabled module can rewrite")
	fmt.Println("    modules.json, other modules, config.json, caches, or the Houston")
	fmt.Println("    binary itself. Timeouts, output caps, ANSI stripping and symlink")
	fmt.Println("    rejection are robustness features, not a sandbox.")
	fmt.Println("  • Payloads sent to its handlers include mission titles (for unnamed")
	fmt.Println("    sessions that is your first prompt), cwd paths, and this module's")
	fmt.Println("    settings from config.json (which may hold tokens).")
	fmt.Printf("Review the code in %s\n", filepath.Join(module.Dir(), name))
	fmt.Printf("Disable with:  houston module disable %s\n", name)
}

// surfacesSummary compresses what a manifest contributes into one cell.
func surfacesSummary(man module.Manifest) string {
	var parts []string
	if n := len(man.Actions); n > 0 {
		parts = append(parts, fmt.Sprintf("actions:%d", n))
	}
	if man.Transforms.Missions != nil {
		parts = append(parts, "transform")
	}
	if man.Transforms.Preview != nil {
		parts = append(parts, "preview")
	}
	if man.Statusline != nil {
		parts = append(parts, "statusline")
	}
	if man.PreLaunch != nil {
		parts = append(parts, "preLaunch")
	}
	if n := len(man.Views); n > 0 {
		parts = append(parts, fmt.Sprintf("views:%d", n))
	}
	if man.Theme != nil {
		parts = append(parts, "theme")
	}
	if len(parts) == 0 {
		return "—"
	}
	return strings.Join(parts, " ")
}

// shadowedKeys reports enabled-module action AND view keys that can never
// fire, under exactly the rules the TUI enforces (buildModContribs): one
// claim pass per module in lexicographic name order — a module's actions
// claim before its views, and across modules the earlier module keeps a key
// whatever the class. Built-ins (tab keys included) always win. ls/doctor
// and the TUI can never disagree about what is live.
func shadowedKeys(mods []module.Module) []string {
	claimed := map[string]string{}
	var out []string
	for _, m := range mods {
		if !m.Enabled {
			continue
		}
		for _, a := range m.Manifest.Actions {
			builtin := tui.BuiltinMissionsKeys
			if a.Screen == "accounts" {
				builtin = tui.BuiltinAccountsKeys
			}
			if builtin[a.Key] {
				out = append(out, fmt.Sprintf("%s: action %q key %q (%s) is shadowed by a built-in key", m.Name, a.ID, a.Key, a.Screen))
				continue
			}
			k := a.Screen + ":" + a.Key
			if by, taken := claimed[k]; taken {
				out = append(out, fmt.Sprintf("%s: action %q key %q (%s) is shadowed by module %s", m.Name, a.ID, a.Key, a.Screen, by))
				continue
			}
			claimed[k] = m.Name
		}
		for _, v := range m.Manifest.Views {
			what := "view"
			if v.Tab {
				what = "tab view"
			}
			if tui.BuiltinMissionsKeys[v.Key] {
				out = append(out, fmt.Sprintf("%s: %s %q key %q is shadowed by a built-in key", m.Name, what, v.ID, v.Key))
				continue
			}
			k := "missions:" + v.Key
			if by, taken := claimed[k]; taken {
				out = append(out, fmt.Sprintf("%s: %s %q key %q is shadowed by module %s", m.Name, what, v.ID, v.Key, by))
				continue
			}
			claimed[k] = m.Name
			// Page actions are pruned by the TUI against the view page's own
			// keys — mirror that here or doctor gives a false all-clear on a
			// dead binding.
			for _, va := range v.Actions {
				if tui.BuiltinViewPageKeys[va.Key] {
					out = append(out, fmt.Sprintf("%s: view %q action %q key %q is shadowed by a built-in view-page key", m.Name, v.ID, va.ID, va.Key))
				}
			}
		}
	}
	return out
}

// modCommand is one declared command array with a human label, for the add
// transparency printout and doctor's static checks.
type modCommand struct {
	label string
	argv  []string
}

func manifestCommands(man module.Manifest) []modCommand {
	var out []modCommand
	for _, a := range man.Actions {
		label := fmt.Sprintf("action %s (%s, %s)", a.ID, a.Key, a.Screen)
		if a.Interactive {
			label += " interactive"
		}
		out = append(out, modCommand{label, a.Command})
	}
	if h := man.Transforms.Missions; h != nil {
		out = append(out, modCommand{"transforms.missions", h.Command})
	}
	if h := man.Transforms.Preview; h != nil {
		out = append(out, modCommand{"transforms.preview", h.Command})
	}
	if s := man.Statusline; s != nil {
		out = append(out, modCommand{"statusline", s.Command})
	}
	if h := man.PreLaunch; h != nil {
		out = append(out, modCommand{"preLaunch", h.Command})
	}
	for _, v := range man.Views {
		out = append(out, modCommand{"view " + v.ID, v.Command})
	}
	return out
}

// commandWarnings runs the static, zero-exec checks on a module's declared
// commands: a bare command[0] should resolve on PATH (warning only — PATH may
// differ at runtime) and a relative command[0] must exist inside the module
// dir. Absolute and escaping paths never get this far — manifest validation
// rejects them.
func commandWarnings(man module.Manifest, dir string) []string {
	var out []string
	seen := map[string]bool{}
	for _, c := range manifestCommands(man) {
		if len(c.argv) == 0 {
			continue
		}
		head := c.argv[0]
		if !strings.ContainsAny(head, `/\`) {
			if seen[head] {
				continue
			}
			seen[head] = true
			if _, err := exec.LookPath(head); err != nil {
				out = append(out, fmt.Sprintf("command %q not found on PATH", head))
			}
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(strings.ReplaceAll(head, `\`, "/")))); err != nil {
			out = append(out, fmt.Sprintf("%s: %q not found in the module directory", c.label, head))
		}
	}
	return out
}

// runPreLaunchHooks runs every enabled module's pre-launch hook attached to
// this terminal, in lexicographic order, before claude is launched. A hook's
// exit code is its verdict: nonzero cancels the launch (returns false).
// Fail-open everywhere else: hooks that cannot be built or started are
// skipped with a warning — a broken module must never brick `houston run`.
func runPreLaunchHooks(source string, acc accounts.Account) bool {
	mods, _ := module.LoadEnabled(config.Load())
	hooks := module.PreLaunchMods(mods)
	if len(hooks) == 0 {
		return true
	}
	cwd, _ := os.Getwd()
	row := module.AccountRowOf(acc)
	payload := module.PreLaunchPayload{Source: source, Cwd: cwd, Account: &row}
	for _, mod := range hooks {
		env := module.NewEnvelope(module.EventPreLaunch, mod, payload)
		cmd, cleanup, err := module.ExecPreLaunch(mod, env)
		if err != nil {
			module.LogEvent(mod.Name, module.EventPreLaunch, err.Error(), nil)
			fmt.Fprintf(os.Stderr, "houston: [%s] preLaunch skipped: %v\n", mod.Name, err)
			continue
		}
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		err = cmd.Run()
		cleanup()
		if err == nil {
			continue
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			fmt.Fprintf(os.Stderr, "houston: [%s] launch cancelled (exit %d)\n", mod.Name, ee.ExitCode())
			return false
		}
		module.LogEvent(mod.Name, module.EventPreLaunch, "interactive: "+err.Error(), nil)
		fmt.Fprintf(os.Stderr, "houston: [%s] preLaunch failed: %v\n", mod.Name, err)
	}
	return true
}

// --- balanced launch ------------------------------------------------------

func cmdRun(args []string) {
	accs, err := accounts.Load()
	if err != nil || len(accs) == 0 {
		fmt.Fprintln(os.Stderr, "houston: no accounts; add one with 'houston account add'")
		os.Exit(1)
	}
	// Self-heal the shared data links before handing the terminal to claude:
	// the plans/todos junctions drift recurrently, and a drifted dir traps
	// every plan/todo the launched session writes. Safe relinks are silent;
	// merges and anything left for doctor are worth a line.
	for _, n := range provision.Heal(accs).Notices() {
		fmt.Fprintln(os.Stderr, "houston: "+n)
	}
	forcedID, rest, dangling := extractAccountFlag(args)
	if dangling {
		fmt.Fprintln(os.Stderr, "houston: -a/--account requires an account id")
		os.Exit(1)
	}
	if forcedID != "" {
		// Forced account: no balancing decision to make, so skip the usage probe
		// (multi-second wait) and launch straight away.
		a, ok := findAccount(accs, forcedID)
		if !ok {
			fmt.Fprintf(os.Stderr, "houston: no such account %q\n", forcedID)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "→ launching: %s (forced)\n", a.ID)
		if !a.LoggedIn() {
			fmt.Fprintln(os.Stderr, "  (account not logged in — type /login inside Claude this first time)")
		}
		if !runPreLaunchHooks("run", a) {
			os.Exit(1) // no session ran: scripts must be able to tell
		}
		accounts.TouchUse(a.ID, accounts.Now())
		if err := launch.Cmd(a.ResolveConfigDir(), rest, "").Run(); err != nil {
			os.Exit(1)
		}
		return
	}
	best, probes, err := usage.Best(accs, 8*time.Second)
	if err != nil {
		fmt.Fprintln(os.Stderr, "houston:", err)
		os.Exit(1)
	}
	// While any account isn't logged in yet, target it first so `houston run`
	// walks the user through logging in each one (Best only ranks accounts
	// whose usage probe succeeds, so un-logged-in ones would never be picked).
	for _, a := range accs {
		if !a.LoggedIn() {
			best = a
			break
		}
	}
	printAccountsTable(probes, best.ID)
	if n := update.Notice(version, 3*time.Second); n != "" {
		fmt.Fprintln(os.Stderr, "houston: "+n)
	}
	if !best.LoggedIn() {
		fmt.Fprintln(os.Stderr, "  (account not logged in — type /login inside Claude this first time)")
	}
	if !runPreLaunchHooks("run", best) {
		os.Exit(1) // no session ran: scripts must be able to tell
	}
	accounts.TouchUse(best.ID, accounts.Now())
	// No token injection: identity (and the email) comes from the account dir's
	// own login (CLAUDE_CONFIG_DIR). The token is only used above to probe load.
	// The first time per account, claude asks to log in once.
	cmd := launch.Cmd(best.ResolveConfigDir(), rest, "")
	if err := cmd.Run(); err != nil {
		os.Exit(1)
	}
}

// extractAccountFlag pulls an explicit account selector (-a/--account <id> or
// --account=<id>) out of the args; everything else is passed through to claude.
// dangling reports a trailing -a/--account with no id (an error, not "balance").
func extractAccountFlag(args []string) (id string, rest []string, dangling bool) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-a" || a == "--account":
			if i+1 < len(args) {
				id = args[i+1]
				i++
			} else {
				dangling = true
			}
		case strings.HasPrefix(a, "--account="):
			id = strings.TrimPrefix(a, "--account=")
		default:
			rest = append(rest, a)
		}
	}
	return id, rest, dangling
}

func findAccount(accs []accounts.Account, id string) (accounts.Account, bool) {
	for _, a := range accs {
		if a.ID == id {
			return a, true
		}
	}
	return accounts.Account{}, false
}

// printAccountsTable shows every account with its email + current usage before
// handing the terminal to claude, marking (→) the one being launched.
func printAccountsTable(probes []usage.Probe, bestID string) {
	// Pressure is shown next to the raw windows: it's the number the choice is
	// actually made on, and seeing it exposes a bad ranking at a glance.
	fmt.Fprintln(os.Stderr, "Claude accounts — usage (5h / 7d → pressure):")
	for _, p := range probes {
		mark := "  "
		if p.Account.ID == bestID {
			mark = "→ "
		}
		email := p.Account.Email()
		if email == "" {
			email = "(not logged in yet)"
		}
		use := "    —  /   —"
		if p.OK {
			use = fmt.Sprintf("%4.0f%% / %4.0f%%  → %3.0f%%", p.U5, p.U7, p.Pressure)
		}
		fmt.Fprintf(os.Stderr, "%s%-8s %-34s %s\n", mark, p.Account.ID, trunc(email, 34), use)
	}
	be := ""
	for _, p := range probes {
		if p.Account.ID == bestID {
			if e := p.Account.Email(); e != "" {
				be = " (" + e + ")"
			}
		}
	}
	fmt.Fprintf(os.Stderr, "→ launching: %s%s\n", bestID, be)
	// A brief pause to read the table before claude paints the screen. With a
	// single account there is nothing to compare, so no wait.
	if len(probes) > 1 {
		time.Sleep(700 * time.Millisecond)
	}
}

// --- self-update ----------------------------------------------------------

// cmdUpdate implements `houston update`: it checks GitHub Releases and, with a
// clear pre-warning + confirmation, downloads the verified binary for this
// platform and swaps it in over the running one.
//
//	houston update            check and, if newer, update (asks first)
//	houston update --check    only report; touch nothing
//	houston update -y         don't prompt (for scripts)
//	houston update --force    reinstall even if up to date / on a dev build
func cmdUpdate(args []string) {
	yes, force, checkOnly := false, false, false
	for _, a := range args {
		switch a {
		case "-y", "--yes":
			yes = true
		case "--force", "-f":
			force = true
		case "--check", "-n", "--dry-run":
			checkOnly = true
		default:
			fmt.Fprintf(os.Stderr, "houston: unknown option %q\n", a)
			fmt.Fprintln(os.Stderr, "usage: houston update [--check] [-y|--yes] [--force]")
			os.Exit(1)
		}
	}

	if version == "dev" && !force && !checkOnly {
		fmt.Println("houston: this is a development build (version \"dev\"); it won't self-update.")
		fmt.Println("  Install it with the installer, or use 'houston update --force' to fetch the latest release.")
		return
	}

	fmt.Fprintln(os.Stderr, "Checking GitHub Releases for the latest version…")
	latest := update.FetchLatest(8 * time.Second)
	if latest == "" {
		fmt.Fprintln(os.Stderr, "houston: couldn't reach GitHub Releases (offline?). Try again later.")
		os.Exit(1)
	}
	upToDate := !update.Newer(latest, version)

	if checkOnly {
		if upToDate {
			fmt.Printf("You're up to date: %s is the latest version.\n", version)
		} else {
			fmt.Printf("A new version is available: %s (you have %s).\n  Update with:  houston update\n", latest, version)
		}
		return
	}
	if upToDate && !force {
		fmt.Printf("Already on the latest version (%s). Nothing to do.\n", version)
		fmt.Println("  (use 'houston update --force' to reinstall it)")
		return
	}

	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "houston: couldn't locate my own binary:", err)
		os.Exit(1)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}

	// --- pre-warning: explain what's about to happen before touching anything
	fmt.Println()
	if force && upToDate {
		fmt.Printf("Reinstalling Houston %s (forced) at:\n", latest)
	} else {
		fmt.Printf("About to update Houston:  %s  →  %s\n", version, latest)
	}
	fmt.Printf("  binary:  %s\n", exe)
	fmt.Println()
	fmt.Println("Before continuing:")
	fmt.Println("  • Close any other terminals/tabs where 'houston' or 'claude' is open.")
	fmt.Println("  • Sessions left open will keep the current version until you restart them.")
	if runtime.GOOS == "windows" {
		fmt.Println("  • On Windows the in-use binary is moved aside as 'houston.exe.old'; it's cleaned up automatically later.")
	}
	fmt.Println()

	if !yes {
		fmt.Print("Continue? [y/N]: ")
		resp, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		switch strings.ToLower(strings.TrimSpace(resp)) {
		case "y", "yes":
		default:
			fmt.Println("Cancelled. Nothing was changed.")
			return
		}
	}

	fmt.Fprintln(os.Stderr, "Downloading and verifying the binary…")
	bin, file, err := update.DownloadVerified(latest, 90*time.Second)
	if err != nil {
		fmt.Fprintln(os.Stderr, "houston: update aborted (nothing was changed):", err)
		os.Exit(1)
	}
	leftover, err := update.Swap(exe, bin)
	if err != nil {
		fmt.Fprintln(os.Stderr, "houston: couldn't replace the binary:", err)
		os.Exit(1)
	}

	fmt.Printf("\n✓ Houston updated to %s (%s).\n", latest, file)
	if leftover != "" {
		fmt.Printf("  (the previous binary is still in use by another session; it'll be cleaned up automatically: %s)\n", leftover)
	}
	fmt.Println("  Open a new terminal (or restart open ones) to use the new version.")
}

// --- doctor: audit & repair the multi-account layout ----------------------

func cmdDoctor(args []string) {
	fix, resync := false, false
	for _, a := range args {
		switch a {
		case "--fix", "fix", "-f":
			fix = true
		case "--resync-settings":
			resync = true
		}
	}
	accs, _ := accounts.Load()
	if len(accs) == 0 {
		fmt.Fprintln(os.Stderr, "houston: no accounts; add one with 'houston account add'")
		os.Exit(1)
	}

	sharedMissing, reports := provision.Audit(accs)
	fmt.Printf("Shared store: %s\n", provision.SharedDir())
	if len(sharedMissing) > 0 {
		fmt.Printf("  missing dirs: %s\n", strings.Join(sharedMissing, ", "))
	} else {
		fmt.Println("  all dirs present")
	}

	drift := len(sharedMissing) > 0
	for _, r := range reports {
		fmt.Printf("\n[%s] %s\n", r.Account.ID, r.ConfigDir)
		login := "not logged in"
		if r.LoggedIn {
			login = "logged in ✓"
		}
		cfg := ".claude.json ✓"
		if !r.HasConfig {
			cfg = ".claude.json MISSING"
		}
		fmt.Printf("  %s · %s\n", login, cfg)
		for _, d := range r.Dirs {
			mark := "  ✓"
			if !d.State.OK() {
				mark = "  ✗"
			}
			fmt.Printf("  %s %-14s %s\n", mark, d.Name, d.State)
		}
		if r.HasDrift() {
			drift = true
		}
	}

	doctorModules()

	if n := update.Notice(version, 3*time.Second); n != "" {
		fmt.Println("\nhouston: " + n)
	}
	if resync {
		// settings.json/mcp.json are seeded (copied), not linked — edits to the
		// shared file don't propagate on their own. This is the propagation step.
		fmt.Println("\nResyncing settings from the shared store…")
		res, err := provision.ResyncSettings(accs)
		if err != nil {
			fmt.Fprintln(os.Stderr, "houston: resync failed:", err)
			os.Exit(1)
		}
		for _, c := range res.Created {
			fmt.Println("  + " + c)
		}
		for _, s := range res.Skipped {
			fmt.Println("  ! " + s)
		}
		if len(res.Created) == 0 && len(res.Skipped) == 0 {
			fmt.Println("  (nothing to copy — no seed files in the shared store)")
		}
	}
	if !fix {
		if drift {
			fmt.Println("\nDrift detected. Repair with:  houston doctor --fix")
		} else {
			fmt.Println("\nAll good.")
		}
		return
	}

	fmt.Println("\nRepairing…")
	res, err := provision.Fix(accs)
	if err != nil {
		fmt.Fprintln(os.Stderr, "houston: repair failed:", err)
		os.Exit(1)
	}
	for _, c := range res.Created {
		fmt.Println("  + " + c)
	}
	for _, s := range res.Skipped {
		fmt.Println("  ! " + s)
	}
	if len(res.Created) == 0 && len(res.Skipped) == 0 {
		fmt.Println("  (nothing to change)")
	}
	fmt.Println("Done.")
}

// doctorModules runs the static (zero-exec) module checks: registry vs disk
// state, manifest health, command resolution, key collisions, theme values,
// orphaned install staging dirs, and the resolved theme with per-field layer
// attribution. Broken manifests (unsupported api, absolute/escaping commands,
// ctrl-alias keys) surface through LoadAll's per-module errors.
func doctorModules() {
	cfg := config.Load()
	mods, errs := module.LoadAll(cfg)
	fmt.Printf("\nModules: %s\n", module.Dir())
	// Load() hides a malformed config.json by design (zero value, never
	// fatal); doctor is where the user learns their file is being ignored.
	if err := config.Check(); err != nil {
		fmt.Println("  ✗ config.json: " + err.Error() + " (file ignored)")
	}
	if len(mods) == 0 && len(errs) == 0 {
		fmt.Println("  none installed")
	}
	for _, m := range mods {
		state := "disabled"
		if m.Enabled {
			state = "enabled"
		}
		fmt.Printf("  ✓ %s (%s) %s\n", m.Name, state, surfacesSummary(m.Manifest))
		for _, w := range commandWarnings(m.Manifest, m.Dir) {
			fmt.Println("      ! " + w)
		}
		if m.Manifest.Theme != nil {
			for _, w := range checkThemeOverrides(*m.Manifest.Theme) {
				fmt.Println("      ! theme: " + w)
			}
		}
	}
	for _, w := range shadowedKeys(mods) {
		fmt.Println("  ! " + w)
	}
	for _, e := range errs {
		fmt.Println("  ✗ " + e.Error())
	}
	for _, s := range stagingLeftovers() {
		fmt.Printf("  ! orphaned install staging dir: %s (safe to delete)\n", s)
	}
	for _, n := range module.SegCacheOrphans() {
		fmt.Printf("  ! stale segment-cache entry %q (module no longer installed; safe to delete modules-seg-cache.json)\n", n)
	}
	for _, w := range checkThemeOverrides(cfg.Theme) {
		fmt.Println("  ! config.json theme: " + w)
	}
	printResolvedTheme(mods, cfg)
}

// stagingLeftovers lists .staging-* dirs abandoned by a crashed install (the
// TUI sweeps them at start once the module runtime lands; until then doctor
// points at them).
func stagingLeftovers() []string {
	var out []string
	dirents, _ := os.ReadDir(module.Dir())
	for _, d := range dirents {
		if d.IsDir() && strings.HasPrefix(d.Name(), ".staging-") {
			out = append(out, filepath.Join(module.Dir(), d.Name()))
		}
	}
	return out
}

// knownThemeColors mirrors the theme.Colors JSON keys (theme merges match
// them case-insensitively): doctor keeps its own list so an unknown key gets
// a note here instead of only the silent field-wise skip Merge applies.
var knownThemeColors = map[string]bool{
	"accent": true, "grey": true, "dim": true, "green": true, "yellow": true,
	"selbg": true, "slgreen": true, "slamber": true, "slred": true,
	"sldim": true, "slactive": true,
}

// checkThemeOverrides reports the override fields a Merge would silently
// skip: unknown color keys, non-ANSI-256 color values, out-of-range layout.
func checkThemeOverrides(o theme.Overrides) []string {
	var out []string
	keys := make([]string, 0, len(o.Colors))
	for k := range o.Colors {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := o.Colors[k]
		if !knownThemeColors[strings.ToLower(k)] {
			out = append(out, fmt.Sprintf("unknown color key %q (ignored)", k))
			continue
		}
		if n, err := strconv.Atoi(v); err != nil || n < 0 || n > 255 || v != strconv.Itoa(n) {
			out = append(out, fmt.Sprintf("color %q = %q is not an ANSI-256 code 0-255 (ignored)", k, v))
		}
	}
	if l := o.Layout; l != nil {
		if l.LeftWidth < 0 {
			out = append(out, fmt.Sprintf("layout.leftWidth %d ignored (must be > 0)", l.LeftWidth))
		}
		if l.RightPercent < 0 || l.RightPercent > 100 {
			out = append(out, fmt.Sprintf("layout.rightPercent %d ignored (must be 1-100)", l.RightPercent))
		}
		if l.RightMin < 0 {
			out = append(out, fmt.Sprintf("layout.rightMin %d ignored (must be > 0)", l.RightMin))
		}
	}
	return out
}

// printResolvedTheme shows the final theme and which layer set each field:
// defaults < enabled module themes in lexicographic name order < config.json
// — the same chain the TUI and statusline resolve at startup.
func printResolvedTheme(mods []module.Module, cfg config.Config) {
	t := theme.Default()
	layer := map[string]string{}
	for _, f := range themeFields(t) {
		layer[f.name] = "default"
	}
	apply := func(o theme.Overrides, who string) {
		before := themeFields(t)
		t = t.Merge(o)
		for i, f := range themeFields(t) {
			if f.value != before[i].value {
				layer[f.name] = who
			}
		}
	}
	for _, m := range mods {
		if m.Enabled && m.Manifest.Theme != nil {
			apply(*m.Manifest.Theme, "module "+m.Name)
		}
	}
	apply(cfg.Theme, "config.json")
	fmt.Println("  resolved theme:")
	for _, f := range themeFields(t) {
		fmt.Printf("    %-14s %-5s (%s)\n", f.name, f.value, layer[f.name])
	}
}

// themeField is one theme field flattened for display and layer diffing.
type themeField struct{ name, value string }

func themeFields(t theme.Theme) []themeField {
	c, l := t.Colors, t.Layout
	return []themeField{
		{"accent", c.Accent}, {"grey", c.Grey}, {"dim", c.Dim}, {"green", c.Green},
		{"yellow", c.Yellow}, {"selBg", c.SelBg}, {"slGreen", c.SLGreen},
		{"slAmber", c.SLAmber}, {"slRed", c.SLRed}, {"slDim", c.SLDim},
		{"slActive", c.SLActive},
		{"leftWidth", strconv.Itoa(l.LeftWidth)},
		{"rightPercent", strconv.Itoa(l.RightPercent)},
		{"rightMin", strconv.Itoa(l.RightMin)},
	}
}

// --- TUI / debug ----------------------------------------------------------

func cmdTUI(args []string) {
	var explicitRoot string
	for _, a := range args {
		if a != "--paths" {
			explicitRoot = a
		}
	}
	scanFn := scan.ScanAll
	displayRoot := "auto (shared + accounts)"
	if explicitRoot != "" {
		displayRoot = explicitRoot
		scanFn = func() ([]model.Mission, error) { return scan.Scan(explicitRoot) }
	}

	if len(args) > 0 && args[0] == "--paths" {
		missions, err := scanFn()
		if err != nil {
			fmt.Fprintln(os.Stderr, "houston:", err)
			os.Exit(1)
		}
		fmt.Println("roots:")
		for _, r := range scan.ProjectRoots() {
			fmt.Printf("  %s\n", r)
		}
		fmt.Println()
		for _, m := range missions {
			fmt.Printf("%-8.8s  proj=%-52s  resume-cwd=%s\n", m.ID, m.Project, m.Cwd)
		}
		return
	}

	st, err := store.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "houston: couldn't load the store:", err)
		os.Exit(1)
	}
	missions, err := scanFn()
	if err != nil {
		fmt.Fprintln(os.Stderr, "houston: scan failed:", err)
		os.Exit(1)
	}
	// Theme precedence: defaults < enabled module themes (lexicographic name
	// order, later wins per field) < config.json — installed code must never
	// override the user's explicit taste. Load warnings ride along so a
	// broken enabled module surfaces once at startup, not only in ls/doctor.
	cfg := config.Load()
	mods, loadWarns := module.LoadEnabled(cfg)
	th := theme.Resolve(module.ThemeOverrides(mods), cfg.Theme)
	if err := tui.Run(displayRoot, scanFn, st, missions, th, mods, loadWarns); err != nil {
		fmt.Fprintln(os.Stderr, "houston:", err)
		os.Exit(1)
	}
}

func trunc(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
