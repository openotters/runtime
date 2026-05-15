// Package notes is a per-agent, cross-session key/value store the
// model writes via the note_* tools. Distinct from the chat-message
// store: notes are durable facts the user expects the agent to
// remember between sessions, not conversation history. Storage
// shares memory.db with the message store but lives in its own
// `notes` table.
package notes

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// Note is one stored fact. Preview is denormalised on Save so list
// rendering doesn't have to re-compute it across every row on every
// PrepareStep call. InContext, when true, marks the note for full
// inclusion in the agent's system prompt on every step — the model
// "pins" a fact it expects to reference frequently.
type Note struct {
	Key       string
	Content   string
	Preview   string
	InContext bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Sentinel errors. Wrapped with %w at every package boundary so
// callers can `errors.Is` to distinguish the user-fixable cases
// (oversize content, key collision past the cap) from genuine
// IO failures.
var (
	ErrInvalidKey   = errors.New("invalid note key")
	ErrNoteTooLarge = errors.New("note content exceeds size cap")
	ErrTooManyNotes = errors.New("note count would exceed cap")
)

// keyPattern restricts note keys to lowercase grep-friendly tokens
// so they render cleanly inside markdown tables and JSON tool
// responses without escaping concerns.
var keyPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// previewMaxRunes caps the denormalised preview at one screen-line
// of width. Long lines are truncated with a trailing ellipsis; the
// model can still call note_show for the full content.
const previewMaxRunes = 80

type Store struct {
	db *sql.DB
}

// NewStore opens (or migrates) the notes table on the supplied
// connection. Safe to call alongside memory.NewStore on the same db
// — the two stores use disjoint tables.
func NewStore(ctx context.Context, db *sql.DB) (*Store, error) {
	if err := migrate(ctx, db); err != nil {
		return nil, fmt.Errorf("running notes migrations: %w", err)
	}
	return &Store{db: db}, nil
}

// migrate is idempotent: every statement is safe to re-run on every
// boot. No version table, no destructive ALTERs.
func migrate(ctx context.Context, db *sql.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS notes (
			key        TEXT PRIMARY KEY,
			content    TEXT NOT NULL,
			preview    TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_notes_updated ON notes(updated_at DESC)`,
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			return fmt.Errorf("notes schema: %w", err)
		}
	}

	// in_context column added separately so existing alpha.19 rows
	// keep working and re-running this migration stays a no-op.
	if err := addColumnIfMissing(ctx, db, "notes", "in_context",
		"INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx,
		`CREATE INDEX IF NOT EXISTS idx_notes_in_context ON notes(in_context) WHERE in_context = 1`,
	); err != nil {
		return fmt.Errorf("idx_notes_in_context: %w", err)
	}

	return nil
}

// addColumnIfMissing is the column-add migration helper, duplicated
// from pkg/memory so pkg/notes doesn't pick up a reverse dependency.
// Both packages run their own migrations against the shared
// memory.db connection.
func addColumnIfMissing(ctx context.Context, db *sql.DB, table, column, decl string) error {
	rows, err := db.QueryContext(ctx, fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return fmt.Errorf("table_info %s: %w", table, err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			cid     int
			name    string
			typ     string
			notnull int
			dflt    sql.NullString
			pk      int
		)
		if scanErr := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); scanErr != nil {
			return fmt.Errorf("scan table_info: %w", scanErr)
		}
		if name == column {
			return nil
		}
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return fmt.Errorf("rows table_info: %w", rowsErr)
	}

	if _, execErr := db.ExecContext(ctx,
		fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, decl),
	); execErr != nil {
		return fmt.Errorf("adding %s.%s: %w", table, column, execErr)
	}
	return nil
}

// Save upserts a note. Returns ErrInvalidKey if the key fails the
// validation regex, ErrNoteTooLarge if len(content) > maxBytes, and
// ErrTooManyNotes if inserting a *new* key would exceed maxCount
// (updates to an existing key are unaffected by the count guard).
//
// Quotas live here, not in the tools layer, so there is one source
// of truth — anyone wiring a different surface (test harness, REPL,
// future operator RPC) gets the same enforcement for free.
func (s *Store) Save(ctx context.Context, key, content string, maxBytes, maxCount int) error {
	key = strings.TrimSpace(key)
	if !keyPattern.MatchString(key) {
		return fmt.Errorf("%w: %q (expected [a-z0-9][a-z0-9_-]{0,63})", ErrInvalidKey, key)
	}
	if maxBytes > 0 && len(content) > maxBytes {
		return fmt.Errorf("%w: %d > %d bytes", ErrNoteTooLarge, len(content), maxBytes)
	}

	exists, err := s.exists(ctx, key)
	if err != nil {
		return err
	}
	if !exists && maxCount > 0 {
		count, cErr := s.Count(ctx)
		if cErr != nil {
			return cErr
		}
		if count >= maxCount {
			return fmt.Errorf("%w: %d notes already stored (cap %d)", ErrTooManyNotes, count, maxCount)
		}
	}

	preview := derivePreview(content)
	now := time.Now().UTC()

	// ON CONFLICT keeps created_at stable on updates; only content,
	// preview, and updated_at move. SQLite returns no error on a
	// no-op conflict; the upsert always succeeds.
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO notes (key, content, preview, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET
		  content    = excluded.content,
		  preview    = excluded.preview,
		  updated_at = excluded.updated_at
	`, key, content, preview, now, now)
	if err != nil {
		return fmt.Errorf("upsert note %q: %w", key, err)
	}
	return nil
}

// Get returns one note by key. sql.ErrNoRows is returned untouched
// so callers can errors.Is(err, sql.ErrNoRows) to render a "no
// such key" hint.
func (s *Store) Get(ctx context.Context, key string) (Note, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT key, content, preview, in_context, created_at, updated_at FROM notes WHERE key = ?`,
		key,
	)
	var n Note
	if err := row.Scan(&n.Key, &n.Content, &n.Preview, &n.InContext, &n.CreatedAt, &n.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Note{}, err
		}
		return Note{}, fmt.Errorf("get note %q: %w", key, err)
	}
	return n, nil
}

// Delete removes a note by key. Missing keys are silently
// successful — the tool layer surfaces "deleted (or already
// absent)" to the model regardless.
func (s *Store) Delete(ctx context.Context, key string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM notes WHERE key = ?`, key); err != nil {
		return fmt.Errorf("delete note %q: %w", key, err)
	}
	return nil
}

