// Package chat translates OpenAI chat completions to Claude Code turns and
// back.
//
// Each HTTP request is self-contained: the system and developer messages
// become the system prompt, and the rest of the conversation becomes Claude
// turns (text, tool calls, tool results) that the driver replays before the
// last one. The plugin keeps no state between requests.
package chat

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/thelonelyghost/llm-proxy-provider-claudecode/internal/claudecli"
)

// Request is the subset of an OpenAI chat completion request the plugin
// reads. Sampling parameters (temperature, top_p, max_tokens, stop, ...)
// have no Claude Code equivalent and are ignored.
type Request struct {
	Model           string          `json:"model"`
	Messages        []Message       `json:"messages"`
	Stream          bool            `json:"stream"`
	StreamOptions   *StreamOptions  `json:"stream_options"`
	ReasoningEffort string          `json:"reasoning_effort"`
	Tools           []Tool          `json:"tools"`
	ToolChoice      json.RawMessage `json:"tool_choice"`
	Functions       json.RawMessage `json:"functions"`
	N               *int            `json:"n"`
	ResponseFormat  *struct {
		Type string `json:"type"`
	} `json:"response_format"`
}

// StreamOptions are the OpenAI stream options.
type StreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// Message is one OpenAI chat message.
type Message struct {
	Role string `json:"role"`
	// Content is a string, an array of content parts, or null.
	Content    json.RawMessage `json:"content"`
	ToolCalls  []ToolCall      `json:"tool_calls"`
	ToolCallID string          `json:"tool_call_id"`
}

// Tool is an OpenAI tool definition.
type Tool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

// ToolCall is an OpenAI tool call made by the assistant.
type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// Error is a client error with an OpenAI error type.
type Error struct {
	Status int
	Type   string
	Code   string
	Msg    string
}

func (e *Error) Error() string { return e.Msg }

func invalid(format string, args ...any) *Error {
	return &Error{Status: 400, Type: "invalid_request_error", Msg: fmt.Sprintf(format, args...)}
}

// toolName limits function names to what OpenAI allows.
var toolName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// emptySchema is used when a function declares no parameters.
var emptySchema = json.RawMessage(`{"type":"object","properties":{}}`)

// Decode parses and validates a request body.
func Decode(body []byte) (*Request, error) {
	var req Request
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, invalid("invalid JSON body: %v", err)
	}
	if req.Model == "" {
		return nil, invalid("model is required")
	}
	if len(req.Messages) == 0 {
		return nil, invalid("messages must not be empty")
	}
	if req.N != nil && *req.N != 1 {
		return nil, invalid("n must be 1: this backend returns one choice")
	}
	if len(req.Functions) > 0 && string(req.Functions) != "null" {
		return nil, invalid("functions is not supported; use tools")
	}
	if req.ResponseFormat != nil && req.ResponseFormat.Type != "" && req.ResponseFormat.Type != "text" {
		return nil, invalid("response_format %q is not supported; only \"text\"", req.ResponseFormat.Type)
	}
	return &req, nil
}

// Translate builds the Claude Code turn for req. defaultEffort applies when
// the request sets no reasoning_effort.
func Translate(req *Request, defaultEffort string) (claudecli.Request, error) {
	effort := defaultEffort
	if req.ReasoningEffort != "" {
		e, err := claudecli.ParseEffort(req.ReasoningEffort)
		if err != nil {
			return claudecli.Request{}, invalid("%v", err)
		}
		effort = e
	}

	useTools, err := toolChoice(req.ToolChoice)
	if err != nil {
		return claudecli.Request{}, err
	}
	var tools []claudecli.Tool
	if useTools {
		tools, err = translateTools(req.Tools)
		if err != nil {
			return claudecli.Request{}, err
		}
	}

	var system []string
	var convo []Message
	for i, m := range req.Messages {
		switch m.Role {
		case "system", "developer":
			text, err := contentText(m.Content)
			if err != nil {
				return claudecli.Request{}, invalid("messages[%d]: %v", i, err)
			}
			if text != "" {
				system = append(system, text)
			}
		case "user", "assistant", "tool":
			convo = append(convo, m)
		default:
			return claudecli.Request{}, invalid("messages[%d]: unsupported role %q", i, m.Role)
		}
	}
	if len(convo) == 0 {
		return claudecli.Request{}, invalid("messages must include a user message")
	}
	if last := convo[len(convo)-1].Role; last != "user" && last != "tool" {
		return claudecli.Request{}, invalid("the last message must be from the user or a tool, not %q", last)
	}

	turns, err := buildTurns(convo)
	if err != nil {
		return claudecli.Request{}, err
	}
	return claudecli.Request{
		Model:        req.Model,
		Effort:       effort,
		SystemPrompt: strings.Join(system, "\n\n"),
		Turns:        turns,
		Tools:        tools,
	}, nil
}

