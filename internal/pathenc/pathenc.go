// Package pathenc deals with Claude's project-dir encoding. Claude stores a
// session at projects/<encoded-cwd>/<id>.jsonl, where the encoded cwd replaces
// ':' '\' '/' with '-'. `claude --resume` re-derives that key from the CURRENT
// shell cwd, so to resume we must cd to the path that encodes back to the
// session's project dir — which is the cwd at session *creation*, not the cwd
// of the last message.
package pathenc

import (
	"os"
	"path/filepath"
	"strings"
)

// Encode turns a real path into Claude's project-dir name.
func Encode(p string) string {
	var b strings.Builder
	for _, ch := range p {
		if ch == ':' || ch == '\\' || ch == '/' {
			b.WriteByte('-')
		} else {
			b.WriteRune(ch)
		}
	}
	return b.String()
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// DecodeProjectDir reconstructs a real path from a project-dir name. Segment
// boundaries are ambiguous (a real folder may contain '-'), so we walk the
// filesystem greedily, preferring the longest token run that names an existing
// directory at each level. Returns "" if it can't get started.
func DecodeProjectDir(proj string) string {
	i := strings.Index(proj, "--")
	if i < 0 {
		return ""
	}
	cur := proj[:i] + ":\\"
	tokens := strings.Split(proj[i+2:], "-")
	for j := 0; j < len(tokens); {
		matched := 0
		for k := len(tokens) - j; k >= 1; k-- { // longest run first
			cand := strings.Join(tokens[j:j+k], "-")
			if isDir(filepath.Join(cur, cand)) {
				cur = filepath.Join(cur, cand)
				matched = k
				break
			}
		}
		if matched == 0 { // nothing exists here; best-effort single token
			cur = filepath.Join(cur, tokens[j])
			matched = 1
		}
		j += matched
	}
	return cur
}

// ResolveResumeDir picks the directory to cd into so `claude --resume` finds
// the session living under project dir `proj`.
func ResolveResumeDir(firstCwd, lastCwd, proj string) string {
	if firstCwd != "" && Encode(firstCwd) == proj {
		return firstCwd
	}
	if lastCwd != "" && Encode(lastCwd) == proj {
		return lastCwd
	}
	if d := DecodeProjectDir(proj); d != "" && isDir(d) {
		return d
	}
	if firstCwd != "" {
		return firstCwd
	}
	return lastCwd
}
