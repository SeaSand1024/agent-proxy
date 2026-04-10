package claude

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// PersistentSession holds a long-running Claude process for interactive conversation.
type PersistentSession struct {
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	stdout    *bufio.Scanner
	mu        sync.Mutex
	sessionID string
	alive     bool
	lastUsed  time.Time
	lines     chan string   // stdout lines from reader goroutine
	done      chan struct{} // closed when reader goroutine exits
	stderrBuf strings.Builder
}

type Executor struct {
	claudePath string
	timeout    time.Duration
	sessions   sync.Map // map[string]*PersistentSession
}

func NewExecutor(claudePath string, timeoutSec int) *Executor {
	return &Executor{
		claudePath: claudePath,
		timeout:    time.Duration(timeoutSec) * time.Second,
	}
}

type ExecRequest struct {
	Message   string
	SessionID string
	WorkDir   string
	AddDirs   []string
}

// getOrCreateSession returns an existing persistent Claude process or creates a new one.
func (e *Executor) getOrCreateSession(sessionID string, workDir string, addDirs []string) (*PersistentSession, error) {
	if v, ok := e.sessions.Load(sessionID); ok {
		ps := v.(*PersistentSession)
		if ps.alive {
			return ps, nil
		}
		e.sessions.Delete(sessionID)
	}

	log.Printf("starting persistent claude process: session=%s work_dir=%s add_dirs=%v", sessionID, workDir, addDirs)

	args := []string{
		"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--session-id", sessionID,
	}
	for _, d := range addDirs {
		args = append(args, "--add-dir", d)
	}

	cmd := exec.Command(e.claudePath, args...)
	cmd.Dir = workDir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start claude: %w", err)
	}

	ps := &PersistentSession{
		cmd:       cmd,
		stdin:     stdin,
		sessionID: sessionID,
		alive:     true,
		lastUsed:  time.Now(),
		lines:     make(chan string, 256),
		done:      make(chan struct{}),
	}

	// Scanner for stdout
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)
	ps.stdout = scanner

	// Goroutine: read stdout lines into channel (non-blocking for main loop)
	go func() {
		defer close(ps.done)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line != "" {
				ps.lines <- line
			}
		}
		if err := scanner.Err(); err != nil {
			log.Printf("stdout scanner error session=%s: %v", sessionID, err)
		}
	}()

	// Goroutine: drain stderr
	go func() {
		rd := bufio.NewReader(stderr)
		for {
			line, err := rd.ReadString('\n')
			line = strings.TrimSpace(line)
			if line != "" {
				ps.mu.Lock()
				ps.stderrBuf.WriteString(line)
				ps.stderrBuf.WriteByte('\n')
				ps.mu.Unlock()
				log.Printf("claude stderr [%s]: %s", sessionID, line)
			}
			if err != nil {
				break
			}
		}
	}()

	e.sessions.Store(sessionID, ps)
	log.Printf("persistent claude process started: session=%s pid=%d", sessionID, cmd.Process.Pid)
	return ps, nil
}

// Execute sends a message to the persistent Claude process and streams the response.
func (e *Executor) Execute(ctx context.Context, req ExecRequest, chunks chan<- Chunk) error {
	ctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()

	return e.doExecute(ctx, req, chunks, false)
}

func (e *Executor) doExecute(ctx context.Context, req ExecRequest, chunks chan<- Chunk, isRetry bool) error {
	ps, err := e.getOrCreateSession(req.SessionID, req.WorkDir, req.AddDirs)
	if err != nil {
		return err
	}

	ps.mu.Lock()
	ps.lastUsed = time.Now()

	log.Printf("sending message: session=%s len=%d retry=%v", req.SessionID, len(req.Message), isRetry)

	if err := e.sendMessage(ps, req); err != nil {
		ps.mu.Unlock()

		if isRetry {
			return fmt.Errorf("send after recover failed: %w", err)
		}

		// Process dead — try to auto-recover once
		log.Printf("write failed, attempting auto-recover: %v", err)
		ps.alive = false
		e.sessions.Delete(req.SessionID)
		chunks <- Chunk{Type: ChunkStatus, Text: "Session reconnected"}
		return e.doExecute(ctx, req, chunks, true)
	}

	err = e.readResponse(ctx, ps, chunks)
	ps.mu.Unlock()
	return err
}

