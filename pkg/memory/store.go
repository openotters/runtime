package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"charm.land/fantasy"
)

type Store struct {
	db *sql.DB
}

// flattenAssistantParts collapses a JSON-encoded parts array (the
// new shape for assistant rows) into a single text string for the
// LLM context. Tool blocks are skipped — we don't want the model to
// see "<tool_call>...<result>..." in its own previous response;
// the runtime will re-emit fresh tool calls if needed for the next
// turn. Best-effort: if content isn't JSON, returns it verbatim
// (back-compat hatch for any rows that escape the migration).
func flattenAssistantParts(content string) string {
	type part struct {
		Kind string `json:"kind"`
		Text string `json:"text"`
	}
	var parts []part
	if err := json.Unmarshal([]byte(content), &parts); err != nil {
		return content
	}
	var out []string
	for _, p := range parts {
		if p.Kind == "text" && p.Text != "" {
			out = append(out, p.Text)
		}
	}
	return strings.Join(out, "")
}

func NewStore(ctx context.Context, db *sql.DB) (*Store, error) {
	if err := migrate(ctx, db); err != nil {
		return nil, fmt.Errorf("running migrations: %w", err)
	}

	return &Store{db: db}, nil
}

func migrate(ctx context.Context, db *sql.DB) error {
	// Alpha policy: schema changed shape (rich parts + branches) so
	// any pre-existing rows are dropped on first boot. The previous
	// flat (role,content) table is no longer compatible with the
	// branches_json / active_branch columns the agent service now
	// reads/writes.
	_, err := db.ExecContext(ctx, `
		DROP TABLE IF EXISTS messages;
		CREATE TABLE messages (
			id            INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id    TEXT NOT NULL,
			role          TEXT NOT NULL,
			-- For user turns this is the prompt text verbatim.
			-- For assistant turns this is a JSON-encoded array of
			-- "parts" (text + tool blocks). When branches is
			-- non-empty, this is the active branch's parts.
			content       TEXT NOT NULL,
			-- Assistant-only side channel: JSON-encoded array of
			-- alternative parts arrays produced by Regenerate. The
			-- active_branch index points at whichever one (or this
			-- row's content) is current.
			branches_json TEXT NOT NULL DEFAULT '[]',
			active_branch INTEGER NOT NULL DEFAULT 0,
			created_at    TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		);
		CREATE INDEX idx_messages_session ON messages(session_id, created_at);
	`)

	return err
}

func (s *Store) GetMessages(ctx context.Context, sessionID string) ([]fantasy.Message, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT role, content FROM messages WHERE session_id = ? ORDER BY created_at ASC LIMIT 50",
		sessionID,
	)
	if err != nil {
		return nil, fmt.Errorf("querying messages: %w", err)
	}
	defer rows.Close()

	var messages []fantasy.Message

	for rows.Next() {
		var role, content string
		if err = rows.Scan(&role, &content); err != nil {
			return nil, fmt.Errorf("scanning message: %w", err)
		}

		// Assistant content is now JSON-encoded parts. Flatten to
		// concatenated text for the LLM context — the model only
		// needs to see what it said, not how the dashboard renders
		// the parts.
		if role == "assistant" {
			content = flattenAssistantParts(content)
		}

		messages = append(messages, fantasy.Message{
			Role:    fantasy.MessageRole(role),
			Content: []fantasy.MessagePart{fantasy.TextPart{Text: content}},
		})
	}

	return messages, rows.Err()
}

// StoredMessage is the timestamp-bearing shape used by
// ListMessages. Used by the runtime's gRPC ListSessionMessages
// path so the dashboard can render real wall-clock timestamps on
// hydrated history.
//
// Content shapes (alpha — no retro compat):
//   - user role: plain prompt text.
//   - assistant role: JSON-encoded array of "parts" representing
//     the active branch (text + tool blocks). BranchesJSON, when
//     non-empty, is a JSON-encoded array of alternative parts
//     arrays produced by Regenerate.
type StoredMessage struct {
	ID            int64
	Role          string
	Content       string
	BranchesJSON  string
	ActiveBranch  int
	CreatedAt     time.Time
}

