package bot

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/agent-proxy/internal/claude"
	"github.com/agent-proxy/internal/middleware"
	"github.com/agent-proxy/internal/session"
)

type Handler struct {
	sender   *Sender
	executor *claude.Executor
	sessions *session.Manager
	auth     *middleware.Auth
}

func NewHandler(sender *Sender, executor *claude.Executor, sessions *session.Manager, auth *middleware.Auth) *Handler {
	return &Handler{
		sender:   sender,
		executor: executor,
		sessions: sessions,
		auth:     auth,
	}
}

// Claude Code slash commands forwarded directly to the persistent process.
var claudeSlashCommands = map[string]bool{
	// Session commands
	"compact": true, "context": true, "cost": true, "model": true, "memory": true,
	// Skills
	"init": true, "review": true, "security-review": true, "securityreview": true,
	"insights": true, "simplify": true, "debug": true, "prd": true,
	"batch": true, "loop": true,
	"update-config": true, "updateconfig": true, "update_config": true,
	"claude-api": true, "claudeapi": true, "claude_api": true,
	"tencent-cloud": true, "tencentcloud": true, "tencent_cloud": true,
	"longbridge": true,
	"tencent-tat-ops": true, "tencenttatops": true, "tencent_tat_ops": true,
	"ralph": true, "heapdump": true,
	"ralph-loop:help": true, "ralph-loop:cancel-ralph": true, "ralph-loop:ralph-loop": true,
}

// switchIntentPrefixes matches natural language phrases indicating session switch.
var switchIntentPrefixes = []string{
	"切到", "切换到", "用那个", "继续那个", "回到", "换成", "想用",
	"switch to", "resume that", "continue that", "use that",
}

// trailingFillers are trailing words to strip from the query.
var trailingFillers = []string{"那个", "的", "会话", "session", "的会话"}

// detectSwitchIntent checks if text expresses intent to switch session.
// Returns the query to search for and true if switch intent detected.
func detectSwitchIntent(text string) (query string, isSwitch bool) {
	text = strings.TrimSpace(text)
	for _, prefix := range switchIntentPrefixes {
		idx := strings.Index(text, prefix)
		if idx >= 0 {
			query = text[idx+len(prefix):]
			// Strip trailing fillers
			for {
				changed := false
				for _, f := range trailingFillers {
					if strings.HasSuffix(query, f) {
						query = strings.TrimSuffix(query, f)
						changed = true
					}
				}
				if !changed {
					break
				}
			}
			query = strings.TrimSpace(query)
			if query != "" {
				return query, true
			}
		}
	}
	return "", false
}

// fuzzyMatchSession matches query against session entries.
// Returns best match, all matches with score > 0.3.
func fuzzyMatchSession(query string, sessions []claude.SessionEntry) (best *claude.SessionEntry, matches []*claude.SessionEntry) {
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return nil, nil
	}

	var scored []struct {
		entry *claude.SessionEntry
		score float64
	}

	for i := range sessions {
		score := computeMatchScore(query, &sessions[i])
		if score > 0.3 {
			scored = append(scored, struct {
				entry *claude.SessionEntry
				score float64
			}{&sessions[i], score})
		}
	}

	sort.Slice(scored, func(i, j int) bool {
		return scored[i].score > scored[j].score
	})

	if len(scored) > 0 {
		best = scored[0].entry
		for i := range scored {
			matches = append(matches, scored[i].entry)
		}
	}
	return best, matches
}

