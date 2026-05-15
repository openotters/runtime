package notes

import (
	"fmt"
	"strings"
	"time"
)

// RenderPromptSection turns a list of notes into the `## Notes`
// block injected into the agent's system prompt by the PrepareStep
// callback. Empty input returns the empty string so the caller can
// skip the separator entirely.
//
// Output shape (markdown):
//
//	## Notes
//
//	| Key | Updated | Preview |
//	|-----|---------|---------|
//	| `k8s-cluster`   | 5m ago | homelab cluster, 3 nodes |
//	| `kubeconfig-path` | 2d ago | /kubeconfig.yaml         |
//
//	Notes persist across sessions. Use `note_show <key>` for full content;
//	`note_save` / `note_delete` to mutate.
//
// Cells are sanitised: pipes in the preview are escaped to keep
// the table well-formed (rare, but cheap).
func RenderPromptSection(notes []Note) string {
	if len(notes) == 0 {
		return ""
	}

	now := time.Now().UTC()

	var b strings.Builder
	b.WriteString("## Notes\n\n")
	b.WriteString("| Key | Updated | Preview |\n")
	b.WriteString("|-----|---------|---------|\n")
	for _, n := range notes {
		fmt.Fprintf(&b, "| `%s` | %s | %s |\n",
			n.Key,
			formatRelative(now, n.UpdatedAt),
			escapePipe(n.Preview),
		)
	}
	b.WriteString("\nNotes persist across sessions. Use `note_show <key>` for full content; ")
	b.WriteString("`note_save` / `note_delete` to mutate.\n")

	return b.String()
}

// formatRelative renders a human-readable "how long ago" string.
// Coarse on purpose: seconds aren't useful when notes are durable
// facts (the model cares "today" vs "last week", not "47s ago").
func formatRelative(now, then time.Time) string {
	if then.IsZero() {
		return "—"
	}
	d := now.Sub(then)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// escapePipe replaces literal pipe characters in a markdown table
// cell so the row doesn't break. Newlines should already be gone
// (derivePreview collapses to one line), but defensively replace
// them with a space just in case a caller persisted a raw value.
func escapePipe(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "|", `\|`)
	return s
}
