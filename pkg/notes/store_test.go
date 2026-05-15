package notes_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/openotters/runtime/pkg/notes"
)

// openTestDB opens a fresh sqlite database in t.TempDir and runs
// notes.NewStore against it. Returns the store + a re-opener so a
// test can exercise migrate-twice idempotency without paying the
// fresh-tempdir cost.
func openTestDB(t *testing.T) (*notes.Store, *sql.DB) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "memory.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	store, err := notes.NewStore(context.Background(), db)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return store, db
}

func TestStore_SaveAndGet_RoundTrip(t *testing.T) {
	t.Parallel()
	store, _ := openTestDB(t)
	ctx := context.Background()

	if err := store.Save(ctx, "k8s-cluster", "homelab cluster\nthree-node arm64 control plane", 4096, 64); err != nil {
		t.Fatalf("Save: %v", err)
	}

	n, err := store.Get(ctx, "k8s-cluster")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if n.Key != "k8s-cluster" {
		t.Errorf("Key = %q, want %q", n.Key, "k8s-cluster")
	}
	if !strings.Contains(n.Content, "three-node") {
		t.Errorf("content lost on round-trip: %q", n.Content)
	}
	// Preview should be the first non-empty line, collapsed.
	if n.Preview != "homelab cluster" {
		t.Errorf("Preview = %q, want %q", n.Preview, "homelab cluster")
	}
	if n.CreatedAt.IsZero() || n.UpdatedAt.IsZero() {
		t.Errorf("timestamps are zero: created=%v updated=%v", n.CreatedAt, n.UpdatedAt)
	}
}

func TestStore_Save_UpsertKeepsCreatedAtBumpsUpdated(t *testing.T) {
	t.Parallel()
	store, _ := openTestDB(t)
	ctx := context.Background()

	if err := store.Save(ctx, "k", "first", 4096, 64); err != nil {
		t.Fatalf("Save first: %v", err)
	}
	first, _ := store.Get(ctx, "k")

	// Sleep one second so timestamp comparison is robust against
	// clock granularity (sqlite CURRENT_TIMESTAMP is second-level).
	time.Sleep(1100 * time.Millisecond)

	if err := store.Save(ctx, "k", "second", 4096, 64); err != nil {
		t.Fatalf("Save second: %v", err)
	}
	second, _ := store.Get(ctx, "k")

	if !second.CreatedAt.Equal(first.CreatedAt) {
		t.Errorf("CreatedAt drifted on upsert: first=%v second=%v", first.CreatedAt, second.CreatedAt)
	}
	if !second.UpdatedAt.After(first.UpdatedAt) {
		t.Errorf("UpdatedAt did not advance: first=%v second=%v", first.UpdatedAt, second.UpdatedAt)
	}
	if second.Content != "second" {
		t.Errorf("Content not replaced: %q", second.Content)
	}
}

func TestStore_Save_InvalidKeyRejected(t *testing.T) {
	t.Parallel()
	store, _ := openTestDB(t)
	ctx := context.Background()

	bad := []string{
		"",                      // empty
		"K8S-Cluster",           // uppercase
		"with space",            // space
		"with/slash",            // slash
		"with.dot",              // dot
		strings.Repeat("a", 65), // 65 chars
		"-leading-dash",         // first char must be [a-z0-9]
	}
	for _, key := range bad {
		err := store.Save(ctx, key, "x", 4096, 64)
		if !errors.Is(err, notes.ErrInvalidKey) {
			t.Errorf("Save(%q) = %v, want ErrInvalidKey", key, err)
		}
	}
}

func TestStore_Save_OversizeRejected(t *testing.T) {
	t.Parallel()
	store, _ := openTestDB(t)
	ctx := context.Background()

	err := store.Save(ctx, "big", strings.Repeat("x", 100), 50, 64)
	if !errors.Is(err, notes.ErrNoteTooLarge) {
		t.Fatalf("Save oversize = %v, want ErrNoteTooLarge", err)
	}
}

func TestStore_Save_CountCapBlocksNewKeysOnly(t *testing.T) {
	t.Parallel()
	store, _ := openTestDB(t)
	ctx := context.Background()

	for i := range 3 {
		if err := store.Save(ctx, key(i), "v", 4096, 3); err != nil {
			t.Fatalf("Save %d: %v", i, err)
		}
	}

	// New key past cap → rejected.
	err := store.Save(ctx, "overflow", "v", 4096, 3)
	if !errors.Is(err, notes.ErrTooManyNotes) {
		t.Errorf("new key over cap = %v, want ErrTooManyNotes", err)
	}

	// Update to existing key at cap → still allowed.
	if upErr := store.Save(ctx, key(0), "updated", 4096, 3); upErr != nil {
		t.Errorf("update at cap rejected: %v", upErr)
	}
}

func TestStore_Get_MissingReturnsErrNoRows(t *testing.T) {
	t.Parallel()
	store, _ := openTestDB(t)
	ctx := context.Background()

	_, err := store.Get(ctx, "nope")
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("Get missing = %v, want sql.ErrNoRows", err)
	}
}

func TestStore_Delete_IdempotentOnMiss(t *testing.T) {
	t.Parallel()
	store, _ := openTestDB(t)
	ctx := context.Background()

	if err := store.Delete(ctx, "never-saved"); err != nil {
		t.Fatalf("Delete missing = %v, want nil", err)
	}
}

