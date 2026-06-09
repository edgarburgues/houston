// Package model defines Houston's core types: a Mission (one Claude
// conversation) and a Program (a logical grouping of missions, the slnx
// analogue). Missions are read-only projections of the .jsonl transcripts;
// the mutable bits (tags, notes, programs) live in the Store.
package model

import "time"

// Mission is the immutable metadata extracted from one .jsonl transcript.
type Mission struct {
	ID            string         `json:"id"`        // sessionId (== file base name)
	Path          string         `json:"path"`      // absolute path to the .jsonl
	Project       string         `json:"project"`   // encoded project dir name
	Cwd           string         `json:"cwd"`       // dir to cd into for `claude --resume` (encodes to Project)
	LastCwd       string         `json:"lastCwd"`   // cwd of the last message (where work happened)
	Title         string         `json:"title"`     // aiTitle
	Slug          string         `json:"slug"`      // human slug (starry-tinkering-sky)
	GitBranch     string         `json:"gitBranch"`
	Version       string         `json:"version"`
	FirstPrompt   string         `json:"firstPrompt"`
	LastPrompt    string         `json:"lastPrompt"`
	FirstTime     time.Time      `json:"firstTime"`
	LastTime      time.Time      `json:"lastTime"`
	UserMsgs      int            `json:"userMsgs"`
	AssistantMsgs int            `json:"assistantMsgs"`
	Tools         map[string]int `json:"tools"`
	SizeBytes     int64          `json:"sizeBytes"`
	HasSubagents  bool           `json:"hasSubagents"`
	MTime         time.Time      `json:"mtime"`   // file mtime, for incremental rescans
	Search        string         `json:"search"`  // lowercased prose for in-memory search
}

// Key uniquely identifies a mission. A session-id alone is NOT unique: the same
// session resumed from different working dirs produces separate transcripts
// under different project dirs. So the key is project + id.
func (m Mission) Key() string { return m.Project + "/" + m.ID }

// MessageCount is the total of user + assistant turns.
func (m Mission) MessageCount() int { return m.UserMsgs + m.AssistantMsgs }

// ToolCalls is the total number of tool invocations across the mission.
func (m Mission) ToolCalls() int {
	n := 0
	for _, c := range m.Tools {
		n += c
	}
	return n
}

// Meta is the user-editable data Houston keeps per mission (never written
// back into the .jsonl).
type Meta struct {
	Tags     []string `json:"tags,omitempty"`
	Note     string   `json:"note,omitempty"`
	Pinned   bool     `json:"pinned,omitempty"`
	Archived bool     `json:"archived,omitempty"`
}

// Program is a logical grouping of missions (the ".prog" manifest).
type Program struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Missions    []string `json:"missions"` // ordered mission IDs
}
