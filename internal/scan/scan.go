// Package scan walks a Claude projects directory (one root) and turns each
// .jsonl transcript into a model.Mission. It is read-only and streams files
// line by line so 30 MB transcripts never land in memory whole. ScanAll (in
// roots.go) fans this out across every discovered root and dedupes by real path.
package scan

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"houston/internal/model"
	"houston/internal/pathenc"
)

// maxSearchBytes caps the prose we keep per mission for in-memory search, so a
// huge transcript can't bloat the index.
const maxSearchBytes = 256 * 1024

// rawEntry is the subset of a transcript line Houston cares about.
type rawEntry struct {
	Type       string      `json:"type"`
	Subtype    string      `json:"subtype"`
	SessionID  string      `json:"sessionId"`
	Cwd        string      `json:"cwd"`
	GitBranch  string      `json:"gitBranch"`
	Version    string      `json:"version"`
	Slug       string      `json:"slug"`
	// User-set session name (claude -n / /rename), if the transcript records one.
	// The spelling has varied across Claude Code versions; accept both. A
	// top-level "name" is deliberately NOT read — it's ambiguous with tool/other
	// names. Any non-empty value here wins as the mission's display title.
	SessionName string `json:"sessionName"`
	CustomName  string `json:"customName"`
	Timestamp   string `json:"timestamp"`
	IsMeta     bool        `json:"isMeta"`
	AiTitle    string      `json:"aiTitle"`
	LastPrompt string      `json:"lastPrompt"`
	Message    *rawMessage `json:"message"`
}

type rawMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type contentPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
	Name string `json:"name"` // for tool_use
}

// Scan reads every mission .jsonl under root (excluding subagent transcripts)
// and returns the missions sorted by most-recent activity first. Uncached;
// ScanAll layers the scan cache on top.
func Scan(root string) ([]model.Mission, error) { return scanRoot(root, nil) }

// scanRoot walks one projects root, reusing cached parses for unchanged
// transcripts when c is non-nil.
func scanRoot(root string, c *Cache) ([]model.Mission, error) {
	var missions []model.Mission
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries, keep going
		}
		if d.IsDir() {
			if d.Name() == "subagents" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		fi, _ := d.Info()
		if m, ok := c.lookup(path, fi); ok {
			missions = append(missions, m)
			return nil
		}
		m, perr := parseFile(path)
		if perr == nil && m.ID != "" {
			c.put(path, fi, m)
			missions = append(missions, m)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(missions, func(i, j int) bool {
		return missions[i].LastTime.After(missions[j].LastTime)
	})
	return missions, nil
}

func parseFile(path string) (model.Mission, error) {
	f, err := os.Open(path)
	if err != nil {
		return model.Mission{}, err
	}
	defer f.Close()

	fi, _ := f.Stat()
	m := model.Mission{
		Path:    path,
		ID:      strings.TrimSuffix(filepath.Base(path), ".jsonl"),
		Project: filepath.Base(filepath.Dir(path)),
		Tools:   map[string]int{},
	}
	if fi != nil {
		m.SizeBytes = fi.Size()
		m.MTime = fi.ModTime()
	}
	// A "subagents" sibling dir means this mission spawned subagents.
	if st, err := os.Stat(filepath.Join(filepath.Dir(path), m.ID, "subagents")); err == nil && st.IsDir() {
		m.HasSubagents = true
	}

	var sb strings.Builder
	r := bufio.NewReaderSize(f, 1<<20)
	for {
		line, rerr := r.ReadString('\n')
		if s := strings.TrimSpace(line); s != "" {
			ingest(&m, &sb, s)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			break
		}
	}
	m.Search = strings.ToLower(sb.String())
	if m.Title == "" {
		m.Title = firstLine(m.FirstPrompt)
	}
	if m.Name != "" { // an explicit user-set name wins over aiTitle/first prompt
		m.Title = m.Name
	}
	// m.Cwd currently holds the creation cwd; resolve the dir that actually
	// encodes back to the project folder so `claude --resume` finds the session.
	m.Cwd = pathenc.ResolveResumeDir(m.Cwd, m.LastCwd, m.Project)
	return m, nil
}

func ingest(m *model.Mission, sb *strings.Builder, line string) {
	var e rawEntry
	if json.Unmarshal([]byte(line), &e) != nil {
		return
	}
	// Capture common fields wherever they appear (they're repeated on most lines).
	// Cwd: keep the FIRST seen as creation cwd (m.Cwd) and the latest as LastCwd.
	if e.Cwd != "" {
		if m.Cwd == "" {
			m.Cwd = e.Cwd
		}
		m.LastCwd = e.Cwd
	}
	if e.GitBranch != "" {
		m.GitBranch = e.GitBranch
	}
	if e.Version != "" {
		m.Version = e.Version
	}
	if e.Slug != "" {
		m.Slug = e.Slug
	}
	if e.SessionName != "" {
		m.Name = e.SessionName
	} else if e.CustomName != "" {
		m.Name = e.CustomName
	}
	switch e.Type {
	case "ai-title":
		if e.AiTitle != "" {
			m.Title = e.AiTitle
		}
	case "last-prompt":
		if e.LastPrompt != "" {
			m.LastPrompt = e.LastPrompt
		}
	case "user":
		txt := messageText(e.Message)
		if txt != "" && !e.IsMeta {
			m.UserMsgs++
			if m.FirstPrompt == "" {
				m.FirstPrompt = txt
			}
			appendSearch(sb, txt)
		}
		stampTime(m, e.Timestamp)
	case "assistant":
		m.AssistantMsgs++
		txt := messageText(e.Message)
		if txt != "" {
			appendSearch(sb, txt)
		}
		for _, name := range toolNames(e.Message) {
			m.Tools[name]++
		}
		stampTime(m, e.Timestamp)
	}
}

func appendSearch(sb *strings.Builder, txt string) {
	if sb.Len() >= maxSearchBytes {
		return
	}
	sb.WriteString(txt)
	sb.WriteByte('\n')
}

// messageText pulls human/assistant prose out of a message, ignoring
// tool_result/tool_use/image payloads (which are huge and not useful to search).
func messageText(msg *rawMessage) string {
	if msg == nil || len(msg.Content) == 0 {
		return ""
	}
	trimmed := strings.TrimSpace(string(msg.Content))
	if strings.HasPrefix(trimmed, "\"") {
		var s string
		if json.Unmarshal(msg.Content, &s) == nil {
			return s
		}
		return ""
	}
	var parts []contentPart
	if json.Unmarshal(msg.Content, &parts) != nil {
		return ""
	}
	var out []string
	for _, p := range parts {
		if p.Type == "text" && strings.TrimSpace(p.Text) != "" {
			out = append(out, p.Text)
		}
	}
	return strings.Join(out, "\n")
}

func toolNames(msg *rawMessage) []string {
	if msg == nil || len(msg.Content) == 0 {
		return nil
	}
	if strings.HasPrefix(strings.TrimSpace(string(msg.Content)), "\"") {
		return nil
	}
	var parts []contentPart
	if json.Unmarshal(msg.Content, &parts) != nil {
		return nil
	}
	var names []string
	for _, p := range parts {
		if p.Type == "tool_use" && p.Name != "" {
			names = append(names, p.Name)
		}
	}
	return names
}

func stampTime(m *model.Mission, ts string) {
	if ts == "" {
		return
	}
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return
	}
	if m.FirstTime.IsZero() || t.Before(m.FirstTime) {
		m.FirstTime = t
	}
	if t.After(m.LastTime) {
		m.LastTime = t
	}
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}
