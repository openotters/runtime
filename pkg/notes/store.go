package notes

import "time"

// Note mirrors one row from the daemon's agent_notes table.
// Content is only populated by GetNote / SaveNote responses;
// ListNotes returns rows with empty Content to keep payloads
// small. Preview is denormalised server-side (first non-empty
// line, capped at 80 runes).
//
// The runtime no longer owns the storage layer — pkg/notesclient
// is the wire client and pkg/notes only carries the in-memory
// shape + the markdown renderers used by the PrepareStep callback
// and the note_* tool descriptions.
type Note struct {
	Key       string
	Content   string
	Preview   string
	InContext bool
	CreatedAt time.Time
	UpdatedAt time.Time
}
