package claude

import (
	"encoding/json"
	"regexp"
	"strings"
)

// Decision represents a choice Claude is asking the user to make
type Decision struct {
	Question string   `json:"question"`
	Context  string   `json:"context"`
	Options  []Option `json:"options"`
}

// Option represents a single choice
type Option struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description"`
}

// DecisionParser extracts decisions from Claude's streaming output
type DecisionParser struct {
	buffer      strings.Builder
	lastContent string
}

// NewDecisionParser creates a new decision parser
func NewDecisionParser() *DecisionParser {
	return &DecisionParser{}
}

// AddContent adds content from a streaming message
func (p *DecisionParser) AddContent(content string) {
	p.lastContent = content
	p.buffer.WriteString(content)
	p.buffer.WriteString("\n")
}

// ExtractDecision tries to extract a decision from the accumulated content
func (p *DecisionParser) ExtractDecision() (*Decision, error) {
	text := p.buffer.String()

	// Look for patterns like:
	// "Which approach should I use?"
	// "1. Option A - description"
	// "2. Option B - description"
	// "3. Option C - description"
	// "4. Option D - description"

	// Try to find a question
	questionRegex := regexp.MustCompile(`(?m)^(Which|What|How|Should I|Do you want|Would you like).*\?`)
	questionMatch := questionRegex.FindString(text)

	if questionMatch == "" {
		return nil, nil // No decision found
	}

	// Try to find numbered options (1., 2., 3., 4.)
	optionRegex := regexp.MustCompile(`(?m)^\s*(\d+)\.\s+(.+?)(?:\s*-\s*(.+))?$`)
	matches := optionRegex.FindAllStringSubmatch(text, -1)

	if len(matches) < 2 {
		return nil, nil // Not enough options
	}

	// Extract options
	options := []Option{}
	for _, match := range matches {
		if len(match) >= 3 {
			num := match[1]
			label := strings.TrimSpace(match[2])
			description := ""
			if len(match) >= 4 && match[3] != "" {
				description = strings.TrimSpace(match[3])
			}

			options = append(options, Option{
				ID:          num,
				Label:       label,
				Description: description,
			})
		}
	}

	// Limit to 4 options (mobile friendly)
	if len(options) > 4 {
		options = options[:4]
	}

	if len(options) < 2 {
		return nil, nil // Need at least 2 options
	}

	// Get context (text before the question)
	contextEnd := strings.Index(text, questionMatch)
	context := ""
	if contextEnd > 0 {
		context = strings.TrimSpace(text[:contextEnd])
	}

	return &Decision{
		Question: questionMatch,
		Context:  context,
		Options:  options,
	}, nil
}

// Reset clears the parser buffer
func (p *DecisionParser) Reset() {
	p.buffer.Reset()
	p.lastContent = ""
}

// ParseThinkingContent extracts thinking/reasoning from content
func ParseThinkingContent(content json.RawMessage) (string, error) {
	var text string
	if err := json.Unmarshal(content, &text); err != nil {
		// Try as object with text field
		var obj struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(content, &obj); err != nil {
			return "", err
		}
		return obj.Text, nil
	}
	return text, nil
}

// IsQuestionAsking checks if content is asking a question
func IsQuestionAsking(content string) bool {
	// Common question patterns
	patterns := []string{
		"Which",
		"What",
		"How should",
		"Should I",
		"Do you want",
		"Would you like",
		"Which approach",
		"What method",
	}

	lower := strings.ToLower(content)
	for _, pattern := range patterns {
		if strings.Contains(lower, strings.ToLower(pattern)) && strings.Contains(content, "?") {
			return true
		}
	}

	return false
}
