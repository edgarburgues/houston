// Package theme defines Houston's TUI palette and pane layout plus the
// override shape shared by config.json and module manifests.
package theme

// Overrides is the sparse customization shape users (config.json) and modules
// (manifest "theme") provide: ANSI-256 color codes as strings plus optional
// pane widths. Invalid or unknown values are skipped field-wise at merge time,
// never rejected wholesale — a typo in one color must not discard the rest.
type Overrides struct {
	Colors map[string]string                                `json:"colors,omitempty"`
	Layout *struct{ LeftWidth, RightPercent, RightMin int } `json:"layout,omitempty"`
}
