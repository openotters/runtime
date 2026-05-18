package memory

import (
	"context"
	"fmt"
	"strings"

	"charm.land/fantasy"
	"go.uber.org/zap"
)

type Config struct {
	Strategy    string
	MaxMessages int
	MaxTokens   int
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
// the configured caps. Non-destructive: the raw history stays on
// disk for the UI's ListMessages view. The summarize strategy
// generates a one-paragraph recap and inserts it as a system row;
// the slide strategy just hides the older rows without a recap.
//
// Both paths flip visible_to_model = 0 on older rows. The next
// GetMessages call (model history load) returns only the recent
// window + the synthetic summary, but ListMessages still shows
// everything — the operator can scroll back through the full
// transcript with a "collapsed" hint on hidden rows.
func (c *Compactor) Compact(
	ctx context.Context, model fantasy.LanguageModel,
	store *Store, sessionID string,
) error {
	msgs, err := store.GetMessages(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("loading messages: %w", err)
	}

	if !c.shouldCompact(msgs) {
		return nil
	}

	c.logger.Info("compacting history",
		zap.String("strategy", c.strategy),
		zap.String("session", sessionID),
		zap.Int("messages", len(msgs)),
	)

	keep := c.maxMessages
	if c.strategy == "summarize" {
		// Summarize keeps the most recent half; the LLM-generated
		// summary stands in for the older half. Matches the
		// previous balance — half-context, half-recent.
		keep = c.maxMessages / 2
		if keep < 2 {
			keep = 2
		}
	}
	if keep < 2 {
		keep = 2
	}
	if keep > len(msgs) {
		keep = len(msgs)
	}

	switch c.strategy {
	case "summarize":
		summary, sumErr := c.generateSummary(ctx, model, msgs[:len(msgs)-keep])
		if sumErr != nil {
			c.logger.Warn("summarize failed, falling back to sliding", zap.Error(sumErr))
			if hideErr := store.HideOlder(ctx, sessionID, keep); hideErr != nil {
				return fmt.Errorf("hiding older messages: %w", hideErr)
			}
		} else if applyErr := store.HideAndAppendSummary(ctx, sessionID, summary); applyErr != nil {
			return fmt.Errorf("applying summary: %w", applyErr)
		}
	default:
		if hideErr := store.HideOlder(ctx, sessionID, keep); hideErr != nil {
			return fmt.Errorf("hiding older messages: %w", hideErr)
		}
	}

	c.logger.Info("history compacted",
		zap.String("session", sessionID),
		zap.Int("before_visible", len(msgs)),
		zap.Int("kept_recent", keep),
		zap.String("strategy", c.strategy),
	)

	return nil
}

func (c *Compactor) shouldCompact(msgs []fantasy.Message) bool {
	if len(msgs) > c.maxMessages {
		return true
	}

	if c.maxTokens > 0 && c.estimateTokens(msgs) > c.maxTokens {
		return true
	}

	return false
}

func (c *Compactor) estimateTokens(msgs []fantasy.Message) int {
	total := 0

	for _, m := range msgs {
		total += messageTextLen(m) / 4
	}

	return total
}

// generateSummary asks the LLM to condense the older messages into
// one paragraph. Returned as a plain string; the caller wraps it
// in the synthetic system row via store.HideAndAppendSummary.
func (c *Compactor) generateSummary(
	ctx context.Context, model fantasy.LanguageModel, oldMsgs []fantasy.Message,
) (string, error) {
	if len(oldMsgs) == 0 {
		return "", nil
	}

	var b strings.Builder
	for _, m := range oldMsgs {
		fmt.Fprintf(&b, "[%s]: %s\n", m.Role, messageText(m))
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
		zap.Int("old_messages", len(oldMsgs)),
		zap.Int("summary_len", len(summary)),
	)

	return "[Conversation summary]: " + summary, nil
}

func messageTextLen(m fantasy.Message) int {
	total := 0

	for _, part := range m.Content {
		if tp, ok := part.(fantasy.TextPart); ok {
			total += len(tp.Text)
		}
	}

	return total
}
