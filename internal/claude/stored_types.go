package claude

import (
	"encoding/json"
	"strings"
	"time"
)

// StoredMessage represents a message from ~/.claude/projects JSONL files.
// This is what Claude Code writes to disk during interactive sessions.
type StoredMessage struct {
	// Identity
	UUID       string `json:"uuid"`
	ParentUUID string `json:"parentUuid"`
	SessionID  string `json:"sessionId"`

	// Type: "user", "assistant", "summary", "system"
	Type     string `json:"type"`
	UserType string `json:"userType,omitempty"` // "external" for user messages

	// Context
	CWD     string `json:"cwd"`
	Version string `json:"version"`

	// Timing
	Timestamp time.Time `json:"timestamp"`

	// Content - same structure as StreamMessage.Message
	Message json.RawMessage `json:"message"`

	// Metrics (assistant messages only)
	CostUSD    float64 `json:"costUSD,omitempty"`
	DurationMs int64   `json:"durationMs,omitempty"`

	// Summary messages only
	Summary  string `json:"summary,omitempty"`
	LeafUUID string `json:"leafUuid,omitempty"`
}

// ParsedMessageContent represents the inner message structure.
// This is IDENTICAL to what's inside StreamMessage.Message
type ParsedMessageContent struct {
	ID         string                `json:"id,omitempty"`
	Role       string                `json:"role"` // "user", "assistant", "system"
	Model      string                `json:"model,omitempty"`
	Content    []MessageContentBlock `json:"content"`
	StopReason string                `json:"stop_reason,omitempty"`
	Usage      *UsageInfo            `json:"usage,omitempty"`
}

// MessageContentBlock represents a content block in a message
type MessageContentBlock struct {
	Type  string          `json:"type"` // "text", "tool_use", "tool_result"
	Text  string          `json:"text,omitempty"`
	Name  string          `json:"name,omitempty"`
	ID    string          `json:"id,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

// UsageInfo represents token usage information
type UsageInfo struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

// ToStreamMessage converts a stored message to our existing StreamMessage format.
// This allows us to REUSE all existing parsing logic in handleStreamMessage().
func (sm *StoredMessage) ToStreamMessage() (*StreamMessage, error) {
	msg := &StreamMessage{
		Type: sm.Type,
	}

	// Parse the message field into our existing struct
	if len(sm.Message) > 0 {
		var parsed struct {
			Content []struct {
				Type  string          `json:"type"`
				Text  string          `json:"text,omitempty"`
				Name  string          `json:"name,omitempty"`
				Input json.RawMessage `json:"input,omitempty"`
				ID    string          `json:"id,omitempty"`
			} `json:"content"`
			StopReason string `json:"stop_reason,omitempty"`
		}

		if err := json.Unmarshal(sm.Message, &parsed); err != nil {
			// Not all messages have parseable content (e.g., summary)
			return msg, nil
		}

		msg.Message.Content = parsed.Content
		msg.Message.StopReason = parsed.StopReason
	}

	return msg, nil
}

// GetModel extracts the model name from an assistant message
func (sm *StoredMessage) GetModel() string {
	if sm.Type != "assistant" || len(sm.Message) == 0 {
		return ""
	}

	var parsed struct {
		Model string `json:"model"`
	}
	json.Unmarshal(sm.Message, &parsed)
	return parsed.Model
}

// GetTextContent extracts text content from the message
// Handles multiple formats that Claude Code might use
func (sm *StoredMessage) GetTextContent() string {
	if len(sm.Message) == 0 {
		return ""
	}

	// First, try to parse as standard API format with content array
	var parsed ParsedMessageContent
	if err := json.Unmarshal(sm.Message, &parsed); err == nil {
		for _, block := range parsed.Content {
			if block.Type == "text" && block.Text != "" {
				return block.Text
			}
		}
	}

	// Try alternative format where content is a plain string
	var altFormat struct {
		Content string `json:"content"`
		Text    string `json:"text"`
	}
	if err := json.Unmarshal(sm.Message, &altFormat); err == nil {
		if altFormat.Content != "" {
			return altFormat.Content
		}
		if altFormat.Text != "" {
			return altFormat.Text
		}
	}

	// Try format where top-level has content as array of strings or mixed
	var mixedContent struct {
		Content []interface{} `json:"content"`
	}
	if err := json.Unmarshal(sm.Message, &mixedContent); err == nil {
		var texts []string
		for _, item := range mixedContent.Content {
			switch v := item.(type) {
			case string:
				if v != "" {
					texts = append(texts, v)
				}
			case map[string]interface{}:
				if t, ok := v["type"].(string); ok && t == "text" {
					if text, ok := v["text"].(string); ok && text != "" {
						texts = append(texts, text)
					}
				}
			}
		}
		if len(texts) > 0 {
			return strings.Join(texts, "\n")
		}
	}

	// Try nested message format: {"role": "user", "content": "text"}
	var nestedFormat struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(sm.Message, &nestedFormat); err == nil {
		if nestedFormat.Content != "" {
			return nestedFormat.Content
		}
	}

	// Try to extract any text-like field from raw JSON as last resort
	var rawMap map[string]interface{}
	if err := json.Unmarshal(sm.Message, &rawMap); err == nil {
		// Look for common text fields
		for _, key := range []string{"content", "text", "message", "prompt", "input"} {
			if val, ok := rawMap[key]; ok {
				switch v := val.(type) {
				case string:
					if v != "" {
						return v
					}
				}
			}
		}
	}

	// As absolute last resort, try parsing as plain string
	var plainString string
	if err := json.Unmarshal(sm.Message, &plainString); err == nil && plainString != "" {
		return plainString
	}

	return ""
}

// GetRole returns the message role
func (sm *StoredMessage) GetRole() string {
	if len(sm.Message) == 0 {
		if sm.Type == "user" {
			return "user"
		}
		return sm.Type
	}

	var parsed struct {
		Role string `json:"role"`
	}
	json.Unmarshal(sm.Message, &parsed)

	if parsed.Role != "" {
		return parsed.Role
	}
	return sm.Type
}

// GetToolUses extracts tool use blocks from the message
func (sm *StoredMessage) GetToolUses() []MessageContentBlock {
	if len(sm.Message) == 0 {
		return nil
	}

	var parsed ParsedMessageContent
	if err := json.Unmarshal(sm.Message, &parsed); err != nil {
		return nil
	}

	var tools []MessageContentBlock
	for _, block := range parsed.Content {
		if block.Type == "tool_use" {
			tools = append(tools, block)
		}
	}
	return tools
}

// IsComplete returns true if this message indicates the session is complete
func (sm *StoredMessage) IsComplete() bool {
	if len(sm.Message) == 0 {
		return false
	}

	var parsed struct {
		StopReason string `json:"stop_reason"`
	}
	json.Unmarshal(sm.Message, &parsed)

	return parsed.StopReason == "end_turn"
}