// ListMessages returns the persisted messages for sessionID with
// their stored timestamps. Order ASC, capped at 50 rows like
// GetMessages.
func (s *Store) ListMessages(ctx context.Context, sessionID string) ([]StoredMessage, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, role, content, branches_json, active_branch, created_at
		   FROM messages
		  WHERE session_id = ?
		  ORDER BY created_at ASC, id ASC LIMIT 50`,
		sessionID,
	)
	if err != nil {
		return nil, fmt.Errorf("querying messages: %w", err)
	}
	defer rows.Close()

	var out []StoredMessage
	for rows.Next() {
		var m StoredMessage
		if err = rows.Scan(&m.ID, &m.Role, &m.Content, &m.BranchesJSON, &m.ActiveBranch, &m.CreatedAt); err != nil {
			return nil, fmt.Errorf("scanning message: %w", err)
		}
		out = append(out, m)
	}

	return out, rows.Err()
}

// LastAssistantMessage returns the most recent assistant row for
// sessionID, or sql.ErrNoRows if none exists. Used by the
// regenerate path to append a branch onto the prior turn instead
// of inserting a new row.
func (s *Store) LastAssistantMessage(ctx context.Context, sessionID string) (StoredMessage, error) {
	var m StoredMessage
	err := s.db.QueryRowContext(ctx,
		`SELECT id, role, content, branches_json, active_branch, created_at
		   FROM messages
		  WHERE session_id = ? AND role = 'assistant'
		  ORDER BY created_at DESC, id DESC LIMIT 1`,
		sessionID,
	).Scan(&m.ID, &m.Role, &m.Content, &m.BranchesJSON, &m.ActiveBranch, &m.CreatedAt)

	return m, err
}

// UpdateBranches rewrites a single message's content + branches +
// active index. Used by the regenerate path: the old content slides
// into branches, the new parts become the new content, and active
// flips to point at it.
func (s *Store) UpdateBranches(ctx context.Context, id int64, content, branchesJSON string, active int) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE messages SET content = ?, branches_json = ?, active_branch = ? WHERE id = ?`,
		content, branchesJSON, active, id,
	)

	return err
}

func (s *Store) SaveMessage(ctx context.Context, sessionID, role, content string) error {
	_, err := s.db.ExecContext(ctx,
		"INSERT INTO messages (session_id, role, content) VALUES (?, ?, ?)",
		sessionID, role, content,
	)

	return err
}

func (s *Store) ReplaceMessages(ctx context.Context, sessionID string, messages []fantasy.Message) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}

	defer tx.Rollback() //nolint:errcheck // rollback on commit is a no-op

	_, err = tx.ExecContext(ctx, "DELETE FROM messages WHERE session_id = ?", sessionID)
	if err != nil {
		return fmt.Errorf("deleting old messages: %w", err)
	}

	for _, m := range messages {
		text := messageText(m)
		if text == "" {
			continue
		}

		_, err = tx.ExecContext(ctx,
			"INSERT INTO messages (session_id, role, content) VALUES (?, ?, ?)",
			sessionID, string(m.Role), text,
		)
		if err != nil {
			return fmt.Errorf("inserting message: %w", err)
		}
	}

	return tx.Commit()
}

func (s *Store) CountMessages(ctx context.Context, sessionID string) (int, error) {
	var count int

	err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM messages WHERE session_id = ?", sessionID,
	).Scan(&count)

	return count, err
}

type SessionInfo struct {
	ID           string
	MessageCount int
	LastActive   int64
}

func (s *Store) ListSessions(ctx context.Context) ([]SessionInfo, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT session_id, COUNT(*) as msg_count, MAX(created_at) as last_active
		FROM messages
		GROUP BY session_id
		ORDER BY last_active DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("listing sessions: %w", err)
	}
	defer rows.Close()

	var sessions []SessionInfo

	for rows.Next() {
		var si SessionInfo
		var lastActive string

		if err = rows.Scan(&si.ID, &si.MessageCount, &lastActive); err != nil {
			return nil, fmt.Errorf("scanning session: %w", err)
		}

		if t, parseErr := time.Parse("2006-01-02 15:04:05", lastActive); parseErr == nil {
			si.LastActive = t.Unix()
		}

		sessions = append(sessions, si)
	}

	return sessions, rows.Err()
}

func (s *Store) DeleteSession(ctx context.Context, sessionID string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM messages WHERE session_id = ?", sessionID)
	return err
}

func messageText(m fantasy.Message) string {
	for _, part := range m.Content {
		if tp, ok := part.(fantasy.TextPart); ok {
			return tp.Text
		}
	}

	return ""
}
