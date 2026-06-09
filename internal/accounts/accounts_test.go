package accounts

import "testing"

func TestCRUD(t *testing.T) {
	t.Setenv("HOUSTON_HOME", t.TempDir())

	if a, _ := Load(); len(a) != 0 {
		t.Fatalf("esperaba 0 cuentas iniciales, hay %d", len(a))
	}
	a1, err := Add("work@x.com", "2026-05-28T10:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	if a1.ID != "work-x-com" {
		t.Errorf("slug id inesperado: %q", a1.ID)
	}
	if _, err := Add("work@x.com", "2026-05-28T11:00:00Z"); err != nil {
		t.Fatal(err)
	}
	list, _ := Load()
	if len(list) != 2 {
		t.Fatalf("esperaba 2 cuentas, hay %d", len(list))
	}
	if list[1].ID != "work-x-com-2" {
		t.Errorf("colisión de id no resuelta: %q", list[1].ID)
	}
	if list[0].Label != "work@x.com" {
		t.Errorf("label mal guardado: %q", list[0].Label)
	}

	// Token is optional now (identity comes from the per-account /login).
	if _, err := Add("no-token", "2026-05-28T12:00:00Z"); err != nil {
		t.Errorf("añadir cuenta sin token debería permitirse, dio: %v", err)
	}
	Remove("no-token")

	Remove(a1.ID)
	list, _ = Load()
	if len(list) != 1 || list[0].ID != "work-x-com-2" {
		t.Fatalf("Remove no dejó la cuenta correcta: %+v", list)
	}
}

