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

	var compacted []fantasy.Message

	switch c.strategy {
	case "summarize":
		compacted, err = c.summarize(ctx, model, msgs)
		if err != nil {
			c.logger.Warn("summarize failed, falling back to sliding", zap.Error(err))
			compacted = c.slide(msgs)
		}
	default:
		compacted = c.slide(msgs)
	}

	if err = store.ReplaceMessages(ctx, sessionID, compacted); err != nil {
		return fmt.Errorf("replacing messages: %w", err)
	}

	c.logger.Info("history compacted",
		zap.String("session", sessionID),
		zap.Int("before", len(msgs)),
		zap.Int("after", len(compacted)),
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

func (c *Compactor) slide(msgs []fantasy.Message) []fantasy.Message {
	keep := c.maxMessages
	if keep < 2 {
		keep = 2
	}

	if len(msgs) <= keep {
		return msgs
	}

	sliced := msgs[len(msgs)-keep:]

	return appendNotice(sliced,
		fmt.Sprintf("[system]: Memory compacted (sliding). %d older messages were dropped. "+
			"If the user refers to something you don't recall, let them know your memory was compacted.", len(msgs)-keep),
	)
}

func (c *Compactor) summarize(
	ctx context.Context, model fantasy.LanguageModel, msgs []fantasy.Message,
) ([]fantasy.Message, error) {
	keep := c.maxMessages / 2
	if keep < 2 {
		keep = 2
	}

	if len(msgs) <= keep {
		return msgs, nil
	}

	oldMsgs := msgs[:len(msgs)-keep]
	recentMsgs := msgs[len(msgs)-keep:]

	var b strings.Builder

	for _, m := range oldMsgs {
		b.WriteString(fmt.Sprintf("[%s]: %s\n", m.Role, messageText(m)))
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
		return nil, fmt.Errorf("generating summary: %w", err)
	}

	summary := resp.Content.Text()

	c.logger.Info("history summarized",
		zap.Int("old_messages", len(oldMsgs)),
		zap.Int("kept_messages", len(recentMsgs)),
		zap.Int("summary_len", len(summary)),
	)

	compacted := make([]fantasy.Message, 0, 2+len(recentMsgs))
	compacted = append(compacted, fantasy.Message{
		Role: fantasy.MessageRoleAssistant,
		Content: []fantasy.MessagePart{
			fantasy.TextPart{Text: "[Conversation summary]: " + summary},
		},
	})
	compacted = append(compacted, recentMsgs...)

	return appendNotice(compacted,
		fmt.Sprintf("[system]: Memory compacted (summarized). %d older messages were condensed into a summary above. "+
			"Use the summary to maintain context.", len(oldMsgs)),
	), nil
}

func appendNotice(msgs []fantasy.Message, text string) []fantasy.Message {
	return append(msgs, fantasy.Message{
		Role: fantasy.MessageRoleUser,
		Content: []fantasy.MessagePart{
			fantasy.TextPart{Text: text},
		},
	})
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