func (e *Executor) sendMessage(ps *PersistentSession, req ExecRequest) error {
	userMsg := map[string]interface{}{
		"type": "user",
		"message": map[string]interface{}{
			"role": "user",
			"content": []map[string]interface{}{
				{
					"type": "text",
					"text": req.Message,
				},
			},
		},
	}

	msgBytes, err := json.Marshal(userMsg)
	if err != nil {
		return fmt.Errorf("marshal message: %w", err)
	}

	_, err = ps.stdin.Write(append(msgBytes, '\n'))
	if err != nil {
		ps.alive = false
		return fmt.Errorf("write stdin: %w", err)
	}
	return nil
}

func (e *Executor) readResponse(ctx context.Context, ps *PersistentSession, chunks chan<- Chunk) error {
	lineCount := 0
	hasAssistantText := false
	turnCount := 0

	for {
		select {
		case <-ctx.Done():
			// Timeout or cancelled — kill the process stdin to force exit
			log.Printf("context done (timeout/cancel): session=%s", ps.sessionID)
			return fmt.Errorf("request cancelled or timed out")

		case line, ok := <-ps.lines:
			if !ok {
				// Reader goroutine exited — process died
				ps.alive = false
				e.sessions.Delete(ps.sessionID)

				// Check stderr for clues
				ps.mu.Lock()
				stderrOut := ps.stderrBuf.String()
				ps.mu.Unlock()
				if stderrOut != "" {
					chunks <- Chunk{Type: ChunkError, Text: strings.TrimSpace(stderrOut)}
				}
				return fmt.Errorf("claude process exited unexpectedly")
			}

			lineCount++

			var event StreamEvent
			if err := json.Unmarshal([]byte(line), &event); err != nil {
				log.Printf("parse error (line %d): %v, raw: %.200s", lineCount, err, line)
				continue
			}

			switch event.Type {
			case "system":
				log.Printf("stream system: subtype=%s", event.Subtype)

			case "assistant":
				turnCount++
				if event.Message != nil {
					for _, block := range event.Message.Content {
						switch block.Type {
						case "text":
							if block.Text != "" {
								hasAssistantText = true
								chunks <- Chunk{Type: ChunkText, Text: block.Text}
							}
						case "tool_use":
							if block.Name != "" {
								toolDesc := formatToolStatus(block.Name, block.Input)
								chunks <- Chunk{Type: ChunkStatus, Text: toolDesc, ToolName: block.Name}
								log.Printf("tool_use: %s", block.Name)
							}
						case "thinking":
							if block.Thinking != "" {
								// Send first 100 chars as thinking summary
								summary := block.Thinking
								if len(summary) > 100 {
									summary = summary[:100] + "..."
								}
								chunks <- Chunk{Type: ChunkThinking, Text: summary}
							}
						}
					}
				}

			case "user":
				log.Printf("stream user event (tool result): turn=%d", turnCount)

			case "result":
				if event.Result != "" && !hasAssistantText {
					chunks <- Chunk{Type: ChunkText, Text: event.Result}
				}
				if event.IsError && event.Result != "" {
					chunks <- Chunk{Type: ChunkError, Text: event.Result}
				}
				log.Printf("response complete: %d lines, %d turns, has_text=%v, result_len=%d, duration=%dms",
					lineCount, turnCount, hasAssistantText, len(event.Result), event.DurationMs)
				return nil
			}
		}
	}
}

// formatToolStatus creates a human-readable status for a tool invocation.
func formatToolStatus(toolName string, input any) string {
	inputMap, ok := input.(map[string]interface{})
	if !ok {
		return toolName
	}
	switch toolName {
	case "Bash", "bash":
		if cmd, ok := inputMap["command"].(string); ok {
			if len(cmd) > 60 {
				cmd = cmd[:60] + "..."
			}
			return "Running: " + cmd
		}
	case "Read", "read":
		if p, ok := inputMap["file_path"].(string); ok {
			return "Reading " + shortPath(p)
		}
	case "Write", "write":
		if p, ok := inputMap["file_path"].(string); ok {
			return "Writing " + shortPath(p)
		}
	case "Edit", "edit":
		if p, ok := inputMap["file_path"].(string); ok {
			return "Editing " + shortPath(p)
		}
	case "Glob", "glob":
		if pattern, ok := inputMap["pattern"].(string); ok {
			return "Searching: " + pattern
		}
	case "Grep", "grep":
		if pattern, ok := inputMap["pattern"].(string); ok {
			return "Grep: " + pattern
		}
	case "LS", "ls":
		if p, ok := inputMap["path"].(string); ok {
			return "Listing " + shortPath(p)
		}
	}
	// Fallback
	if p, ok := inputMap["file_path"].(string); ok {
		return toolName + ": " + shortPath(p)
	}
	if p, ok := inputMap["path"].(string); ok {
		return toolName + ": " + shortPath(p)
	}
	return toolName
}