// computeMatchScore calculates how well query matches a session.
// Uses substring matching, word boundary hits, and prefix matching.
func computeMatchScore(query string, entry *claude.SessionEntry) float64 {
	summary := strings.ToLower(entry.Summary)
	id := strings.ToLower(entry.ID)

	// Exact substring (highest weight)
	if strings.Contains(summary, query) || strings.Contains(id, query) {
		return 0.9
	}

	// Query words must all appear in summary
	words := strings.Fields(query)
	if len(words) > 1 {
		allFound := true
		for _, w := range words {
			if !strings.Contains(summary, w) && !strings.Contains(id, w) {
				allFound = false
				break
			}
		}
		if allFound {
			return 0.8
		}
	}

	// Prefix match on words
	queryWords := strings.Fields(query)
	matchCount := 0
	for _, qw := range queryWords {
		if strings.HasPrefix(summary, qw) || strings.HasPrefix(id, qw) {
			matchCount++
		} else if strings.Contains(summary, qw) || strings.Contains(id, qw) {
			matchCount++
		}
	}
	if matchCount > 0 {
		return 0.3 + 0.3*float64(matchCount)/float64(len(queryWords))
	}

	// Check if query is a prefix of summary or ID
	if strings.HasPrefix(summary, query) || strings.HasPrefix(id, query) {
		return 0.7
	}

	return 0
}

// handleSwitchSession handles natural language session switching.
func (h *Handler) handleSwitchSession(ctx context.Context, chatID int64, userID int64, query string) {
	sess := h.sessions.Get(userID)
	sessions, err := h.executor.ListSessions(ctx, sess.WorkDir)
	if err != nil || len(sessions) == 0 {
		h.sender.SendText(chatID, "📂 没有找到可切换的会话。")
		return
	}

	best, matches := fuzzyMatchSession(query, sessions)

	if best == nil {
		h.sender.SendText(chatID, fmt.Sprintf("🔍 没有找到与「%s」相关的会话，试试更具体的描述？", query))
		return
	}

	displayID := best.ID
	if len(displayID) > 8 {
		displayID = displayID[:8]
	}

	// High confidence match — switch directly
	if len(matches) == 1 || best.Summary != "" && strings.Contains(strings.ToLower(best.Summary), strings.ToLower(query)) {
		h.sender.SendText(chatID, fmt.Sprintf("🔄 切换到: %s", best.Summary))
		h.handleResumeWithSession(ctx, chatID, userID, best.ID)
		return
	}

	// Multiple candidates — show picker
	text := fmt.Sprintf("🔍 找到多个与「%s」相关的会话：\n\n", query)
	rows := make([][]tgbotapi.InlineKeyboardButton, 0, len(matches)+1)
	for _, m := range matches {
		summary := m.Summary
		if summary == "" {
			summary = m.ID[:8]
		} else if len(summary) > 40 {
			summary = summary[:37] + "..."
		}
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(summary, callbackDataPrefix+"resume:"+m.ID),
		))
	}
	rows = append(rows, tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("❌ 取消", callbackDataPrefix+"cancel:"+sess.ID),
	))

	h.sender.SendInlineKeyboard(chatID, text, rows)
}

