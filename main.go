// Houston — mission-control for Claude Code: balanced multi-account launching
// (one config dir per account, shared data) plus a TUI to browse, organize and
// resume conversations.
package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"houston/internal/accounts"
	"houston/internal/launch"
	"houston/internal/model"
	"houston/internal/scan"
	"houston/internal/store"
	"houston/internal/tui"
	"houston/internal/usage"
)

func main() {
	args := os.Args[1:]
	switch {
	case len(args) > 0 && args[0] == "account":
		cmdAccount(args[1:])
	case len(args) > 0 && args[0] == "run":
		cmdRun(args[1:])
	default:
		cmdTUI(args)
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
			fmt.Fprintln(os.Stderr, "uso: houston account add <etiqueta>")
			os.Exit(1)
		}
		acc, err := accounts.Add(label, accounts.Now())
		if err != nil {
			fmt.Fprintln(os.Stderr, "houston:", err)
			os.Exit(1)
		}
		fmt.Printf("cuenta añadida: %s (%s)\n", acc.ID, acc.Label)
		fmt.Println("siguiente: 'houston run' — la primera vez abrirá /login en el navegador para esta cuenta.")
	case "ls", "list", "":
		accs, _ := accounts.Load()
		if len(accs) == 0 {
			fmt.Println("sin cuentas. Añade una:  houston account add <etiqueta>")
			return
		}
		fmt.Fprintln(os.Stderr, "sondeando uso...")
		probes := usage.ProbeAll(accs, 8*time.Second)
		fmt.Printf("%-16s %-30s %-8s %s\n", "ID", "EMAIL / ETIQUETA", "PRESIÓN", "5h / 7d")
		for _, p := range probes {
			name := p.Account.Email()
			if name == "" {
				name = p.Account.Label
			}
			switch {
			case p.OK:
				fmt.Printf("%-16s %-30s %6.0f%%   %.0f%% / %.0f%%\n",
					p.Account.ID, trunc(name, 30), pct(p.Pressure), pct(p.U5), pct(p.U7))
			case !p.Account.LoggedIn():
				fmt.Printf("%-16s %-30s   (sin login todavía)\n", p.Account.ID, trunc(name, 30))
			default:
				fmt.Printf("%-16s %-30s   (sin uso: %s)\n", p.Account.ID, trunc(name, 30), p.Err)
			}
		}
	case "rm", "remove":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "uso: houston account rm <id>")
			os.Exit(1)
		}
		if err := accounts.Remove(args[1]); err != nil {
			fmt.Fprintln(os.Stderr, "houston:", err)
			os.Exit(1)
		}
		fmt.Println("cuenta eliminada:", args[1])
	default:
		fmt.Fprintln(os.Stderr, "uso: houston account [add <etiqueta> | ls | rm <id>]")
		os.Exit(1)
	}
}

// --- balanced launch ------------------------------------------------------

func cmdRun(args []string) {
	accs, err := accounts.Load()
	if err != nil || len(accs) == 0 {
		fmt.Fprintln(os.Stderr, "houston: no hay cuentas; añade una con 'houston account add'")
		os.Exit(1)
	}
	forcedID, rest := extractAccountFlag(args)
	best, probes, err := usage.Best(accs, 8*time.Second)
	if err != nil {
		fmt.Fprintln(os.Stderr, "houston:", err)
		os.Exit(1)
	}
	switch {
	case forcedID != "":
		a, ok := findAccount(accs, forcedID)
		if !ok {
			fmt.Fprintf(os.Stderr, "houston: no existe la cuenta %q\n", forcedID)
			os.Exit(1)
		}
		best = a
	default:
		// While any account isn't logged in yet, target it first so `houston run`
		// walks the user through logging in each one (Best only ranks accounts
		// whose usage probe succeeds, so un-logged-in ones would never be picked).
		for _, a := range accs {
			if !a.LoggedIn() {
				best = a
				break
			}
		}
	}
	printAccountsTable(probes, best.ID)
	if !best.LoggedIn() {
		fmt.Fprintln(os.Stderr, "  (cuenta sin login — escribe /login dentro de Claude esta primera vez)")
	}
	accounts.TouchUse(best.ID, accounts.Now())
	// No inyectamos el token: la identidad (y el email) viene del login propio de
	// la carpeta de la cuenta (CLAUDE_CONFIG_DIR). El token solo se usa arriba para
	// sondear la carga. La primera vez por cuenta, claude pedirá login una vez.
	cmd := launch.Cmd(best.ResolveConfigDir(), rest, "")
	if err := cmd.Run(); err != nil {
		os.Exit(1)
	}
}

// extractAccountFlag pulls an explicit account selector (-a/--account <id> or
// --account=<id>) out of the args; everything else is passed through to claude.
func extractAccountFlag(args []string) (id string, rest []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-a" || a == "--account":
			if i+1 < len(args) {
				id = args[i+1]
				i++
			}
		case strings.HasPrefix(a, "--account="):
			id = strings.TrimPrefix(a, "--account=")
		default:
			rest = append(rest, a)
		}
	}
	return id, rest
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
	fmt.Fprintln(os.Stderr, "Cuentas Claude — uso (5h / 7d):")
	for _, p := range probes {
		mark := "  "
		if p.Account.ID == bestID {
			mark = "→ "
		}
		email := p.Account.Email()
		if email == "" {
			email = "(sin login todavía)"
		}
		use := "    —  /   —"
		if p.OK {
			use = fmt.Sprintf("%4.0f%% / %4.0f%%", pct(p.U5), pct(p.U7))
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
	fmt.Fprintf(os.Stderr, "→ lanzando: %s%s\n", bestID, be)
	time.Sleep(1500 * time.Millisecond) // un instante para poder leer la tabla
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
	displayRoot := "auto (shared + cuentas)"
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
		fmt.Fprintln(os.Stderr, "houston: no pude cargar el store:", err)
		os.Exit(1)
	}
	missions, err := scanFn()
	if err != nil {
		fmt.Fprintln(os.Stderr, "houston: error escaneando:", err)
		os.Exit(1)
	}
	if err := tui.Run(displayRoot, scanFn, st, missions); err != nil {
		fmt.Fprintln(os.Stderr, "houston:", err)
		os.Exit(1)
	}
}

// pct returns the utilization as a percentage. The OAuth usage endpoint already
// reports it in percent (e.g. 41 = 41%, 1 = 1%), so it's used as-is — no scaling
// (an earlier ×100 turned a real 1% into 100%).
func pct(v float64) float64 { return v }

func trunc(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
