package tool

import "strings"

// splitArgs parses a single user-provided command string into an argv
// slice using shell-like quoting: unquoted whitespace separates
// tokens, and single/double quotes group characters verbatim. The
// quote char itself is dropped; nested quotes are not supported.
//
// This is intentionally simple — sufficient for the LLM's tool-call
// shape (one free-form string that should land in argv). We used to
// do this inside each BIN binary via the wrap package; it now lives
// in the runtime so every tool can be a plain CLI.
func splitArgs(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}

	var (
		args    []string
		current strings.Builder
		inQuote bool
		quote   byte
	)

	for i := 0; i < len(s); i++ {
		c := s[i]

		switch {
		case inQuote:
			if c == quote {
				inQuote = false

				continue
			}

			current.WriteByte(c)

		case c == '"' || c == '\'':
			inQuote = true
			quote = c

		case c == ' ' || c == '\t':
			if current.Len() > 0 {
				args = append(args, current.String())
				current.Reset()
			}

		default:
			current.WriteByte(c)
		}
	}

	if current.Len() > 0 {
		args = append(args, current.String())
	}

	return args
}