func (h *Handler) Handle(ctx context.Context, update tgbotapi.Update) {
	if update.Message == nil {
		return
	}

	msg := update.Message
	userID := msg.From.ID
	chatID := msg.Chat.ID

	if !h.auth.IsAllowed(userID) {
		log.Printf("unauthorized: user_id=%d username=%s", userID, msg.From.UserName)
		h.sender.SendText(chatID, "⛔ Unauthorized.")
		return
	}

	text := strings.TrimSpace(msg.Text)
	if text == "" {
		return
	}

	if msg.IsCommand() {
		cmd := msg.Command()
		args := msg.CommandArguments()

		// === Bot-only commands ===
		switch cmd {
		case "start":
			h.handleStart(chatID)
			return
		case "help":
			h.handleHelp(chatID)
			return
		case "stop":
			if h.sessions.Cancel(userID) {
				h.sender.SendText(chatID, "🛑 Stopping current task...")
			} else {
				h.sender.SendText(chatID, "No active task to stop.")
			}
			return
		case "newsession":
			oldSess := h.sessions.Get(userID)
			h.executor.KillSession(oldSess.ID)
			h.sessions.NewSession(userID)
			h.sender.SendText(chatID, "✅ New session created.")
			return
		case "clear":
			oldSess := h.sessions.Get(userID)
			h.executor.KillSession(oldSess.ID)
			h.sessions.NewSession(userID)
			h.sender.SendText(chatID, "🗑 Session cleared.")
			return
		case "setdir":
			h.handleSetDir(chatID, userID, args)
			return
		case "adddir", "add_dir":
			dir := strings.TrimSpace(args)
			if dir == "" {
				h.sender.SendText(chatID, "Usage: /adddir <path>")
				return
			}
			oldSess := h.sessions.Get(userID)
			h.executor.KillSession(oldSess.ID)
			h.sessions.AddDir(userID, dir)
			h.sessions.NewSession(userID)
			h.sender.SendText(chatID, fmt.Sprintf("📂 Added directory: %s\n(Session restarted)", dir))
			return
		case "sessionstatus":
			h.handleSessionStatus(chatID, userID)
			return
		case "id":
			h.sender.SendText(chatID, fmt.Sprintf("Your Telegram user ID: %d", userID))
			return

		// === CLI subcommands (need separate process) ===
		case "version":
			h.handleCLICommand(ctx, chatID, "--version")
			return
		case "doctor":
			h.handleCLISubcommand(ctx, chatID, userID, "doctor")
			return
		case "config":
			subcmd := "config list"
			if args != "" {
				subcmd = "config " + args
			}
			h.handleCLISubcommand(ctx, chatID, userID, subcmd)
			return
		case "mcp":
			subcmd := "mcp list"
			if args != "" {
				subcmd = "mcp " + args
			}
			h.handleCLISubcommand(ctx, chatID, userID, subcmd)
			return
		case "agents":
			h.handleCLISubcommand(ctx, chatID, userID, "agents")
			return
		case "plugins", "plugin":
			subcmd := "plugin list"
			if args != "" {
				subcmd = "plugin " + args
			}
			h.handleCLISubcommand(ctx, chatID, userID, subcmd)
			return
		case "auth":
			if args != "" {
				h.handleCLISubcommand(ctx, chatID, userID, "auth "+args)
			} else {
				h.sender.SendText(chatID, "🔐 /auth login | logout | status")
			}
			return
		case "login":
			h.handleCLISubcommand(ctx, chatID, userID, "auth login")
			return
		case "logout":
			h.handleCLISubcommand(ctx, chatID, userID, "auth logout")
			return
		case "automode":
			h.handleCLISubcommand(ctx, chatID, userID, "auto-mode")
			return
		case "update", "upgrade":
			h.handleCLISubcommand(ctx, chatID, userID, "update")
			return
		case "install":
			if args != "" {
				h.handleCLISubcommand(ctx, chatID, userID, "install "+args)
			} else {
				h.sender.SendText(chatID, "📦 /install [stable|latest|version]")
			}
			return

		// === Interactive commands with inline picker ===
		case "continue":
			h.handleContinueOrPick(ctx, chatID, userID)
			return
		case "resume":
			h.handleResumeOrPick(ctx, chatID, userID)
			return
		case "frompr":
			h.handleFromPRCommand(ctx, chatID, userID, args)
			return
		case "sessions":
			h.handleSessionsCommand(ctx, chatID, userID)
			return
		}

		// === Claude Code slash commands → forward directly ===
		if claudeSlashCommands[cmd] {
			slashCmd := "/" + cmd
			if args != "" {
				slashCmd += " " + args
			}
			h.handleClaudeMessage(ctx, chatID, userID, slashCmd)
			return
		}

		// Unknown slash command → forward raw text to Claude
	}

	// === Natural language session switch ===
	if query, ok := detectSwitchIntent(text); ok {
		h.handleSwitchSession(ctx, chatID, userID, query)
		return
	}

	h.handleClaudeMessage(ctx, chatID, userID, text)
}

func (h *Handler) handleStart(chatID int64) {
	h.sender.SendText(chatID, "🤖 Claude Code Telegram Proxy\n\nSend any message to chat with Claude!\nUse /help for commands.")
}

