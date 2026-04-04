package memory

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"charm.land/fantasy"
)

type Store struct {
	db *sql.DB
}

func NewStore(db *sql.DB) (*Store, error) {
	if err := migrate(db); err != nil {
		return nil, fmt.Errorf("running migrations: %w", err)
	}

	return &Store{db: db}, nil
}

func migrate(db *sql.DB) error {
	_, err := db.ExecContext(context.Background(), `
		CREATE TABLE IF NOT EXISTS messages (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT NOT NULL,
			role TEXT NOT NULL,
			content TEXT NOT NULL,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		);
		CREATE INDEX IF NOT EXISTS idx_messages_session ON messages(session_id, created_at);
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

		messages = append(messages, fantasy.Message{
			Role:    fantasy.MessageRole(role),
			Content: []fantasy.MessagePart{fantasy.TextPart{Text: content}},
		})
	}

	return messages, rows.Err()
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