// toolChoice reports whether tools are offered. Forcing a call is not
// supported: Claude Code decides on its own whether to call a tool.
func toolChoice(raw json.RawMessage) (bool, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return true, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch s {
		case "auto":
			return true, nil
		case "none":
			return false, nil
		}
		return false, invalid("tool_choice %q is not supported; use \"auto\" or \"none\"", s)
	}
	return false, invalid("tool_choice naming a function is not supported; use \"auto\" or \"none\"")
}

func translateTools(in []Tool) ([]claudecli.Tool, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make([]claudecli.Tool, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for i, t := range in {
		if t.Type != "function" {
			return nil, invalid("tools[%d]: unsupported type %q", i, t.Type)
		}
		name := t.Function.Name
		if !toolName.MatchString(name) {
			return nil, invalid("tools[%d]: function name %q must match %s", i, name, toolName)
		}
		if _, dup := seen[name]; dup {
			return nil, invalid("tools[%d]: duplicate function name %q", i, name)
		}
		seen[name] = struct{}{}
		schema := t.Function.Parameters
		if len(bytes.TrimSpace(schema)) == 0 || string(schema) == "null" {
			schema = emptySchema
		} else {
			var obj map[string]any
			if err := json.Unmarshal(schema, &obj); err != nil {
				return nil, invalid("tools[%d]: parameters must be a JSON Schema object", i)
			}
			if typ, ok := obj["type"]; ok && typ != "object" {
				return nil, invalid("tools[%d]: parameters must have type \"object\"", i)
			}
		}
		out = append(out, claudecli.Tool{Name: name, Description: t.Function.Description, InputSchema: schema})
	}
	return out, nil
}

// contentText returns the text of a message's content: a string, or an
// array of text parts. Other part types are rejected.
func contentText(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", fmt.Errorf("content must be a string or an array of content parts")
	}
	texts := make([]string, 0, len(parts))
	for _, p := range parts {
		if p.Type != "text" {
			return "", fmt.Errorf("content part type %q is not supported; only text", p.Type)
		}
		texts = append(texts, p.Text)
	}
	return strings.Join(texts, "\n"), nil
}

// buildTurns converts the conversation to Claude turns. Tool messages and a
// user message that follows them form one user turn, with the tool results
// first, as Claude expects after a tool call.
func buildTurns(convo []Message) ([]claudecli.Turn, error) {
	known := map[string]struct{}{}
	turns := make([]claudecli.Turn, 0, len(convo))
	// userTurn returns the open user turn, starting one if needed.
	userTurn := func() *claudecli.Turn {
		if n := len(turns); n > 0 && turns[n-1].Role == claudecli.RoleUser {
			return &turns[n-1]
		}
		turns = append(turns, claudecli.Turn{Role: claudecli.RoleUser})
		return &turns[len(turns)-1]
	}
	for i, m := range convo {
		text, err := contentText(m.Content)
		if err != nil {
			return nil, invalid("messages: %v", err)
		}
		switch m.Role {
		case "user":
			t := userTurn()
			t.Text = joinText(t.Text, text)
		case "assistant":
			t := claudecli.Turn{Role: claudecli.RoleAssistant, Text: text}
			for j, tc := range m.ToolCalls {
				args := json.RawMessage(tc.Function.Arguments)
				if strings.TrimSpace(tc.Function.Arguments) == "" {
					args = json.RawMessage(`{}`)
				}
				var obj map[string]any
				if json.Unmarshal(args, &obj) != nil {
					return nil, invalid("messages[%d].tool_calls[%d]: arguments must be a JSON object", i, j)
				}
				if tc.ID == "" || !toolName.MatchString(tc.Function.Name) {
					return nil, invalid("messages[%d].tool_calls[%d]: needs an id and a valid function name", i, j)
				}
				known[tc.ID] = struct{}{}
				t.ToolCalls = append(t.ToolCalls, claudecli.ToolCall{ID: tc.ID, Name: tc.Function.Name, Input: args})
			}
			if t.Text != "" || len(t.ToolCalls) > 0 {
				turns = append(turns, t)
			}
		case "tool":
			if m.ToolCallID == "" {
				return nil, invalid("tool message %d has no tool_call_id", i)
			}
			if _, ok := known[m.ToolCallID]; !ok {
				return nil, invalid("tool message %d answers unknown tool_call_id %q", i, m.ToolCallID)
			}
			t := userTurn()
			if t.Text != "" {
				return nil, invalid("tool message %d must come right after the assistant tool call", i)
			}
			t.ToolResults = append(t.ToolResults, claudecli.ToolResult{ToolCallID: m.ToolCallID, Content: text})
		}
	}
	for i, t := range turns {
		if t.Role == claudecli.RoleUser && t.Text == "" && len(t.ToolResults) == 0 {
			return nil, invalid("turn %d: a user message has no content", i)
		}
	}
	return turns, nil
}

func joinText(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}
	return a + "\n\n" + b
}
