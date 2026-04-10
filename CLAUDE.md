# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build & Run

```bash
go build -o agent-proxy .
./agent-proxy -config config.yaml
```

## Architecture

```
Telegram ←→ Bot API ←→ Handler ←→ Executor ←→ Claude CLI (persistent subprocess)
                    ↓
              Session Manager (per-user UUID sessions)
```

**Key flow:**
1. `bot.Run()` starts long-polling via `GetUpdatesChan`
2. Each `Update` is routed to `Handler.Handle()` in a goroutine
3. Handler checks auth via `middleware.Auth`, then sends to `Executor.Execute()`
4. `Executor` maintains a `PersistentSession` per session-id — one long-running `claude -p` subprocess
5. JSON stream events flow through `chunks chan Chunk` with types: `ChunkText`, `ChunkStatus`, `ChunkThinking`, `ChunkError`
6. `Handler.handleClaudeMessage()` routes chunks to `Sender` methods: `SendMarkdown`, `SendStatus`, `DeleteStatus`

**Persistent sessions:** The executor keeps a `sync.Map` of `*PersistentSession`, each with its own stdin/stdout/stderr pipes. This maintains conversation context across messages via `--session-id`.

**Config precedence:** Environment variables (`TELEGRAM_BOT_TOKEN`, `ALLOWED_USERS`, etc.) override `config.yaml` values.

## Chunk Types

The streaming response uses four chunk types to communicate different event types:
- `ChunkText` — Claude's textual output, rendered as MarkdownV2
- `ChunkStatus` — Tool execution status (e.g. "Running: grep -r..."), sent as separate status message
- `ChunkThinking` — Claude's thinking summary (first 100 chars), sent as italic status
- `ChunkError` — stderr content or error messages

## Important Patterns

- **Per-user serialization:** `Session.Mu sync.Mutex` ensures one request at a time per user
- **Process recovery:** If stdin write fails, executor auto-deletes the session and retries once
- **Graceful shutdown:** SIGINT/SIGTERM triggers `executor.KillAll()` and stops the update channel
- **Cancel mechanism:** `session.Manager.SetCancel(userID, cancel)` stores per-user cancel funcs for `/stop` command
