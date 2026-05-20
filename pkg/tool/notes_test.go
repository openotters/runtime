package tool_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/openotters/runtime/pkg/notesclient"
	"github.com/openotters/runtime/pkg/tool"
)

// callTool runs a tool by name against the supplied input JSON,
// mirroring how fantasy invokes it at runtime. Returns the parsed
// ToolResponse so tests can assert IsError / Content.
func callTool(t *testing.T, tools []fantasy.AgentTool, name, input string) fantasy.ToolResponse {
	t.Helper()
	for _, tl := range tools {
		if tl.Info().Name == name {
			r, ok := tl.(interface {
				Run(ctx context.Context, params fantasy.ToolCall) (fantasy.ToolResponse, error)
			})
			if !ok {
				t.Fatalf("tool %q does not expose Run", name)
			}
			resp, err := r.Run(context.Background(), fantasy.ToolCall{Input: input})
			if err != nil {
				t.Fatalf("tool %q errored: %v", name, err)
			}
			return resp
		}
	}
	t.Fatalf("tool %q not registered", name)
	return fantasy.ToolResponse{}
}

func openNotesStore(t *testing.T) *notesclient.Fake {
	t.Helper()
	return notesclient.NewFake()
}

func TestBuildNotesTools_Set(t *testing.T) {
	t.Parallel()
	store := openNotesStore(t)
	tools := tool.BuildNotesTools(store, 4096, 64)

	// The runtime + capability list depend on this exact set —
	// each tool name is recorded in pool.go's runtimeCapsForExtras.
	want := []string{"note_save", "note_list", "note_show", "note_delete", "note_pin", "note_unpin"}
	got := make([]string, len(tools))
	for i, tl := range tools {
		got[i] = tl.Info().Name
	}
	if len(got) != len(want) {
		t.Fatalf("tool count = %d, want %d (%v)", len(got), len(want), got)
	}
	for i, n := range want {
		if got[i] != n {
			t.Errorf("tools[%d] = %q, want %q", i, got[i], n)
		}
	}
}

func TestNotePin_Existing(t *testing.T) {
	t.Parallel()
	store := openNotesStore(t)
	tools := tool.BuildNotesTools(store, 4096, 64)

	_ = callTool(t, tools, "note_save", `{"key":"k","content":"v"}`)
	resp := callTool(t, tools, "note_pin", `{"key":"k"}`)
	if resp.IsError {
		t.Fatalf("unexpected IsError: %s", resp.Content)
	}
	if !strings.Contains(resp.Content, "pinned") {
		t.Errorf("missing 'pinned' marker: %q", resp.Content)
	}
}

func TestNotePin_MissingKeyListsExisting(t *testing.T) {
	t.Parallel()
	store := openNotesStore(t)
	tools := tool.BuildNotesTools(store, 4096, 64)

	_ = callTool(t, tools, "note_save", `{"key":"a","content":"x"}`)
	resp := callTool(t, tools, "note_pin", `{"key":"nope"}`)
	if !resp.IsError {
		t.Fatalf("missing key should be IsError")
	}
	for _, needle := range []string{"a", "nope"} {
		if !strings.Contains(resp.Content, needle) {
			t.Errorf("hint missing %q: %q", needle, resp.Content)
		}
	}
}

func TestNoteUnpin_Existing(t *testing.T) {
	t.Parallel()
	store := openNotesStore(t)
	tools := tool.BuildNotesTools(store, 4096, 64)

	_ = callTool(t, tools, "note_save", `{"key":"k","content":"v"}`)
	_ = callTool(t, tools, "note_pin", `{"key":"k"}`)
	resp := callTool(t, tools, "note_unpin", `{"key":"k"}`)
	if resp.IsError {
		t.Fatalf("unexpected IsError: %s", resp.Content)
	}
	if !strings.Contains(resp.Content, "unpinned") {
		t.Errorf("missing 'unpinned' marker: %q", resp.Content)
	}
}

func TestNoteSave_NewKey(t *testing.T) {
	t.Parallel()
	store := openNotesStore(t)
	tools := tool.BuildNotesTools(store, 4096, 64)

	resp := callTool(t, tools, "note_save",
		`{"key":"k8s-cluster","content":"homelab"}`)
	if resp.IsError {
		t.Fatalf("unexpected IsError: %s", resp.Content)
	}
	if !strings.Contains(resp.Content, "new key") {
		t.Errorf("response missing 'new key' marker: %q", resp.Content)
	}
}

func TestNoteSave_Overwrite(t *testing.T) {
	t.Parallel()
	store := openNotesStore(t)
	tools := tool.BuildNotesTools(store, 4096, 64)

	_ = callTool(t, tools, "note_save", `{"key":"k","content":"first"}`)
	resp := callTool(t, tools, "note_save", `{"key":"k","content":"second"}`)

	if resp.IsError {
		t.Fatalf("unexpected IsError: %s", resp.Content)
	}
	if !strings.Contains(resp.Content, "overwrote") {
		t.Errorf("response missing 'overwrote' marker: %q", resp.Content)
	}
}