// List returns every note ordered by most-recently-updated first.
// The model uses this ordering when deciding what's still relevant
// — touching a note acts as an implicit "this still matters".
func (s *Store) List(ctx context.Context) ([]Note, error) {
	return s.queryNotes(ctx, listAllSQL)
}

// ListInContext returns only the notes flagged in_context = 1. Used
// by the PrepareStep callback to render the per-step pinned block.
// Same row shape as List so the renderer can treat both uniformly.
func (s *Store) ListInContext(ctx context.Context) ([]Note, error) {
	return s.queryNotes(ctx, listInContextSQL)
}

const (
	listAllSQL = `SELECT key, content, preview, in_context, created_at, updated_at
		FROM notes ORDER BY updated_at DESC`
	listInContextSQL = `SELECT key, content, preview, in_context, created_at, updated_at
		FROM notes WHERE in_context = 1 ORDER BY updated_at DESC`
)

func (s *Store) queryNotes(ctx context.Context, query string) ([]Note, error) {
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("query notes: %w", err)
	}
	defer rows.Close()

	var out []Note
	for rows.Next() {
		var n Note
		if scanErr := rows.Scan(
			&n.Key, &n.Content, &n.Preview, &n.InContext, &n.CreatedAt, &n.UpdatedAt,
		); scanErr != nil {
			return nil, fmt.Errorf("scan note: %w", scanErr)
		}
		out = append(out, n)
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return nil, fmt.Errorf("rows notes: %w", rowsErr)
	}
	return out, nil
}

// SetInContext flips a note's in_context flag. Errors with
// sql.ErrNoRows if the key doesn't exist so the tool layer can
// surface a clean "no such note" hint.
func (s *Store) SetInContext(ctx context.Context, key string, inContext bool) error {
	flag := 0
	if inContext {
		flag = 1
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE notes SET in_context = ?, updated_at = ? WHERE key = ?`,
		flag, time.Now().UTC(), key,
	)
	if err != nil {
		return fmt.Errorf("set in_context for %q: %w", key, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// Count returns the total number of stored notes. Cheap — used by
// Save to gate new-key inserts against the maxCount quota.
func (s *Store) Count(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM notes`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count notes: %w", err)
	}
	return n, nil
}

func (s *Store) exists(ctx context.Context, key string) (bool, error) {
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM notes WHERE key = ?`, key).Scan(&n); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("exists note %q: %w", key, err)
	}
	return true, nil
}

// derivePreview produces the one-line, truncated preview the list
// renderings show alongside each key. Multi-line content collapses
// to the first non-empty line; long lines truncate at
// previewMaxRunes with a trailing ellipsis. Runs of whitespace
// (including tabs) collapse to a single space so the preview reads
// cleanly inside a markdown table cell.
func derivePreview(content string) string {
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		flat := collapseSpaces(trimmed)
		if utf8.RuneCountInString(flat) > previewMaxRunes {
			runes := []rune(flat)
			return string(runes[:previewMaxRunes-1]) + "…"
		}
		return flat
	}
	return ""
}

func collapseSpaces(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	inSpace := false
	for _, r := range s {
		if r == ' ' || r == '\t' {
			if !inSpace {
				b.WriteByte(' ')
				inSpace = true
			}
			continue
		}
		b.WriteRune(r)
		inSpace = false
	}
	return b.String()
}
