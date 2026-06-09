package tui

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"houston/internal/model"
	"houston/internal/store"
)

func runes(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }
func key(t tea.KeyType) tea.KeyMsg { return tea.KeyMsg{Type: t} }

func fakeMissions() []model.Mission {
	now := time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)
	return []model.Mission{
		{ID: "aaaa1111", Project: "C--Users-TESTER-Documents-Github-Pokemon", Cwd: `C:\p`, Title: "Pokewalker bridge work", LastTime: now, UserMsgs: 5, AssistantMsgs: 5, Search: "pokewalker bridge rom"},
		{ID: "bbbb2222", Project: "C--Users-TESTER-Downloads-Maria", Cwd: `C:\m`, Title: "Ankis digestivo", LastTime: now.Add(-time.Hour), UserMsgs: 3, AssistantMsgs: 3, Search: "anki esofago higado"},
		{ID: "cccc3333", Project: "C--Users-TESTER", Cwd: `C:\u`, Title: "Pokemon icons python", LastTime: now.Add(-2 * time.Hour), UserMsgs: 2, AssistantMsgs: 2, Search: "bitmap icon generator"},
	}
}

// drive applies a sequence of messages and returns the resulting Model.
func drive(m Model, msgs ...tea.Msg) Model {
	var tm tea.Model = m
	for _, msg := range msgs {
		tm, _ = tm.Update(msg)
	}
	return tm.(Model)
}

func newModel(t *testing.T) Model {
	t.Helper()
	st, err := store.LoadFrom(t.TempDir())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	m := New("root", func() ([]model.Mission, error) { return fakeMissions(), nil }, st, fakeMissions())
	return drive(m, tea.WindowSizeMsg{Width: 100, Height: 30})
}

func TestInitialListAllMissions(t *testing.T) {
	m := newModel(t)
	if len(m.mid) != 3 {
		t.Fatalf("esperaba 3 misiones en 'Todas', hay %d", len(m.mid))
	}
	// sorted by recency: aaaa first
	if m.mid[0].ID != "aaaa1111" {
		t.Errorf("orden por recencia incorrecto: %s", m.mid[0].ID)
	}
}

func TestSearchFilters(t *testing.T) {
	m := newModel(t)
	// '/', then type "pok"
	m = drive(m, runes("/"), runes("p"), runes("o"), runes("k"))
	// pokewalker + pokemon icons match "pok"; ankis does not
	if len(m.mid) != 2 {
		t.Fatalf("búsqueda 'pok' debería dar 2, dio %d", len(m.mid))
	}
	m = drive(m, key(tea.KeyEnter))
	if m.act != actNone {
		t.Errorf("enter debería cerrar el modo input")
	}
}

func TestPinTogglesAndPersists(t *testing.T) {
	m := newModel(t)
	target := m.mid[m.midCur].Key()
	m = drive(m, runes("*"))
	if !m.st.MetaOf(target).Pinned {
		t.Fatalf("la misión debería quedar fijada")
	}
	m = drive(m, runes("*"))
	if m.st.MetaOf(target).Pinned {
		t.Fatalf("segunda pulsación debería desfijar")
	}
}

func TestArchiveRemovesFromAll(t *testing.T) {
	m := newModel(t)
	target := m.mid[m.midCur].Key()
	m = drive(m, runes("a")) // archive
	if !m.st.MetaOf(target).Archived {
		t.Fatalf("debería estar archivada")
	}
	for _, ms := range m.mid {
		if ms.Key() == target {
			t.Fatalf("la archivada no debería seguir en 'Todas'")
		}
	}
}

func TestCreateProgramAndAddMission(t *testing.T) {
	m := newModel(t)
	target := m.mid[m.midCur].Key()
	// add selected mission to a new program "Pokemon" via 'p'
	m = drive(m, runes("p"), runes("P"), runes("o"), runes("k"), key(tea.KeyEnter))
	p := m.st.ProgramByName("Pok")
	if p == nil {
		t.Fatalf("el programa 'Pok' debería existir")
	}
	if len(p.Missions) != 1 || p.Missions[0] != target {
		t.Fatalf("la misión debería estar en el programa: %+v", p.Missions)
	}
	// left pane should now include the program
	found := false
	for _, it := range m.left {
		if it.kind == lkProgram && it.prog == "Pok" {
			found = true
		}
	}
	if !found {
		t.Errorf("el panel izquierdo debería listar el programa")
	}
}