func TestNoteSave_InvalidKey_IsError(t *testing.T) {
	t.Parallel()
	store := openNotesStore(t)
	tools := tool.BuildNotesTools(store, 4096, 64)

	resp := callTool(t, tools, "note_save",
		`{"key":"BadKey!","content":"x"}`)
	if !resp.IsError {
		t.Errorf("invalid key should be IsError; got: %q", resp.Content)
	}
}

func TestNoteSave_OverSizeEchoesCap(t *testing.T) {
	t.Parallel()
	store := openNotesStore(t)
	tools := tool.BuildNotesTools(store, 10, 64)

	resp := callTool(t, tools, "note_save",
		`{"key":"big","content":"this is way more than ten bytes"}`)
	if !resp.IsError {
		t.Fatalf("oversize should be IsError")
	}
	// Cap is in the error message so the model can self-correct.
	if !strings.Contains(resp.Content, "10") {
		t.Errorf("cap not surfaced in error: %q", resp.Content)
	}
}

func TestNoteShow_MissingKeyListsExisting(t *testing.T) {
	t.Parallel()
	store := openNotesStore(t)
	tools := tool.BuildNotesTools(store, 4096, 64)

	_ = callTool(t, tools, "note_save", `{"key":"a","content":"x"}`)
	_ = callTool(t, tools, "note_save", `{"key":"b","content":"x"}`)

	resp := callTool(t, tools, "note_show", `{"key":"nope"}`)
	if !resp.IsError {
		t.Fatalf("missing key should be IsError")
	}
	for _, needle := range []string{"a", "b", "nope"} {
		if !strings.Contains(resp.Content, needle) {
			t.Errorf("hint missing %q: %q", needle, resp.Content)
		}
	}
}

func TestNoteShow_ReturnsFullContent(t *testing.T) {
	t.Parallel()
	store := openNotesStore(t)
	tools := tool.BuildNotesTools(store, 4096, 64)

	// JSON-encode the body via marshalling so embedded newlines are
	// escaped correctly — tool input arrives as JSON and the
	// unmarshal would reject a literal-newline string value.
	body := "line one\nline two\nline three"
	saveResp := callTool(t, tools, "note_save",
		mustJSON(t, map[string]string{"key": "multi", "content": body}))
	if saveResp.IsError {
		t.Fatalf("save unexpectedly errored: %s", saveResp.Content)
	}

	resp := callTool(t, tools, "note_show", `{"key":"multi"}`)
	if resp.IsError {
		t.Fatalf("unexpected IsError: %s", resp.Content)
	}
	if resp.Content != body {
		t.Errorf("content mismatch:\n got  %q\n want %q", resp.Content, body)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return string(data)
}

func TestNoteList_Empty(t *testing.T) {
	t.Parallel()
	store := openNotesStore(t)
	tools := tool.BuildNotesTools(store, 4096, 64)

	resp := callTool(t, tools, "note_list", `{}`)
	if resp.IsError {
		t.Fatalf("unexpected IsError: %s", resp.Content)
	}
	if !strings.Contains(resp.Content, "No notes stored") {
		t.Errorf("empty-store message missing: %q", resp.Content)
	}
}

func TestNoteList_PopulatedRendersAsTable(t *testing.T) {
	t.Parallel()
	store := openNotesStore(t)
	tools := tool.BuildNotesTools(store, 4096, 64)

	_ = callTool(t, tools, "note_save", `{"key":"a","content":"alpha"}`)
	_ = callTool(t, tools, "note_save", `{"key":"b","content":"beta"}`)

	resp := callTool(t, tools, "note_list", `{}`)
	if resp.IsError {
		t.Fatalf("unexpected IsError: %s", resp.Content)
	}
	for _, needle := range []string{"## Notes", "| Key | Updated | Preview |", "`a`", "`b`", "alpha", "beta"} {
		if !strings.Contains(resp.Content, needle) {
			t.Errorf("note_list missing %q:\n%s", needle, resp.Content)
		}
	}
}

func TestNoteDelete_Existing(t *testing.T) {
	t.Parallel()
	store := openNotesStore(t)
	tools := tool.BuildNotesTools(store, 4096, 64)

	_ = callTool(t, tools, "note_save", `{"key":"gone","content":"x"}`)
	resp := callTool(t, tools, "note_delete", `{"key":"gone"}`)
	if resp.IsError {
		t.Fatalf("unexpected IsError: %s", resp.Content)
	}
	if !strings.Contains(resp.Content, `deleted "gone"`) {
		t.Errorf("missing deleted marker: %q", resp.Content)
	}
}

func TestNoteDelete_MissingIsOK(t *testing.T) {
	t.Parallel()
	store := openNotesStore(t)
	tools := tool.BuildNotesTools(store, 4096, 64)

	resp := callTool(t, tools, "note_delete", `{"key":"ghost"}`)
	if resp.IsError {
		t.Fatalf("unexpected IsError: %s", resp.Content)
	}
	if !strings.Contains(resp.Content, "already absent") {
		t.Errorf("missing 'already absent' marker: %q", resp.Content)
	}
}
