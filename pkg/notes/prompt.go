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

// RenderInContextBlock renders the pinned-notes section that flows
// into the system prompt on every step. Distinct from
// RenderPromptSection (the table of all notes by preview) in that
// each pinned note appears as a full-content `## <key>` markdown
// section — the model gets the whole body, not just a one-line
// preview, because pinned notes are facts it's expected to lean on
// without re-fetching.
//
// Empty input → empty string so the caller can skip the separator.
func RenderInContextBlock(pinned []Note) string {
	if len(pinned) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("## Pinned notes\n\n")
	b.WriteString("These notes are integrated into your system prompt because they were ")
	b.WriteString("flagged in-context. Use `note_unpin <key>` to remove from this block ")
	b.WriteString("(the underlying note stays saved).\n")

	for _, n := range pinned {
		b.WriteString("\n### ")
		b.WriteString(n.Key)
		b.WriteString("\n\n")
		b.WriteString(strings.TrimSpace(n.Content))
		b.WriteString("\n")
	}
	return b.String()
}
