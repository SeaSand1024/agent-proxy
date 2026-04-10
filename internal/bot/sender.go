package bot

import (
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"
	"unicode/utf16"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

type Sender struct {
	api            *tgbotapi.BotAPI
	maxLen         int
	updateInterval time.Duration
}

func NewSender(api *tgbotapi.BotAPI, maxLen int, updateIntervalMs int) *Sender {
	return &Sender{
		api:            api,
		maxLen:         maxLen,
		updateInterval: time.Duration(updateIntervalMs) * time.Millisecond,
	}
}

// SendText sends a plain text message (no formatting).
func (s *Sender) SendText(chatID int64, text string) (int, error) {
	if text == "" {
		text = "(empty response)"
	}
	segments := s.splitMessage(text)
	var lastMsgID int
	for _, seg := range segments {
		msg := tgbotapi.NewMessage(chatID, seg)
		sent, err := s.api.Send(msg)
		if err != nil {
			log.Printf("send message failed: %v", err)
			return 0, fmt.Errorf("send message: %w", err)
		}
		lastMsgID = sent.MessageID
	}
	return lastMsgID, nil
}

// SendMarkdown sends a message with MarkdownV2 formatting.
// Falls back to plain text if Telegram rejects the formatting.
func (s *Sender) SendMarkdown(chatID int64, text string) (int, error) {
	if text == "" {
		text = "(empty response)"
	}
	escaped := escapeMarkdownV2(text)
	segments := s.splitMessage(escaped)
	var lastMsgID int
	for _, seg := range segments {
		msg := tgbotapi.NewMessage(chatID, seg)
		msg.ParseMode = tgbotapi.ModeMarkdownV2
		sent, err := s.api.Send(msg)
		if err != nil {
			// Fallback: send as plain text
			msg2 := tgbotapi.NewMessage(chatID, text)
			sent2, err2 := s.api.Send(msg2)
			if err2 != nil {
				return 0, fmt.Errorf("send message: %w", err2)
			}
			lastMsgID = sent2.MessageID
			continue
		}
		lastMsgID = sent.MessageID
	}
	return lastMsgID, nil
}

// SendStatus sends a short italic status message (e.g. tool progress).
func (s *Sender) SendStatus(chatID int64, text string) (int, error) {
	msg := tgbotapi.NewMessage(chatID, "_"+escapeMarkdownV2Simple(text)+"_")
	msg.ParseMode = tgbotapi.ModeMarkdownV2
	sent, err := s.api.Send(msg)
	if err != nil {
		// Fallback plain
		msg2 := tgbotapi.NewMessage(chatID, "⏳ "+text)
		sent2, err2 := s.api.Send(msg2)
		if err2 != nil {
			return 0, err2
		}
		return sent2.MessageID, nil
	}
	return sent.MessageID, nil
}

// EditStatus updates an existing status message.
func (s *Sender) EditStatus(chatID int64, msgID int, text string) {
	edit := tgbotapi.NewEditMessageText(chatID, msgID, "_"+escapeMarkdownV2Simple(text)+"_")
	edit.ParseMode = tgbotapi.ModeMarkdownV2
	_, err := s.api.Send(edit)
	if err != nil {
		// Fallback plain
		edit2 := tgbotapi.NewEditMessageText(chatID, msgID, "⏳ "+text)
		s.api.Send(edit2)
	}
}

// DeleteMessage removes a message (used to clean up status messages).
func (s *Sender) DeleteMessage(chatID int64, msgID int) {
	del := tgbotapi.NewDeleteMessage(chatID, msgID)
	_, _ = s.api.Request(del)
}

// SendInlineKeyboard sends a message with inline keyboard buttons.
func (s *Sender) SendInlineKeyboard(chatID int64, text string, rows [][]tgbotapi.InlineKeyboardButton) (int, error) {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(rows...)
	sent, err := s.api.Send(msg)
	if err != nil {
		return 0, fmt.Errorf("send inline keyboard: %w", err)
	}
	return sent.MessageID, nil
}

// AnswerCallbackQuery answers a callback query from an inline button.
func (s *Sender) AnswerCallbackQuery(callbackID string, text string) error {
	ans := tgbotapi.NewCallback(callbackID, text)
	_, err := s.api.Request(ans)
	return err
}

// EditInlineKeyboard replaces the inline keyboard of a message.
func (s *Sender) EditInlineKeyboard(chatID int64, msgID int, rows [][]tgbotapi.InlineKeyboardButton) error {
	edit := tgbotapi.NewEditMessageReplyMarkup(chatID, msgID, tgbotapi.NewInlineKeyboardMarkup(rows...))
	_, err := s.api.Request(edit)
	return err
}

func (s *Sender) SendTyping(chatID int64) {
	action := tgbotapi.NewChatAction(chatID, tgbotapi.ChatTyping)
	_, _ = s.api.Request(action)
}

// ---------------------------------------------------------------------------
// StreamSender — real-time streaming of Claude output to Telegram
// ---------------------------------------------------------------------------

type StreamSender struct {
	sender    *Sender
	chatID    int64
	msgID     int
	text      strings.Builder
	lastFlush time.Time
	dirty     bool
}

func (s *Sender) NewStreamSender(chatID int64) *StreamSender {
	return &StreamSender{
		sender: s,
		chatID: chatID,
	}
}

// Append adds text and flushes to Telegram if enough time has passed.
func (ss *StreamSender) Append(text string) error {
	ss.text.WriteString(text)
	ss.dirty = true

	if time.Since(ss.lastFlush) < ss.sender.updateInterval {
		return nil
	}
	return ss.Flush()
}

// Flush sends/edits the current buffer to Telegram with MarkdownV2 (fallback to plain).
func (ss *StreamSender) Flush() error {
	if !ss.dirty {
		return nil
	}
	content := ss.text.String()
	if content == "" {
		return nil
	}

	displayContent := content + " ▍"

	if ss.msgID == 0 {
		ss.msgID = ss.sendOrEdit(0, displayContent)
	} else {
		ss.sendOrEdit(ss.msgID, displayContent)
	}

	// Handle overflow: if content exceeds max message length
	segments := ss.sender.splitMessage(content)
	if len(segments) > 1 {
		// Finalize first message with clean content (no cursor)
		ss.sendOrEdit(ss.msgID, segments[0])

		// Send remaining segments as new messages
		for i := 1; i < len(segments); i++ {
			newID := ss.sendNew(segments[i])
			if newID != 0 {
				ss.msgID = newID
			}
		}
		// Reset builder to last segment only
		ss.text.Reset()
		ss.text.WriteString(segments[len(segments)-1])
	}

	ss.lastFlush = time.Now()
	ss.dirty = false
	return nil
}

// Finalize sends the final version without streaming cursor, using MarkdownV2.
func (ss *StreamSender) Finalize() error {
	content := ss.text.String()
	if content == "" {
		return nil
	}

	segments := ss.sender.splitMessage(content)
	if len(segments) == 0 {
		return nil
	}

	if ss.msgID == 0 {
		ss.msgID = ss.sendNew(segments[0])
	} else {
		ss.sendOrEdit(ss.msgID, segments[0])
	}

	for i := 1; i < len(segments); i++ {
		newID := ss.sendNew(segments[i])
		if newID != 0 {
			ss.msgID = newID
		}
	}
	return nil
}

// sendOrEdit tries MarkdownV2 first, falls back to plain text.
// For msgID==0 it creates a new message, otherwise edits.
func (ss *StreamSender) sendOrEdit(msgID int, text string) int {
	escaped := escapeMarkdownV2(text)

	if msgID == 0 {
		// Create new
		msg := tgbotapi.NewMessage(ss.chatID, escaped)
		msg.ParseMode = tgbotapi.ModeMarkdownV2
		sent, err := ss.sender.api.Send(msg)
		if err != nil {
			// Fallback plain
			msg2 := tgbotapi.NewMessage(ss.chatID, text)
			sent2, err2 := ss.sender.api.Send(msg2)
			if err2 != nil {
				log.Printf("stream: create failed: %v", err2)
				return 0
			}
			return sent2.MessageID
		}
		return sent.MessageID
	}

	// Edit existing
	edit := tgbotapi.NewEditMessageText(ss.chatID, msgID, escaped)
	edit.ParseMode = tgbotapi.ModeMarkdownV2
	_, err := ss.sender.api.Send(edit)
	if err != nil {
		if strings.Contains(err.Error(), "message is not modified") {
			return msgID
		}
		// Fallback plain
		edit2 := tgbotapi.NewEditMessageText(ss.chatID, msgID, text)
		_, err2 := ss.sender.api.Send(edit2)
		if err2 != nil && !strings.Contains(err2.Error(), "message is not modified") {
			log.Printf("stream: edit failed: %v", err2)
		}
	}
	return msgID
}

func (ss *StreamSender) sendNew(text string) int {
	escaped := escapeMarkdownV2(text)
	msg := tgbotapi.NewMessage(ss.chatID, escaped)
	msg.ParseMode = tgbotapi.ModeMarkdownV2
	sent, err := ss.sender.api.Send(msg)
	if err != nil {
		msg2 := tgbotapi.NewMessage(ss.chatID, text)
		sent2, err2 := ss.sender.api.Send(msg2)
		if err2 != nil {
			log.Printf("stream: send new failed: %v", err2)
			return 0
		}
		return sent2.MessageID
	}
	return sent.MessageID
}

// ---------------------------------------------------------------------------
// MarkdownV2 escaping
// ---------------------------------------------------------------------------

// escapeMarkdownV2 escapes text for Telegram MarkdownV2 while preserving
// code blocks (``` ... ```) and inline code (` ... `).
func escapeMarkdownV2(text string) string {
	// Split by code blocks first to avoid escaping inside them
	parts := splitCodeBlocks(text)
	var result strings.Builder
	for _, p := range parts {
		if p.isCode {
			// Code blocks: only escape backslash and backtick within the block
			result.WriteString(p.text)
		} else {
			// Normal text: escape special characters
			result.WriteString(escapeNonCodeText(p.text))
		}
	}
	return result.String()
}

// escapeMarkdownV2Simple escapes ALL special characters (for use in status messages
// where no Markdown formatting is desired inside the escaped region).
func escapeMarkdownV2Simple(s string) string {
	special := []string{"\\", "_", "*", "[", "]", "(", ")", "~", "`", ">", "#", "+", "-", "=", "|", "{", "}", ".", "!"}
	for _, ch := range special {
		s = strings.ReplaceAll(s, ch, "\\"+ch)
	}
	return s
}

type textPart struct {
	text   string
	isCode bool
}

var codeBlockRe = regexp.MustCompile("(?s)(```[\\s\\S]*?```)")

func splitCodeBlocks(text string) []textPart {
	matches := codeBlockRe.FindAllStringIndex(text, -1)
	if len(matches) == 0 {
		// Check for inline code
		return splitInlineCode(text)
	}

	var parts []textPart
	lastEnd := 0
	for _, m := range matches {
		if m[0] > lastEnd {
			parts = append(parts, splitInlineCode(text[lastEnd:m[0]])...)
		}
		parts = append(parts, textPart{text: text[m[0]:m[1]], isCode: true})
		lastEnd = m[1]
	}
	if lastEnd < len(text) {
		parts = append(parts, splitInlineCode(text[lastEnd:])...)
	}
	return parts
}

var inlineCodeRe = regexp.MustCompile("(`[^`\n]+`)")

func splitInlineCode(text string) []textPart {
	matches := inlineCodeRe.FindAllStringIndex(text, -1)
	if len(matches) == 0 {
		return []textPart{{text: text, isCode: false}}
	}

	var parts []textPart
	lastEnd := 0
	for _, m := range matches {
		if m[0] > lastEnd {
			parts = append(parts, textPart{text: text[lastEnd:m[0]], isCode: false})
		}
		parts = append(parts, textPart{text: text[m[0]:m[1]], isCode: true})
		lastEnd = m[1]
	}
	if lastEnd < len(text) {
		parts = append(parts, textPart{text: text[lastEnd:], isCode: false})
	}
	return parts
}

func escapeNonCodeText(s string) string {
	// Characters that must be escaped in MarkdownV2 outside code spans:
	// _ * [ ] ( ) ~ > # + - = | { } . !
	// We also escape \ itself first.
	s = strings.ReplaceAll(s, "\\", "\\\\")
	for _, ch := range []string{"_", "*", "[", "]", "(", ")", "~", ">", "#", "+", "-", "=", "|", "{", "}", ".", "!"} {
		s = strings.ReplaceAll(s, ch, "\\"+ch)
	}
	return s
}

// ---------------------------------------------------------------------------
// Message splitting
// ---------------------------------------------------------------------------

func (s *Sender) splitMessage(text string) []string {
	if utf16Len(text) <= s.maxLen {
		return []string{text}
	}

	var segments []string
	remaining := text
	for utf16Len(remaining) > s.maxLen {
		cutPoint := findCutPoint(remaining, s.maxLen)
		segments = append(segments, remaining[:cutPoint])
		remaining = remaining[cutPoint:]
	}
	if remaining != "" {
		segments = append(segments, remaining)
	}
	return segments
}

func utf16Len(s string) int {
	return len(utf16.Encode([]rune(s)))
}

func findCutPoint(s string, maxLen int) int {
	runes := []rune(s)
	count := 0
	for i, r := range runes {
		u16 := utf16.Encode([]rune{r})
		count += len(u16)
		if count >= maxLen {
			sub := string(runes[:i])
			if lastNL := strings.LastIndex(sub, "\n"); lastNL > len(sub)/2 {
				return lastNL + 1
			}
			return len(string(runes[:i]))
		}
	}
	return len(s)
}
