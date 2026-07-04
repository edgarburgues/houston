// Package export renders a mission's transcript to a readable Markdown file.
package export

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"houston/internal/model"
)

type rawEntry struct {
	Type    string `json:"type"`
	Message *struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
	Timestamp string `json:"timestamp"`
	IsMeta    bool   `json:"isMeta"`
}

type part struct {
	Type string `json:"type"`
	Text string `json:"text"`
	Name string `json:"name"`
}

// Mission writes the mission as Markdown to outPath and returns the path.
func Mission(m model.Mission, outPath string) (string, error) {
	in, err := os.Open(m.Path)
	if err != nil {
		return "", err
	}
	defer in.Close()

	if err := os.MkdirAll(filepath.Dir(outPath), 0o700); err != nil {
		return "", err
	}
	// 0600: transcripts routinely contain secrets (keys pasted into prompts,
	// tool output); don't hand them to every local user via default perms.
	out, err := os.OpenFile(outPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return "", err
	}
	defer out.Close()
	w := bufio.NewWriter(out)
	defer w.Flush()

	title := m.Title
	if title == "" {
		title = m.ID
	}
	fmt.Fprintf(w, "# %s\n\n", title)
	fmt.Fprintf(w, "- **Misión**: `%s`\n- **Proyecto**: %s\n- **cwd**: `%s`\n- **Rama**: %s\n",
		m.ID, m.Project, m.Cwd, m.GitBranch)
	if !m.FirstTime.IsZero() {
		fmt.Fprintf(w, "- **Periodo**: %s → %s\n",
			m.FirstTime.Local().Format("2006-01-02 15:04"),
			m.LastTime.Local().Format("2006-01-02 15:04"))
	}
	fmt.Fprintf(w, "- **Mensajes**: %d · **Tool calls**: %d\n\n---\n\n", m.MessageCount(), m.ToolCalls())

	r := bufio.NewReaderSize(in, 1<<20)
	for {
		line, rerr := r.ReadString('\n')
		if s := strings.TrimSpace(line); s != "" {
			writeTurn(w, s)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			break
		}
	}
	return outPath, nil
}

func writeTurn(w *bufio.Writer, line string) {
	var e rawEntry
	if json.Unmarshal([]byte(line), &e) != nil || e.Message == nil {
		return
	}
	txt := text(e.Message.Content)
	if strings.TrimSpace(txt) == "" {
		return
	}
	switch e.Type {
	case "user":
		if e.IsMeta {
			return
		}
		fmt.Fprintf(w, "### 🧑 Usuario\n\n%s\n\n", txt)
	case "assistant":
		fmt.Fprintf(w, "### 🤖 Claude\n\n%s\n\n", txt)
	}
}

func text(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	if strings.HasPrefix(strings.TrimSpace(string(raw)), "\"") {
		var s string
		_ = json.Unmarshal(raw, &s)
		return s
	}
	var parts []part
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var out []string
	for _, p := range parts {
		if p.Type == "text" && strings.TrimSpace(p.Text) != "" {
			out = append(out, p.Text)
		}
	}
	return strings.Join(out, "\n\n")
}
