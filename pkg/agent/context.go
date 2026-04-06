package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func BuildSystemPrompt(workspaceDir string, files []string) (string, error) {
	var b strings.Builder

	for _, filename := range files {
		path := filepath.Join(workspaceDir, filename)

		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}

			return "", fmt.Errorf("reading %s: %w", filename, err)
		}

		content := strings.TrimSpace(string(data))
		if content == "" {
			continue
		}

		if b.Len() > 0 {
			b.WriteString("\n\n---\n\n")
		}

		fmt.Fprintf(&b, "## %s\n\n%s\n", filename, content)
	}

	return b.String(), nil
}
