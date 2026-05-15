package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ContextFile pairs a context document's display name with the path
// to its source on disk. Name shows up as the markdown header inside
// the assembled system prompt; Path is resolved against the agent
// root by BuildSystemPrompt. Empty Name falls back to the basename
// of Path so the section still gets a readable header.
type ContextFile struct {
	Name string
	Path string
}

// BuildSystemPrompt reads each context file and concatenates them
// into one markdown blob separated by horizontal rules. Each section
// is prefixed with `## <Name>` so the model sees a stable document
// title (AGENT, WORKSPACE, SOUL, …) rather than an absolute on-disk
// path. Missing files are silently skipped (the daemon may declare
// optional context); files that are present but blank contribute
// nothing.
func BuildSystemPrompt(root string, files []ContextFile) (string, error) {
	var b strings.Builder

	for _, f := range files {
		path := filepath.Join(root, f.Path)

		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}

			return "", fmt.Errorf("reading %s: %w", f.Path, err)
		}

		content := strings.TrimSpace(string(data))
		if content == "" {
			continue
		}

		header := f.Name
		if header == "" {
			header = filepath.Base(f.Path)
		}

		if b.Len() > 0 {
			b.WriteString("\n\n---\n\n")
		}

		fmt.Fprintf(&b, "## %s\n\n%s\n", header, content)
	}

	return b.String(), nil
}
