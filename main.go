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
	"houston/internal/provision"
	"houston/internal/scan"
	"houston/internal/statusline"
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
					p.Account.ID, trunc(name, 30), p.Pressure, p.U5, p.U7)
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
			use = fmt.Sprintf("%4.0f%% / %4.0f%%", p.U5, p.U7)
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
	// Una pausa breve para leer la tabla antes de que claude pinte la pantalla.
	// Con una sola cuenta no hay nada que comparar, así que no esperamos.
	if len(probes) > 1 {
		time.Sleep(700 * time.Millisecond)
	}
}

// --- doctor: audit & repair the multi-account layout ----------------------

func cmdDoctor(args []string) {
	fix := false
	for _, a := range args {
		if a == "--fix" || a == "fix" || a == "-f" {
			fix = true
		}
	}
	accs, _ := accounts.Load()
	if len(accs) == 0 {
		fmt.Fprintln(os.Stderr, "houston: no hay cuentas; añade una con 'houston account add'")
		os.Exit(1)
	}

	sharedMissing, reports := provision.Audit(accs)
	fmt.Printf("Store compartido: %s\n", provision.SharedDir())
	if len(sharedMissing) > 0 {
		fmt.Printf("  faltan dirs: %s\n", strings.Join(sharedMissing, ", "))
	} else {
		fmt.Println("  todos los dirs presentes")
	}

	drift := len(sharedMissing) > 0
	for _, r := range reports {
		fmt.Printf("\n[%s] %s\n", r.Account.ID, r.ConfigDir)
		login := "sin login"
		if r.LoggedIn {
			login = "login ✓"
		}
		cfg := ".claude.json ✓"
		if !r.HasConfig {
			cfg = ".claude.json FALTA"
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

	if !fix {
		if drift {
			fmt.Println("\nHay deriva. Repara con:  houston doctor --fix")
		} else {
			fmt.Println("\nTodo en orden.")
		}
		return
	}

	fmt.Println("\nReparando…")
	res, err := provision.Fix(accs)
	if err != nil {
		fmt.Fprintln(os.Stderr, "houston: fallo reparando:", err)
		os.Exit(1)
	}
	for _, c := range res.Created {
		fmt.Println("  + " + c)
	}
	for _, s := range res.Skipped {
		fmt.Println("  ! " + s)
	}
	if len(res.Created) == 0 && len(res.Skipped) == 0 {
		fmt.Println("  (nada que cambiar)")
	}
	fmt.Println("Listo.")
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

func trunc(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