func (h *Handler) handleHelp(chatID int64) {
	h.sender.SendText(chatID, `📖 Commands

🤖 Bot:
/start - Welcome
/help - This list
/stop - Stop current task
/newsession - New conversation
/clear - Clear history
/setdir <path> - Set working dir
/adddir <path> - Add extra dir access
/sessionstatus - Session info
/sessions - List saved sessions
/id - Your user ID

💬 Claude Code (forwarded directly):
/compact - Compress context
/context - Context usage
/cost - Session cost
/model - Current model
/memory - Read/edit CLAUDE.md
/review - Code review
/securityreview - Security audit
/insights - Project insights
/simplify - Simplify code
/debug <issue> - Debug
/init - Create CLAUDE.md
/prd - Generate PRD
/batch <tasks> - Batch execute
/loop <task> - Loop until done

🔄 Session Management:
/continue - Continue last conversation
/resume - Resume from saved sessions (picker)
/frompr <ref> - Resume from PR (number or URL)
/sessions - List all saved sessions

🛠 Skills (forwarded directly):
/updateconfig /claudeapi /tencentcloud
/longbridge /tencenttatops /ralph

⚙️ System (CLI):
/version /doctor /config /mcp
/agents /plugins /automode
/auth /login /logout
/update /install

Any text → sent to Claude Code`)
}

func (h *Handler) handleSetDir(chatID int64, userID int64, dir string) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		h.sender.SendText(chatID, "Usage: /setdir <path>")
		return
	}
	oldSess := h.sessions.Get(userID)
	h.executor.KillSession(oldSess.ID)
	h.sessions.SetWorkDir(userID, dir)
	h.sessions.NewSession(userID)
	h.sender.SendText(chatID, fmt.Sprintf("📁 Working directory: %s\n(New session started)", dir))
}

func (h *Handler) handleSessionStatus(chatID int64, userID int64) {
	sess := h.sessions.Get(userID)
	h.sender.SendText(chatID, fmt.Sprintf("📊 Session: %s\nDir: %s\nMessages: %d\nCreated: %s",
		sess.ID[:8], sess.WorkDir, sess.MessageCount, sess.Created.Format("15:04:05")))
}

func (h *Handler) handleCLICommand(ctx context.Context, chatID int64, flag string) {
	h.sender.SendTyping(chatID)
	output, err := h.executor.ExecuteFlag(ctx, flag)
	if err != nil {
		h.sender.SendText(chatID, fmt.Sprintf("⚠️ %s", err.Error()))
		return
	}
	if output == "" {
		output = "(no output)"
	}
	h.sender.SendText(chatID, output)
}

func (h *Handler) handleCLISubcommand(ctx context.Context, chatID int64, userID int64, subcmd string) {
	h.sender.SendTyping(chatID)
	sess := h.sessions.Get(userID)
	output, err := h.executor.ExecuteSubcommand(ctx, subcmd, sess.WorkDir)
	if err != nil {
		h.sender.SendText(chatID, fmt.Sprintf("⚠️ %s", err.Error()))
		return
	}
	if output == "" {
		output = "(no output)"
	}
	h.sender.SendText(chatID, output)
}

// callbackDataPrefix is used to prefix callback data for inline buttons.
const callbackDataPrefix = "cb:"

// HandleCallback processes callback queries from inline keyboard buttons.
func (h *Handler) HandleCallback(ctx context.Context, cq tgbotapi.CallbackQuery) {
	// Answer first to remove "loading" state
	h.sender.AnswerCallbackQuery(cq.ID, "")

	data := cq.Data
	if !strings.HasPrefix(data, callbackDataPrefix) {
		return
	}
	data = strings.TrimPrefix(data, callbackDataPrefix)

	parts := strings.SplitN(data, ":", 2)
	if len(parts) < 2 {
		return
	}
	action := parts[0]
	payload := parts[1]

	switch action {
	case "resume":
		h.handleResumeWithSession(ctx, cq.Message.Chat.ID, cq.From.ID, payload)
	case "frompr":
		h.handleFromPR(ctx, cq.Message.Chat.ID, cq.From.ID, payload)
	case "continue":
		h.handleContinue(ctx, cq.Message.Chat.ID, cq.From.ID)
	}
}

