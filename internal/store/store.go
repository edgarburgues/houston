// Package store holds Houston's mutable state: per-mission metadata (tags,
// notes, pin, archive) and the user's Programs. Missions themselves are scanned
// live and never persisted here. Meta lives in store.json; each Program is its
// own .prog manifest under programs/.
package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"houston/internal/accounts"
	"houston/internal/model"
)

// Dir is where Houston keeps its data. It delegates to accounts.StoreDir so
// every Houston store (accounts.json, store.json, programs, exports, caches)
// honors $HOUSTON_HOME consistently instead of splitting across two dirs.
func Dir() string { return accounts.StoreDir() }

type Store struct {
	dir      string
	Meta     map[string]model.Meta `json:"meta"` // key: mission.Key()
	Programs []model.Program        `json:"-"`
}

// Load reads meta + programs from the default data dir.
func Load() (*Store, error) { return LoadFrom(Dir()) }

// LoadFrom reads meta + programs from dir, creating it if needed. Exposed so
// tests can use a temp dir instead of the real ~/.claude/houston.
func LoadFrom(d string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(d, "programs"), 0o755); err != nil {
		return nil, err
	}
	s := &Store{dir: d, Meta: map[string]model.Meta{}}
	if b, err := os.ReadFile(filepath.Join(d, "store.json")); err == nil {
		var on struct {
			Meta map[string]model.Meta `json:"meta"`
		}
		if json.Unmarshal(b, &on) == nil && on.Meta != nil {
			s.Meta = on.Meta
		}
	}
	// load programs
	entries, _ := os.ReadDir(filepath.Join(d, "programs"))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".prog") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(d, "programs", e.Name()))
		if err != nil {
			continue
		}
		var p model.Program
		if json.Unmarshal(b, &p) == nil && p.Name != "" {
			s.Programs = append(s.Programs, p)
		}
	}
	sort.Slice(s.Programs, func(i, j int) bool { return s.Programs[i].Name < s.Programs[j].Name })
	return s, nil
}

// writeAtomic writes b via a uniquely-named same-dir temp file + rename so a
// crash mid-write can't leave a truncated store.json / .prog behind, and two
// concurrent TUIs can't interleave writes into the same temp file.
func writeAtomic(path string, b []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

func (s *Store) saveMeta() error {
	b, err := json.MarshalIndent(struct {
		Meta map[string]model.Meta `json:"meta"`
	}{s.Meta}, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(s.dir, "store.json"), b)
}

// windowsReserved are device names Windows refuses as a file stem — with or
// without an extension ("CON.prog" is as invalid as "CON").
var windowsReserved = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true,
	"COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true,
	"LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

func progFile(dir, name string) string {
	safe := strings.Map(func(r rune) rune {
		if strings.ContainsRune(`<>:"/\|?*`, r) || r < 32 {
			return '_'
		}
		return r
	}, name)
	// Windows silently drops trailing dots/spaces ("foo." collides with "foo"),
	// and refuses reserved device stems outright.
	safe = strings.TrimRight(safe, ". ")
	stem := strings.ToUpper(strings.SplitN(safe, ".", 2)[0])
	if safe == "" || windowsReserved[stem] {
		safe = "_" + safe
	}
	return filepath.Join(dir, "programs", safe+".prog")
}

func (s *Store) saveProgram(p model.Program) error {
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(progFile(s.dir, p.Name), b)
}

// --- meta mutations ---

func (s *Store) MetaOf(key string) model.Meta { return s.Meta[key] }

// setMeta persists the change and returns any write error so the UI can report
// it instead of silently claiming success.
func (s *Store) setMeta(key string, m model.Meta) error {
	if len(m.Tags) == 0 && m.Note == "" && !m.Pinned && !m.Archived {
		delete(s.Meta, key)
	} else {
		s.Meta[key] = m
	}
	return s.saveMeta()
}

func (s *Store) TogglePin(key string) error {
	m := s.Meta[key]
	m.Pinned = !m.Pinned
	return s.setMeta(key, m)
}

func (s *Store) ToggleArchive(key string) error {
	m := s.Meta[key]
	m.Archived = !m.Archived
	return s.setMeta(key, m)
}

func (s *Store) SetNote(key, note string) error {
	m := s.Meta[key]
	m.Note = strings.TrimSpace(note)
	return s.setMeta(key, m)
}

func (s *Store) AddTag(key, tag string) error {
	tag = strings.TrimSpace(tag)
	if tag == "" {
		return nil
	}
	m := s.Meta[key]
	for _, t := range m.Tags {
		if strings.EqualFold(t, tag) {
			return nil
		}
	}
	m.Tags = append(m.Tags, tag)
	return s.setMeta(key, m)
}

func (s *Store) RemoveTag(key, tag string) error {
	m := s.Meta[key]
	out := m.Tags[:0]
	for _, t := range m.Tags {
		if !strings.EqualFold(t, tag) {
			out = append(out, t)
		}
	}
	m.Tags = out
	return s.setMeta(key, m)
}

// --- program mutations ---

func (s *Store) ProgramByName(name string) *model.Program {
	for i := range s.Programs {
		if s.Programs[i].Name == name {
			return &s.Programs[i]
		}
	}
	return nil
}

func (s *Store) CreateProgram(name, desc string) error {
	name = strings.TrimSpace(name)
	if name == "" || s.ProgramByName(name) != nil {
		return nil
	}
	p := model.Program{Name: name, Description: desc}
	s.Programs = append(s.Programs, p)
	sort.Slice(s.Programs, func(i, j int) bool { return s.Programs[i].Name < s.Programs[j].Name })
	return s.saveProgram(p)
}

func (s *Store) AddToProgram(name, missionKey string) error {
	p := s.ProgramByName(name)
	if p == nil {
		return nil
	}
	for _, k := range p.Missions {
		if k == missionKey {
			return nil
		}
	}
	p.Missions = append(p.Missions, missionKey)
	return s.saveProgram(*p)
}

func (s *Store) RemoveFromProgram(name, missionKey string) error {
	p := s.ProgramByName(name)
	if p == nil {
		return nil
	}
	out := p.Missions[:0]
	for _, k := range p.Missions {
		if k != missionKey {
			out = append(out, k)
		}
	}
	p.Missions = out
	return s.saveProgram(*p)
}
