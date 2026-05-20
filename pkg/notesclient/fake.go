package notesclient

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/openotters/runtime/pkg/notes"
)

// Fake is an in-memory implementation of the Store / NotesProvider
// surfaces used by the runtime's tool layer and PrepareStep
// callback. Test-only; production code uses *Client over gRPC.
type Fake struct {
	mu    sync.Mutex
	rows  map[string]fakeRow
	clock func() time.Time
}

type fakeRow struct {
	content   string
	preview   string
	inContext bool
	createdAt time.Time
	updatedAt time.Time
}

func NewFake() *Fake {
	return &Fake{rows: map[string]fakeRow{}, clock: time.Now}
}

var keyPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

func (f *Fake) now() time.Time { return f.clock() }

func (f *Fake) List(_ context.Context) ([]notes.Note, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([]notes.Note, 0, len(f.rows))
	for k, r := range f.rows {
		out = append(out, notes.Note{
			Key:       k,
			Preview:   r.preview,
			InContext: r.inContext,
			CreatedAt: r.createdAt,
			UpdatedAt: r.updatedAt,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].UpdatedAt.After(out[j].UpdatedAt)
	})
	return out, nil
}

func (f *Fake) ListInContext(_ context.Context) ([]notes.Note, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([]notes.Note, 0)
	for k, r := range f.rows {
		if !r.inContext {
			continue
		}
		out = append(out, notes.Note{
			Key:       k,
			Content:   r.content,
			Preview:   r.preview,
			InContext: r.inContext,
			CreatedAt: r.createdAt,
			UpdatedAt: r.updatedAt,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].UpdatedAt.After(out[j].UpdatedAt)
	})
	return out, nil
}

func (f *Fake) Get(_ context.Context, key string) (notes.Note, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	r, ok := f.rows[key]
	if !ok {
		return notes.Note{}, fmt.Errorf("%w: %q", ErrNoteNotFound, key)
	}
	return notes.Note{
		Key:       key,
		Content:   r.content,
		Preview:   r.preview,
		InContext: r.inContext,
		CreatedAt: r.createdAt,
		UpdatedAt: r.updatedAt,
	}, nil
}

func (f *Fake) Save(_ context.Context, key, content string, maxBytes, maxCount int) error {
	key = strings.TrimSpace(key)
	if !keyPattern.MatchString(key) {
		return fmt.Errorf("%w: %q", ErrInvalidKey, key)
	}
	if maxBytes > 0 && len(content) > maxBytes {
		return fmt.Errorf("%w: %d > %d bytes", ErrNoteTooLarge, len(content), maxBytes)
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if _, exists := f.rows[key]; !exists && maxCount > 0 && len(f.rows) >= maxCount {
		return fmt.Errorf("%w: %d notes already stored (cap %d)", ErrTooManyNotes, len(f.rows), maxCount)
	}

	prev := f.rows[key]
	createdAt := prev.createdAt
	if createdAt.IsZero() {
		createdAt = f.now()
	}
	f.rows[key] = fakeRow{
		content:   content,
		preview:   derivePreview(content),
		inContext: prev.inContext,
		createdAt: createdAt,
		updatedAt: f.now(),
	}
	return nil
}

func (f *Fake) Delete(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.rows, key)
	return nil
}

func (f *Fake) SetInContext(_ context.Context, key string, inContext bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	r, ok := f.rows[key]
	if !ok {
		return fmt.Errorf("%w: %q", ErrNoteNotFound, key)
	}
	r.inContext = inContext
	r.updatedAt = f.now()
	f.rows[key] = r
	return nil
}

// derivePreview is a stripped-down copy of the legacy
// pkg/notes.derivePreview — first non-empty line, capped at 80
// runes.
func derivePreview(content string) string {
	const maxRunes = 80
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		runes := []rune(line)
		if len(runes) > maxRunes {
			return string(runes[:maxRunes-1]) + "…"
		}
		return line
	}
	return ""
}
