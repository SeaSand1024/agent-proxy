package claude

// ChunkType identifies what kind of content a Chunk carries.
type ChunkType int

const (
	ChunkText     ChunkType = iota // Assistant text output
	ChunkStatus                    // Tool execution status (e.g. "Reading main.go...")
	ChunkThinking                  // Thinking summary
	ChunkError                     // Error from stderr or process failure
)

// Chunk is the unified message unit sent from executor to handler.
type Chunk struct {
	Type     ChunkType
	Text     string
	ToolName string // only for ChunkStatus
}

// StreamEvent represents a line of Claude's stream-json output.
type StreamEvent struct {
	Type            string          `json:"type"`
	Subtype         string          `json:"subtype,omitempty"`
	Message         *MessageContent `json:"message,omitempty"`
	Result          string          `json:"result,omitempty"`
	SessionID       string          `json:"session_id,omitempty"`
	IsError         bool            `json:"is_error,omitempty"`
	ParentToolUseID string          `json:"parent_tool_use_id,omitempty"`
	NumTurns        int             `json:"num_turns,omitempty"`
	DurationMs      int             `json:"duration_ms,omitempty"`
}

type MessageContent struct {
	Role    string         `json:"role,omitempty"`
	Content []ContentBlock `json:"content"`
}

type ContentBlock struct {
	Type      string `json:"type"`
	Text      string `json:"text,omitempty"`
	Thinking  string `json:"thinking,omitempty"`
	Name      string `json:"name,omitempty"`
	Input     any    `json:"input,omitempty"`
	ID        string `json:"id,omitempty"`
	Content   any    `json:"content,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
	ToolUseID string `json:"tool_use_id,omitempty"`
}