// handleResumeWithSession resumes a session with the given session ID.
func (h *Handler) handleResumeWithSession(ctx context.Context, chatID int64, userID int64, sessionID string) {
	sess := h.sessions.Get(userID)
	h.executor.KillSession(sess.ID)
	h.sessions.NewSession(userID) // fresh Bot session
	displayID := sessionID
	if len(displayID) > 8 {
		displayID = displayID[:8]
	}
	h.sender.SendText(chatID, "▶️ Resuming session: "+displayID+"...")
	h.handleClaudeMessageWithResume(ctx, chatID, userID, "hi", sessionID)
}

// handleFromPR resumes from a PR.
func (h *Handler) handleFromPR(ctx context.Context, chatID int64, userID int64, prRef string) {
	h.sender.SendText(chatID, "📋 Resuming from PR: "+prRef)
	// --from-pr is a CLI flag, not a message to Claude. Need to handle via subcommand.
	h.handleCLISubcommand(ctx, chatID, userID, "--from-pr "+prRef)
}

// handleContinue continues the most recent conversation.
func (h *Handler) handleContinue(ctx context.Context, chatID int64, userID int64) {
	// Find the most recent session to resume
	sess := h.sessions.Get(userID)
	sessions, err := h.executor.ListSessions(ctx, sess.WorkDir)
	if err != nil || len(sessions) == 0 {
		h.sender.SendText(chatID, "📂 No saved sessions found.")
		return
	}
	// Resume the most recent one
	h.handleResumeWithSession(ctx, chatID, userID, sessions[0].ID)
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// handleContinueOrPick shows a session picker if multiple sessions exist, otherwise continues.
func (h *Handler) handleContinueOrPick(ctx context.Context, chatID int64, userID int64) {
	sess := h.sessions.Get(userID)
	sessions, err := h.executor.ListSessions(ctx, sess.WorkDir)
	if err != nil || len(sessions) == 0 {
		h.sender.SendText(chatID, "📂 No saved sessions found.")
		return
	}
	// Resume the most recent one
	h.handleResumeWithSession(ctx, chatID, userID, sessions[0].ID)
}

// handleResumeOrPick shows a session picker for the user to choose.
func (h *Handler) handleResumeOrPick(ctx context.Context, chatID int64, userID int64) {
	sess := h.sessions.Get(userID)
	sessions, err := h.executor.ListSessions(ctx, sess.WorkDir)
	if err != nil || len(sessions) == 0 {
		h.sender.SendText(chatID, "📂 No saved sessions found.")
		return
	}

	text := "📂 *Select a session to resume*\n"
	rows := make([][]tgbotapi.InlineKeyboardButton, 0, len(sessions)+1)
	for _, s := range sessions {
		summary := s.Summary
		if summary == "" {
			summary = s.ID[:8]
		} else if len(summary) > 50 {
			summary = summary[:47] + "..."
		}
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(summary[:minInt(len(summary), 40)], callbackDataPrefix+"resume:"+s.ID),
		))
	}
	// Cancel button
	rows = append(rows, tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("❌ Cancel", callbackDataPrefix+"cancel:"+sess.ID),
	))

	h.sender.SendInlineKeyboard(chatID, text, rows)
}

// handleFromPRCommand handles /frompr command with optional PR ref.
func (h *Handler) handleFromPRCommand(ctx context.Context, chatID int64, userID int64, prRef string) {
	if prRef == "" {
		h.sender.SendText(chatID, "Usage: /frompr <PR number or URL>")
		return
	}
	h.handleFromPR(ctx, chatID, userID, prRef)
}

// handleSessionsCommand shows a list of available sessions.
func (h *Handler) handleSessionsCommand(ctx context.Context, chatID int64, userID int64) {
	sess := h.sessions.Get(userID)
	sessions, err := h.executor.ListSessions(ctx, sess.WorkDir)
	if err != nil || len(sessions) == 0 {
		h.sender.SendText(chatID, "📂 No saved sessions found.")
		return
	}

	text := "📂 *Available sessions*\n"
	rows := make([][]tgbotapi.InlineKeyboardButton, 0, len(sessions)+1)
	for _, s := range sessions {
		summary := s.Summary
		if summary == "" {
			summary = s.ID[:8]
		} else if len(summary) > 50 {
			summary = summary[:47] + "..."
		}
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(summary[:minInt(len(summary), 40)], callbackDataPrefix+"resume:"+s.ID),
		))
	}
	// Cancel button
	rows = append(rows, tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("❌ Cancel", callbackDataPrefix+"cancel:"+sess.ID),
	))

	h.sender.SendInlineKeyboard(chatID, text, rows)
}

