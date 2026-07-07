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
	m := New("root", func() ([]model.Mission, error) { return fakeMissions(), nil }, st, fakeMissions(), nil)
	return drive(m, tea.WindowSizeMsg{Width: 100, Height: 30})
}

func TestInitialListAllMissions(t *testing.T) {
	m := newModel(t)
	if len(m.mid) != 3 {
		t.Fatalf("expected 3 missions under 'All', got %d", len(m.mid))
	}
	// sorted by recency: aaaa first
	if m.mid[0].ID != "aaaa1111" {
		t.Errorf("recency order wrong: %s", m.mid[0].ID)
	}
}

func TestSearchFilters(t *testing.T) {
	m := newModel(t)
	// '/', then type "pok"
	m = drive(m, runes("/"), runes("p"), runes("o"), runes("k"))
	// pokewalker + pokemon icons match "pok"; ankis does not
	if len(m.mid) != 2 {
		t.Fatalf("search 'pok' should give 2, got %d", len(m.mid))
	}
	m = drive(m, key(tea.KeyEnter))
	if m.act != actNone {
		t.Errorf("enter should close input mode")
	}
}

func TestPinTogglesAndPersists(t *testing.T) {
	m := newModel(t)
	target := m.mid[m.midCur].Key()
	m = drive(m, runes("*"))
	if !m.st.MetaOf(target).Pinned {
		t.Fatalf("the mission should end up pinned")
	}
	m = drive(m, runes("*"))
	if m.st.MetaOf(target).Pinned {
		t.Fatalf("a second press should unpin")
	}
}

func TestArchiveRemovesFromAll(t *testing.T) {
	m := newModel(t)
	target := m.mid[m.midCur].Key()
	m = drive(m, runes("a")) // archive
	if !m.st.MetaOf(target).Archived {
		t.Fatalf("it should be archived")
	}
	for _, ms := range m.mid {
		if ms.Key() == target {
			t.Fatalf("an archived mission should not stay under 'All'")
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
		t.Fatalf("program 'Pok' should exist")
	}
	if len(p.Missions) != 1 || p.Missions[0] != target {
		t.Fatalf("the mission should be in the program: %+v", p.Missions)
	}
	// left pane should now include the program
	found := false
	for _, it := range m.left {
		if it.kind == lkProgram && it.prog == "Pok" {
			found = true
		}
	}
	if !found {
		t.Errorf("the left pane should list the program")
	}
}
