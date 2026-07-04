// Package pathenc deals with Claude's project-dir encoding. Claude stores a
// session at projects/<encoded-cwd>/<id>.jsonl, where the encoded cwd replaces
// EVERY non-alphanumeric character with '-' (verified against real stores:
// ':' '\' '/' '.' and ' ' all encode to '-'; a UNC path like
// \\10.66.77.20\Media\South Park becomes --10-66-77-20-Media-South-Park).
// `claude --resume` re-derives that key from the CURRENT shell cwd, so to
// resume we must cd to the path that encodes back to the session's project
// dir — which is the cwd at session *creation*, not the cwd of the last
// message.
package pathenc

import (
	"os"
	"path/filepath"
	"strings"
)

// Encode turns a real path into Claude's project-dir name: every rune outside
// [a-zA-Z0-9] becomes '-'.
func Encode(p string) string {
	var b strings.Builder
	for _, ch := range p {
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') {
			b.WriteRune(ch)
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// DecodeProjectDir reconstructs a real path from a project-dir name. The
// encoding is lossy — a '-' may stand for a real '-', '.', ' ' or any other
// punctuation — so at each level we list the parent directory and pick the
// child whose ENCODED name matches the longest prefix of the remaining tokens
// (this resolves both hyphen ambiguity like "08-copia" and punctuation like
// "my.project" or "South Park"). When nothing matches, a best-effort single
// token keeps us moving. Returns "" if it can't get started.
//
// Windows-only: it keys off the "--" that a drive root ("C:\" -> "C--") leaves
// in the encoded name. POSIX roots encode "/Users" -> "-Users" (no "--"), so
// this returns "" there and ResolveResumeDir falls back to the stored cwd.
func DecodeProjectDir(proj string) string {
	i := strings.Index(proj, "--")
	if i <= 0 { // no drive prefix (UNC "--..." or POSIX): can't reconstruct
		return ""
	}
	cur := proj[:i] + `:\`
	rest := proj[i+2:]
	for rest != "" {
		name, n := bestChild(cur, rest)
		if n == 0 {
			// Nothing on disk encodes to a prefix of rest (dir renamed/removed):
			// take one raw token and keep going, best effort.
			if j := strings.IndexByte(rest, '-'); j >= 0 {
				name, rest = rest[:j], rest[j+1:]
			} else {
				name, rest = rest, ""
			}
			cur = filepath.Join(cur, name)
			continue
		}
		cur = filepath.Join(cur, name)
		rest = strings.TrimPrefix(rest[n:], "-")
	}
	return cur
}

// bestChild returns the child directory of cur whose encoded name is the
// longest match for a prefix of rest (the full rest, or a prefix followed by
// the '-' separator), and how many bytes of rest its encoding consumes.
func bestChild(cur, rest string) (string, int) {
	entries, err := os.ReadDir(cur)
	if err != nil {
		return "", 0
	}
	best, bestLen := "", 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		enc := Encode(e.Name())
		if len(enc) <= bestLen || enc == "" {
			continue
		}
		if rest == enc || (strings.HasPrefix(rest, enc) && len(rest) > len(enc) && rest[len(enc)] == '-') {
			best, bestLen = e.Name(), len(enc)
		}
	}
	return best, bestLen
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
