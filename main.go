// Houston — mission-control for Claude Code: balanced multi-account launching
// (one config dir per account, shared data) plus a TUI to browse, organize and
// resume conversations.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"houston/internal/accounts"
	"houston/internal/browse"
	"houston/internal/config"
	"houston/internal/fleet"
	"houston/internal/launch"
	"houston/internal/model"
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

// --- balanced launch ------------------------------------------------------

func cmdRun(args []string) {
	accs, err := accounts.Load()
	if err != nil || len(accs) == 0 {
		fmt.Fprintln(os.Stderr, "houston: no accounts; add one with 'houston account add'")
		os.Exit(1)
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
	// User overrides only for now; enabled-module theme contributions join the
	// chain (via theme.Resolve) once the module loader exists.
	th := theme.Default().Merge(config.Load().Theme)
	if err := tui.Run(displayRoot, scanFn, st, missions, th); err != nil {
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
