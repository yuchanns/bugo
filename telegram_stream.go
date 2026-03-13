package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/go-telegram/bot"
	log "github.com/yuchanns/bugo/internal/logging"
)

const (
	telegramDraftChunkLimit = 3500
	telegramDraftFlushRunes = 48
	telegramDraftCursor     = " ▌"
	telegramDraftRetries    = 2
)

type telegramDraftStreamer struct {
	bot      *bot.Bot
	chatID   int64
	threadID int
	draftID  string

	mu            sync.Mutex
	builder       strings.Builder
	sentRunes     int
	flushedRunes  int
	lastFlushed   string
	cooldownUntil time.Time
}

func newTelegramDraftStreamer(botClient *bot.Bot, chatID int64, threadID int) *telegramDraftStreamer {
	return &telegramDraftStreamer{
		bot:      botClient,
		chatID:   chatID,
		threadID: threadID,
		draftID:  fmt.Sprintf("%d", time.Now().UnixNano()),
	}
}

func (s *telegramDraftStreamer) Append(ctx context.Context, delta string) error {
	if delta == "" {
		return nil
	}

	s.mu.Lock()
	s.builder.WriteString(delta)
	s.mu.Unlock()

	if err := s.flushCompletedPrefix(ctx); err != nil {
		return err
	}

	s.mu.Lock()
	text := s.builder.String()
	textRunes := utf8RuneCount(text)
	pendingRunes := textRunes - s.flushedRunes
	coolingDown := !s.cooldownUntil.IsZero() && time.Now().Before(s.cooldownUntil)
	s.mu.Unlock()

	if coolingDown || pendingRunes < telegramDraftFlushRunes {
		return nil
	}
	return s.flushDraft(ctx, true)
}

func (s *telegramDraftStreamer) Finalize(ctx context.Context, finalText string) error {
	if strings.TrimSpace(finalText) == "" {
		s.mu.Lock()
		finalText = s.builder.String()
		s.mu.Unlock()
	} else {
		finalText = trimPrefixRunes(finalText, s.sentRunes)
	}
	if strings.TrimSpace(finalText) == "" {
		return nil
	}
	return s.sendFinal(ctx, finalText)
}

func (s *telegramDraftStreamer) Stop() {}

func (s *telegramDraftStreamer) flushCompletedPrefix(ctx context.Context) error {
	s.mu.Lock()
	text := s.builder.String()
	parts := splitTextByRunes(text, telegramDraftChunkLimit)
	if len(parts) <= 1 {
		s.mu.Unlock()
		return nil
	}
	ready := append([]string(nil), parts[:len(parts)-1]...)
	remain := parts[len(parts)-1]
	s.builder.Reset()
	s.builder.WriteString(remain)
	s.lastFlushed = ""
	s.flushedRunes = 0
	s.cooldownUntil = time.Time{}
	s.mu.Unlock()

	for _, part := range ready {
		if err := s.sendFinal(ctx, part); err != nil {
			return err
		}
	}
	return nil
}

func (s *telegramDraftStreamer) flushDraft(ctx context.Context, withCursor bool) error {
	s.mu.Lock()
	text := s.builder.String()
	if text == "" {
		s.mu.Unlock()
		return nil
	}
	display := text
	if withCursor {
		display += telegramDraftCursor
	}
	if display == s.lastFlushed {
		s.mu.Unlock()
		return nil
	}
	textRunes := utf8RuneCount(text)
	s.mu.Unlock()

	err := s.sendDraftWithRetry(ctx, display)
	if err != nil {
		log.Warn().
			Int64("chat_id", s.chatID).
			Int("thread_id", s.threadID).
			Err(err).
			Msg("telegram.draft.flush.failed")
		return nil
	}

	s.mu.Lock()
	s.lastFlushed = display
	s.flushedRunes = textRunes
	s.cooldownUntil = time.Time{}
	s.mu.Unlock()
	return nil
}

func (s *telegramDraftStreamer) sendFinal(ctx context.Context, text string) error {
	parts := splitTextByRunes(text, telegramDraftChunkLimit)
	for _, part := range parts {
		if _, err := s.bot.SendMessage(ctx, &bot.SendMessageParams{
			ChatID:          s.chatID,
			MessageThreadID: s.threadID,
			Text:            part,
		}); err != nil {
			return err
		}
	}

	s.mu.Lock()
	s.sentRunes += utf8RuneCount(text)
	s.lastFlushed = ""
	s.flushedRunes = 0
	s.cooldownUntil = time.Time{}
	s.mu.Unlock()
	return nil
}

func (s *telegramDraftStreamer) sendDraftWithRetry(ctx context.Context, text string) error {
	var lastErr error
	for attempt := 0; attempt <= telegramDraftRetries; attempt++ {
		_, err := s.bot.SendMessageDraft(ctx, &bot.SendMessageDraftParams{
			ChatID:          s.chatID,
			MessageThreadID: s.threadID,
			DraftID:         s.draftID,
			Text:            text,
		})
		if err == nil {
			return nil
		}
		lastErr = err

		var rateLimitErr *bot.TooManyRequestsError
		if !errors.As(err, &rateLimitErr) {
			return err
		}

		wait := time.Duration(max(rateLimitErr.RetryAfter, 1)) * time.Second
		s.mu.Lock()
		s.cooldownUntil = time.Now().Add(wait)
		s.mu.Unlock()

		if attempt == telegramDraftRetries {
			return err
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return lastErr
}

func utf8RuneCount(text string) int {
	return len([]rune(text))
}

func trimPrefixRunes(text string, n int) string {
	if n <= 0 {
		return text
	}
	runes := []rune(text)
	if n >= len(runes) {
		return ""
	}
	return string(runes[n:])
}
