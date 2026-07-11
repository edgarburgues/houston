package store

import (
	"testing"
)

func TestSetCwdOverrideRoundTrip(t *testing.T) {
	dir := t.TempDir()
	st, err := LoadFrom(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetCwdOverride("p/m1", `C:\moved\here`); err != nil {
		t.Fatal(err)
	}
	if got := st.MetaOf("p/m1").CwdOverride; got != `C:\moved\here` {
		t.Fatalf("override: %q", got)
	}
	// Persisted: a fresh load sees it.
	st2, err := LoadFrom(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := st2.MetaOf("p/m1").CwdOverride; got != `C:\moved\here` {
		t.Fatalf("override after reload: %q", got)
	}
	// Empty clears, and clearing the only meta field drops the entry.
	if err := st2.SetCwdOverride("p/m1", "  "); err != nil {
		t.Fatal(err)
	}
	if got := st2.MetaOf("p/m1").CwdOverride; got != "" {
		t.Fatalf("clear: %q", got)
	}
}
