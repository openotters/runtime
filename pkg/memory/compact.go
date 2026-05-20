package memory

import (
	"context"
	"fmt"
	"strings"

	"charm.land/fantasy"
	"go.uber.org/zap"

	"github.com/openotters/runtime/pkg/memoryclient"
)

type Config struct {
	Strategy    string
	MaxMessages int
	MaxTokens   int
}

// CompactorStore is the read+rewrite surface Compact needs.
// *memoryclient.Client satisfies it; tests inject an in-memory
// fake.
type CompactorStore interface {
	ListMessages(ctx context.Context, sessionID string) ([]memoryclient.StoredMessage, error)
	ReplaceMessages(ctx context.Context, sessionID string, msgs []memoryclient.StoredMessage) error
}

type Compactor struct {
	strategy    string
	maxMessages int
	maxTokens   int
	logger      *zap.Logger
}

func NewCompactor(cfg Config, logger *zap.Logger) *Compactor {
	return &Compactor{
		strategy:    cfg.Strategy,
		maxMessages: cfg.MaxMessages,
		maxTokens:   cfg.MaxTokens,
		logger:      logger.Named("compactor"),
	}
}

// Compact narrows the LLM-facing message window when it grows past
// the configured caps. The daemon's agent_messages table has no
// "hidden but visible-to-UI" notion, so compaction is destructive
// in the new model: rows that drop out of the window are gone for
// the UI too. The summarize strategy injects a synthetic
// "[Conversation summary]: …" assistant row in place of the old
// window; the slide strategy just keeps the tail.
//
// Per the alpha plan that moved storage to the daemon, this trade
// is intentional — the dashboard ListMessages view now mirrors the
// model's actual context, never a "collapsed" superset.
func (c *Compactor) Compact(
	ctx context.Context, model fantasy.LanguageModel,
	store CompactorStore, sessionID string,
) error {
	rows, err := store.ListMessages(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("listing messages: %w", err)
	}

	// shouldCompact evaluates the message count + an approximate
	// token estimate. Use the raw row content for the estimate —
	// it's already what the daemon stores and the model will see
	// after re-expansion.
	if !c.shouldCompact(rows) {
		return nil
	}

	c.logger.Info("compacting history",
		zap.String("strategy", c.strategy),
		zap.String("session", sessionID),
		zap.Int("messages", len(rows)),
	)

	keep := c.maxMessages
	if c.strategy == "summarize" {
		keep = c.maxMessages / 2
		if keep < 2 {
			keep = 2
		}
	}
	if keep < 2 {
		keep = 2
	}
	if keep > len(rows) {
		keep = len(rows)
	}

	dropped := rows[:len(rows)-keep]
	kept := rows[len(rows)-keep:]

	if c.strategy == "summarize" {
		summary, sumErr := c.generateSummary(ctx, model, dropped)
		switch {
		case sumErr != nil:
			c.logger.Warn("summarize failed, falling back to sliding", zap.Error(sumErr))
		case summary != "":
			// Prepend the synthetic summary as a system row so the
			// model picks it up via GetMessages on the next turn.
			synthetic := memoryclient.StoredMessage{
				Role:    "system",
				Content: summary,
			}
			kept = append([]memoryclient.StoredMessage{synthetic}, kept...)
		}
	}

	if replaceErr := store.ReplaceMessages(ctx, sessionID, kept); replaceErr != nil {
		return fmt.Errorf("replacing messages: %w", replaceErr)
	}

	c.logger.Info("history compacted",
		zap.String("session", sessionID),
		zap.Int("before", len(rows)),
		zap.Int("kept", keep),
		zap.String("strategy", c.strategy),
	)

	return nil
}

func (c *Compactor) shouldCompact(rows []memoryclient.StoredMessage) bool {
	if len(rows) > c.maxMessages {
		return true
	}
	if c.maxTokens > 0 && c.estimateTokens(rows) > c.maxTokens {
		return true
	}
	return false
}

func (c *Compactor) estimateTokens(rows []memoryclient.StoredMessage) int {
	total := 0
	for _, r := range rows {
		total += len(r.Content) / 4
	}
	return total
}

// generateSummary asks the LLM to condense the dropped rows into
// one paragraph. The result is wrapped with a recognisable prefix
// so the operator (and any debug logging) can see it apart from
// real assistant turns.
func (c *Compactor) generateSummary(
	ctx context.Context, model fantasy.LanguageModel, dropped []memoryclient.StoredMessage,
) (string, error) {
	if len(dropped) == 0 {
		return "", nil
	}

	var b strings.Builder
	for _, r := range dropped {
		fmt.Fprintf(&b, "[%s]: %s\n", r.Role, r.Content)
	}

	instruction := "Summarize the following conversation concisely, " +
		"preserving key facts, decisions, and context. " +
		"Output only the summary, no preamble.\n\n"

	prompt := fantasy.Prompt{
		{Role: fantasy.MessageRoleUser, Content: []fantasy.MessagePart{
			fantasy.TextPart{Text: instruction + b.String()},
		}},
	}

	resp, err := model.Generate(ctx, fantasy.Call{Prompt: prompt})
	if err != nil {
		return "", fmt.Errorf("generating summary: %w", err)
	}

	summary := resp.Content.Text()

	c.logger.Info("history summarized",
		zap.Int("old_messages", len(dropped)),
		zap.Int("summary_len", len(summary)),
	)

	return "[Conversation summary]: " + summary, nil
}