// shortPath returns the last 2 components of a path for brevity.
func shortPath(p string) string {
	parts := strings.Split(p, "/")
	if len(parts) <= 2 {
		return p
	}
	return ".../" + strings.Join(parts[len(parts)-2:], "/")
}

// KillSession terminates a persistent session.
func (e *Executor) KillSession(sessionID string) {
	if v, ok := e.sessions.Load(sessionID); ok {
		ps := v.(*PersistentSession)
		ps.mu.Lock()
		defer ps.mu.Unlock()
		if ps.alive {
			ps.stdin.Close()
			if ps.cmd.Process != nil {
				_ = syscall.Kill(-ps.cmd.Process.Pid, syscall.SIGTERM)
			}
			ps.alive = false
			log.Printf("killed persistent session: %s", sessionID)
		}
		e.sessions.Delete(sessionID)
	}
}

// KillAll terminates all persistent sessions.
func (e *Executor) KillAll() {
	e.sessions.Range(func(key, value interface{}) bool {
		ps := value.(*PersistentSession)
		ps.stdin.Close()
		if ps.cmd.Process != nil {
			_ = syscall.Kill(-ps.cmd.Process.Pid, syscall.SIGTERM)
		}
		ps.alive = false
		e.sessions.Delete(key)
		return true
	})
	log.Printf("killed all persistent sessions")
}

// ExecuteFlag runs claude with a single flag (e.g. "--version") and returns stdout.
func (e *Executor) ExecuteFlag(ctx context.Context, flag string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, e.claudePath, flag)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	output, err := cmd.CombinedOutput()
	if err != nil {
		return strings.TrimSpace(string(output)), fmt.Errorf("claude %s: %w\n%s", flag, err, string(output))
	}
	return strings.TrimSpace(string(output)), nil
}

// SessionEntry represents a resumable session from claude sessions list.
type SessionEntry struct {
	ID        string
	SessionID string
	Summary   string
}

// ListSessions uses "claude sessions list --json" to get available sessions.
func (e *Executor) ListSessions(ctx context.Context, workDir string) ([]SessionEntry, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	args := []string{"sessions", "list", "--json"}
	cmd := exec.CommandContext(ctx, e.claudePath, args...)
	cmd.Dir = workDir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("claude sessions list: %w\n%s", err, string(output))
	}

	return parseSessionsList(string(output))
}

// ExecuteSubcommand runs a claude subcommand (e.g. "doctor", "config list").
func (e *Executor) ExecuteSubcommand(ctx context.Context, subcmd string, workDir string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	args := strings.Fields(subcmd)
	cmd := exec.CommandContext(ctx, e.claudePath, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if workDir != "" {
		cmd.Dir = workDir
	}

	output, err := cmd.CombinedOutput()
	result := strings.TrimSpace(string(output))
	if err != nil {
		if result != "" {
			return result, nil
		}
		return "", fmt.Errorf("claude %s: %w", subcmd, err)
	}
	return result, nil
}

func parseSessionsList(output string) ([]SessionEntry, error) {
	// Try to parse as JSON array first
	var entries []SessionEntry
	if err := json.Unmarshal([]byte(output), &entries); err == nil {
		return entries, nil
	}

	// Fallback: plain text format - one session per line "uuid: summary"
	var result []SessionEntry
	lines := strings.Split(strings.TrimSpace(output), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		colonIdx := strings.Index(line, ": ")
		if colonIdx > 0 {
			result = append(result, SessionEntry{
				ID:      line[:colonIdx],
				Summary: line[colonIdx+2:],
			})
		} else if line != "" {
			result = append(result, SessionEntry{
				ID:      line,
				Summary: "",
			})
		}
	}
	return result, nil
}