func TestStore_List_OrdersByUpdatedDesc(t *testing.T) {
	t.Parallel()
	store, _ := openTestDB(t)
	ctx := context.Background()

	for _, k := range []string{"a", "b", "c"} {
		if err := store.Save(ctx, k, "x", 4096, 64); err != nil {
			t.Fatalf("Save %s: %v", k, err)
		}
	}

	// Touch "a" so it should bubble to the front.
	time.Sleep(1100 * time.Millisecond)
	if err := store.Save(ctx, "a", "touched", 4096, 64); err != nil {
		t.Fatalf("Save touched: %v", err)
	}

	got, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("List len = %d, want 3", len(got))
	}
	if got[0].Key != "a" {
		t.Errorf("most-recent-first ordering broken: got[0] = %q", got[0].Key)
	}
}

func TestStore_Count(t *testing.T) {
	t.Parallel()
	store, _ := openTestDB(t)
	ctx := context.Background()

	for i := range 5 {
		if err := store.Save(ctx, key(i), "x", 4096, 64); err != nil {
			t.Fatalf("Save %d: %v", i, err)
		}
	}

	n, err := store.Count(ctx)
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if n != 5 {
		t.Errorf("Count = %d, want 5", n)
	}
}

func TestStore_Migrate_Idempotent(t *testing.T) {
	t.Parallel()
	_, db := openTestDB(t)
	ctx := context.Background()

	// Second NewStore on the same db must not fail; it re-runs
	// CREATE TABLE IF NOT EXISTS / CREATE INDEX IF NOT EXISTS.
	if _, err := notes.NewStore(ctx, db); err != nil {
		t.Fatalf("second NewStore: %v", err)
	}
}

func TestStore_SetInContext_TogglesFlag(t *testing.T) {
	t.Parallel()
	store, _ := openTestDB(t)
	ctx := context.Background()

	if err := store.Save(ctx, "k", "body", 4096, 64); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Freshly saved note is unpinned by default.
	n, _ := store.Get(ctx, "k")
	if n.InContext {
		t.Errorf("new note must default to InContext=false")
	}

	// Pin → InContext flips true.
	if err := store.SetInContext(ctx, "k", true); err != nil {
		t.Fatalf("SetInContext true: %v", err)
	}
	n, _ = store.Get(ctx, "k")
	if !n.InContext {
		t.Errorf("after pin, InContext must be true")
	}

	// Unpin → back to false.
	if err := store.SetInContext(ctx, "k", false); err != nil {
		t.Fatalf("SetInContext false: %v", err)
	}
	n, _ = store.Get(ctx, "k")
	if n.InContext {
		t.Errorf("after unpin, InContext must be false")
	}
}

func TestStore_SetInContext_MissingReturnsErrNoRows(t *testing.T) {
	t.Parallel()
	store, _ := openTestDB(t)
	ctx := context.Background()

	if err := store.SetInContext(ctx, "ghost", true); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("SetInContext on missing key = %v, want sql.ErrNoRows", err)
	}
}

func TestStore_ListInContext_OnlyPinned(t *testing.T) {
	t.Parallel()
	store, _ := openTestDB(t)
	ctx := context.Background()

	_ = store.Save(ctx, "a", "first", 4096, 64)
	_ = store.Save(ctx, "b", "second", 4096, 64)
	_ = store.Save(ctx, "c", "third", 4096, 64)
	_ = store.SetInContext(ctx, "b", true)

	pinned, err := store.ListInContext(ctx)
	if err != nil {
		t.Fatalf("ListInContext: %v", err)
	}
	if len(pinned) != 1 {
		t.Fatalf("ListInContext len = %d, want 1", len(pinned))
	}
	if pinned[0].Key != "b" {
		t.Errorf("ListInContext[0].Key = %q, want %q", pinned[0].Key, "b")
	}
}

func TestStore_Save_PreservesInContextOnUpsert(t *testing.T) {
	t.Parallel()
	store, _ := openTestDB(t)
	ctx := context.Background()

	// Save → pin → re-save (overwrite content). The pin must
	// survive: re-using note_save shouldn't unpin the note out
	// from under the model.
	if err := store.Save(ctx, "k", "v1", 4096, 64); err != nil {
		t.Fatalf("Save v1: %v", err)
	}
	if err := store.SetInContext(ctx, "k", true); err != nil {
		t.Fatalf("Pin: %v", err)
	}
	if err := store.Save(ctx, "k", "v2", 4096, 64); err != nil {
		t.Fatalf("Save v2: %v", err)
	}

	n, _ := store.Get(ctx, "k")
	if !n.InContext {
		t.Errorf("upsert dropped InContext flag")
	}
	if n.Content != "v2" {
		t.Errorf("content not replaced: %q", n.Content)
	}
}

func TestStore_Save_PreviewTruncatesLongLines(t *testing.T) {
	t.Parallel()
	store, _ := openTestDB(t)
	ctx := context.Background()

	long := strings.Repeat("x", 200)
	if err := store.Save(ctx, "long", long, 4096, 64); err != nil {
		t.Fatalf("Save: %v", err)
	}
	n, _ := store.Get(ctx, "long")
	if !strings.HasSuffix(n.Preview, "…") {
		t.Errorf("Preview should end with ellipsis when truncated: %q", n.Preview)
	}
	if len([]rune(n.Preview)) > 80 {
		t.Errorf("Preview length = %d runes, want ≤ 80: %q", len([]rune(n.Preview)), n.Preview)
	}
}

func key(i int) string {
	return "k" + string(rune('0'+i))
}
