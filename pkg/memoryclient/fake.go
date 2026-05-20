package memoryclient

import (
	"context"
	"database/sql"
	"sort"
	"sync"
	"time"

	"charm.land/fantasy"
)

const roleAssistant = "assistant"

// Fake is an in-memory implementation of the MemoryStore /
// CompactorStore surfaces used by the runtime's agent service and
// compactor. Test-only — production code uses *Client over gRPC to
// the daemon.
type Fake struct {
	mu       sync.Mutex
	nextID   int64
	rows     map[string][]row // session_id → ordered rows
	clockNow func() time.Time
}

type row struct {
	id           int64
	role         string
	content      string
	branchesJSON string
	activeBranch int
	createdAt    time.Time
}

// NewFake returns a fresh in-memory store ready to use.
func NewFake() *Fake {
	return &Fake{
		rows:     map[string][]row{},
		clockNow: time.Now,
	}
}

func (f *Fake) now() time.Time {
	return f.clockNow()
}

func (f *Fake) GetMessages(_ context.Context, sessionID string) ([]fantasy.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	var out []fantasy.Message
	for _, r := range f.rows[sessionID] {
		if r.role == roleAssistant {
			out = append(out, expandAssistantParts(r.content)...)
			continue
		}
		out = append(out, fantasy.Message{
			Role:    fantasy.MessageRole(r.role),
			Content: []fantasy.MessagePart{fantasy.TextPart{Text: r.content}},
		})
	}
	return out, nil
}

func (f *Fake) ListMessages(_ context.Context, sessionID string) ([]StoredMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	rows := f.rows[sessionID]
	out := make([]StoredMessage, 0, len(rows))
	for _, r := range rows {
		out = append(out, StoredMessage{
			ID:           r.id,
			Role:         r.role,
			Content:      r.content,
			BranchesJSON: r.branchesJSON,
			ActiveBranch: r.activeBranch,
			CreatedAt:    r.createdAt,
		})
	}
	return out, nil
}

func (f *Fake) SaveMessage(_ context.Context, sessionID, role, content string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.nextID++
	f.rows[sessionID] = append(f.rows[sessionID], row{
		id:        f.nextID,
		role:      role,
		content:   content,
		createdAt: f.now(),
	})
	return f.nextID, nil
}

func (f *Fake) AppendAssistantStub(ctx context.Context, sessionID string) (int64, error) {
	return f.SaveMessage(ctx, sessionID, roleAssistant, "[]")
}

func (f *Fake) UpdateBranches(_ context.Context, id int64, content, branchesJSON string, active int) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	for sess, rows := range f.rows {
		for i := range rows {
			if rows[i].id == id {
				rows[i].content = content
				rows[i].branchesJSON = branchesJSON
				rows[i].activeBranch = active
				f.rows[sess] = rows
				return nil
			}
		}
	}
	return sql.ErrNoRows
}

func (f *Fake) LastAssistantMessage(_ context.Context, sessionID string) (StoredMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	rows := f.rows[sessionID]
	for i := len(rows) - 1; i >= 0; i-- {
		if rows[i].role == roleAssistant {
			r := rows[i]
			return StoredMessage{
				ID:           r.id,
				Role:         r.role,
				Content:      r.content,
				BranchesJSON: r.branchesJSON,
				ActiveBranch: r.activeBranch,
				CreatedAt:    r.createdAt,
			}, nil
		}
	}
	return StoredMessage{}, sql.ErrNoRows
}

func (f *Fake) ReplaceMessages(_ context.Context, sessionID string, msgs []StoredMessage) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	now := f.now()
	rows := make([]row, 0, len(msgs))
	for _, m := range msgs {
		f.nextID++
		rows = append(rows, row{
			id:           f.nextID,
			role:         m.Role,
			content:      m.Content,
			branchesJSON: m.BranchesJSON,
			activeBranch: m.ActiveBranch,
			createdAt:    now,
		})
	}
	f.rows[sessionID] = rows
	return nil
}

func (f *Fake) ListSessions(_ context.Context) ([]SessionInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([]SessionInfo, 0, len(f.rows))
	for id, rows := range f.rows {
		if len(rows) == 0 {
			continue
		}
		last := rows[len(rows)-1].createdAt
		out = append(out, SessionInfo{
			ID:           id,
			MessageCount: len(rows),
			LastActive:   last,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].LastActive.After(out[j].LastActive)
	})
	return out, nil
}

func (f *Fake) DeleteSession(_ context.Context, sessionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.rows, sessionID)
	return nil
}

// CountMessages mirrors the test helper that used to live on
// memory.Store. Counts rows for sessionID regardless of role.
func (f *Fake) CountMessages(_ context.Context, sessionID string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.rows[sessionID]), nil
}
