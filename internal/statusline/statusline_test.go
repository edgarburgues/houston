package statusline

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestAccountID(t *testing.T) {
	cases := map[string]string{
		filepath.Join("x", "account-work2"): "work2",
		filepath.Join("x", "account-work"):  "work",
		"":                                  "",
	}
	for in, want := range cases {
		if got := accountID(in); got != want {
			t.Errorf("accountID(%q)=%q, want %q", in, got, want)
		}
	}
}

func TestRenderAllAccountsWithActiveMark(t *testing.T) {
	ctx := 12.0
	rows := []row{
		{ID: "work", U5: 12, U7: 3, OK: true},
		{ID: "work2", U5: 41, U7: 7, OK: true, Active: true},
		{ID: "work3", OK: false}, // not logged in / probe failed
	}
	got := Render(rows, "Opus 4.8", &ctx)
	// every account shown, active marked, failed one shown as —
	for _, want := range []string{"work 12/3", "►work2 41/7", "work3 —", "Opus 4.8", "ctx 12%"} {
		if !strings.Contains(got, want) {
			t.Errorf("línea %q no contiene %q", got, want)
		}
	}
	// only the active account carries the ► marker
	if strings.Count(got, "►") != 1 {
		t.Errorf("debería haber exactamente un marcador de cuenta activa: %q", got)
	}
}

func TestRenderNoAccounts(t *testing.T) {
	got := Render(nil, "", nil)
	if !strings.Contains(got, "🚀") {
		t.Errorf("debería seguir mostrando algo aunque no haya cuentas: %q", got)
	}
}
