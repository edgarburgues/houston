package tui

import (
	"os"
	"path/filepath"
	"testing"

	"houston/internal/model"
	"houston/internal/store"
)

func TestAdoptMissionsOverridesAndFlagsMissing(t *testing.T) {
	dir := t.TempDir()
	st, err := store.LoadFrom(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gone := filepath.Join(dir, "no-longer-here")
	moved := filepath.Join(dir, "new-location")
	if err := os.MkdirAll(moved, 0o700); err != nil {
		t.Fatal(err)
	}
	// m1 moved: the override points at the new, existing location.
	if err := st.SetCwdOverride("p/m1", moved); err != nil {
		t.Fatal(err)
	}

	m := Model{st: st}
	m.adoptMissions([]model.Mission{
		{ID: "m1", Project: "p", Cwd: gone},  // override rescues it
		{ID: "m2", Project: "p", Cwd: gone},  // truly missing
		{ID: "m3", Project: "p", Cwd: moved}, // fine as scanned
		{ID: "m4", Project: "p", Cwd: ""},    // no cwd: never flagged
	})

	if got := m.missions[0].Cwd; got != moved {
		t.Fatalf("override not applied: %q", got)
	}
	if m.cwdMissing["p/m1"] {
		t.Fatal("overridden mission must not be flagged")
	}
	if !m.cwdMissing["p/m2"] {
		t.Fatal("missing cwd must be flagged")
	}
	if m.cwdMissing["p/m3"] || m.cwdMissing["p/m4"] {
		t.Fatalf("false positives: %+v", m.cwdMissing)
	}
}

func TestAdoptMissionsNilStore(t *testing.T) {
	m := Model{}
	m.adoptMissions([]model.Mission{{ID: "m1", Project: "p", Cwd: t.TempDir()}})
	if len(m.cwdMissing) != 0 {
		t.Fatalf("existing dir flagged: %+v", m.cwdMissing)
	}
}