// handleClaudeMessage sends the user's message to Claude and streams the response.
func (h *Handler) handleClaudeMessage(ctx context.Context, chatID int64, userID int64, text string) {
	h.handleClaudeMessageWithResume(ctx, chatID, userID, text, "")
}

// handleClaudeMessageWithResume sends a message, optionally resuming a Claude session.
func (h *Handler) handleClaudeMessageWithResume(ctx context.Context, chatID int64, userID int64, text string, resumeSessionID string) {
	sess := h.sessions.Get(userID)

	if !sess.Mu.TryLock() {
		h.sender.SendText(chatID, "⏳ Previous request still processing. Send /stop to cancel.")
		return
	}
	defer sess.Mu.Unlock()

	h.sender.SendTyping(chatID)
	h.sessions.IncrementMessageCount(userID)

	// Create a cancellable context for /stop support
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	h.sessions.SetCancel(userID, cancel)
	defer h.sessions.ClearCancel(userID)

	chunks := make(chan claude.Chunk, 100)
	stream := h.sender.NewStreamSender(chatID)

	errCh := make(chan error, 1)
	go func() {
		err := h.executor.Execute(ctx, claude.ExecRequest{
			Message:         text,
			SessionID:       sess.ID,
			ResumeSessionID: resumeSessionID,
			WorkDir:         sess.WorkDir,
			AddDirs:         sess.AddDirs,
		}, chunks)
		close(chunks)
		errCh <- err
	}()

	// Typing indicator in background
	typingDone := make(chan struct{})
	go func() {
		ticker := time.NewTicker(4 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-typingDone:
				return
			case <-ticker.C:
				h.sender.SendTyping(chatID)
			}
		}
	}()

	hasContent := false
	var statusMsgID int // current status message (will be deleted on completion)

	for chunk := range chunks {
		switch chunk.Type {
		case claude.ChunkText:
			if chunk.Text != "" {
				hasContent = true
				// If there's a status message, delete it since we have real content now
				if statusMsgID != 0 {
					h.sender.DeleteMessage(chatID, statusMsgID)
					statusMsgID = 0
				}
				if err := stream.Append(chunk.Text); err != nil {
					log.Printf("stream append failed: %v", err)
				}
			}

		case claude.ChunkStatus:
			// Show tool execution status as a separate message
			if statusMsgID == 0 {
				id, err := h.sender.SendStatus(chatID, chunk.Text)
				if err == nil {
					statusMsgID = id
				}
			} else {
				h.sender.EditStatus(chatID, statusMsgID, chunk.Text)
			}

		case claude.ChunkThinking:
			// Update status message with thinking summary
			thinkText := "💭 " + chunk.Text
			if statusMsgID == 0 {
				id, err := h.sender.SendStatus(chatID, thinkText)
				if err == nil {
					statusMsgID = id
				}
			} else {
				h.sender.EditStatus(chatID, statusMsgID, thinkText)
			}

		case claude.ChunkError:
			if chunk.Text != "" {
				h.sender.SendText(chatID, "⚠️ "+chunk.Text)
			}
		}
	}
	close(typingDone)

	// Clean up status message
	if statusMsgID != 0 {
		h.sender.DeleteMessage(chatID, statusMsgID)
	}

	if hasContent {
		if err := stream.Finalize(); err != nil {
			log.Printf("stream finalize failed: %v", err)
		}
	}

	if err := <-errCh; err != nil {
		h.sender.SendText(chatID, fmt.Sprintf("⚠️ %s", err.Error()))
	} else if !hasContent {
		h.sender.SendText(chatID, "⚠️ Empty response. Try again.")
	}
}
