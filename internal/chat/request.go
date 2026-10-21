// Package chat translates OpenAI chat completions to Claude Code turns and
// back.
//
// Each HTTP request is self-contained: the system and developer messages
// become the system prompt, and the rest of the conversation becomes Claude
// turns (text, images, documents, tool calls, tool results) that the driver
// replays before the last one. The plugin keeps no state between requests.
package chat

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"regexp"
	"slices"
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

// contentPart is one OpenAI content part.
type contentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	ImageURL *struct {
		URL string `json:"url"`
	} `json:"image_url"`
	File *struct {
		FileData string `json:"file_data"`
	} `json:"file"`
}

// Media types Claude accepts in image and document blocks.
var (
	imageTypes    = map[string]bool{"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true}
	documentTypes = map[string]bool{"application/pdf": true}
)

// contentText returns the text of a message's content: a string, or an
// array of text parts. Other part types are rejected.
func contentText(raw json.RawMessage) (string, error) {
	blocks, err := contentBlocks(raw, false)
	if err != nil || len(blocks) == 0 {
		return "", err
	}
	return blocks[0].Text, nil
}

// contentBlocks converts a message's content, a string or an array of
// content parts, to Claude blocks. Adjacent text parts become one text
// block. Image and file parts are accepted only when media is true.
func contentBlocks(raw json.RawMessage, media bool) ([]claudecli.Block, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s == "" {
			return nil, nil
		}
		return []claudecli.Block{{Kind: claudecli.BlockText, Text: s}}, nil
	}
	var parts []contentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, fmt.Errorf("content must be a string or an array of content parts")
	}
	var blocks []claudecli.Block
	var texts []string
	flush := func() {
		if text := strings.Join(texts, "\n"); text != "" {
			blocks = append(blocks, claudecli.Block{Kind: claudecli.BlockText, Text: text})
		}
		texts = nil
	}
	for _, p := range parts {
		if p.Type == "text" {
			texts = append(texts, p.Text)
			continue
		}
		if !media {
			return nil, fmt.Errorf("content part type %q is not supported here; only text", p.Type)
		}
		b, err := mediaBlock(p)
		if err != nil {
			return nil, err
		}
		flush()
		blocks = append(blocks, b)
	}
	flush()
	return blocks, nil
}

// mediaBlock converts an image_url or file part to an image or document
// block.
func mediaBlock(p contentPart) (claudecli.Block, error) {
	switch p.Type {
	case "image_url":
		if p.ImageURL == nil {
			return claudecli.Block{}, fmt.Errorf("image_url part has no image_url")
		}
		u := p.ImageURL.URL
		if strings.HasPrefix(u, "https://") {
			return claudecli.Block{Kind: claudecli.BlockImage, URL: u}, nil
		}
		mediaType, data, err := dataURL(u, imageTypes)
		if err != nil {
			return claudecli.Block{}, fmt.Errorf("image_url: %w; send an https URL or a base64 data URL", err)
		}
		return claudecli.Block{Kind: claudecli.BlockImage, MediaType: mediaType, Data: data}, nil
	case "file":
		if p.File == nil {
			return claudecli.Block{}, fmt.Errorf("file part has no file")
		}
		if p.File.FileData == "" {
			return claudecli.Block{}, fmt.Errorf("file parts need file_data; file_id is not supported")
		}
		mediaType, data, err := dataURL(p.File.FileData, documentTypes)
		if err != nil {
			return claudecli.Block{}, fmt.Errorf("file_data: %w", err)
		}
		return claudecli.Block{Kind: claudecli.BlockDocument, MediaType: mediaType, Data: data}, nil
	case "input_audio":
		return claudecli.Block{}, fmt.Errorf("audio input is not supported")
	}
	return claudecli.Block{}, fmt.Errorf("content part type %q is not supported", p.Type)
}

// dataURL splits a base64 data URL ("data:<media type>;base64,<data>") into
// its media type and data. The media type must be in allowed and the data
// must be valid base64: Claude Code turns a bad image into a text note
// instead of an error.
func dataURL(u string, allowed map[string]bool) (mediaType, data string, err error) {
	rest, ok := strings.CutPrefix(u, "data:")
	if !ok {
		return "", "", fmt.Errorf("not a data URL")
	}
	meta, data, ok := strings.Cut(rest, ",")
	mediaType, base64Encoded := strings.CutSuffix(meta, ";base64")
	if !ok || !base64Encoded || data == "" {
		return "", "", fmt.Errorf("data URL must be base64 encoded")
	}
	mediaType = strings.ToLower(mediaType)
	if !allowed[mediaType] {
		return "", "", fmt.Errorf("media type %q is not supported (accepted: %s)", mediaType, strings.Join(slices.Sorted(maps.Keys(allowed)), ", "))
	}
	if _, err := io.Copy(io.Discard, base64.NewDecoder(base64.StdEncoding, strings.NewReader(data))); err != nil {
		return "", "", fmt.Errorf("data URL has invalid base64 data")
	}
	return mediaType, data, nil
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
		content, err := contentBlocks(m.Content, m.Role != "assistant")
		if err != nil {
			return nil, invalid("messages: %v", err)
		}
		switch m.Role {
		case "user":
			t := userTurn()
			t.Content = appendContent(t.Content, content)
		case "assistant":
			t := claudecli.Turn{Role: claudecli.RoleAssistant, Content: content}
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
			if len(t.Content) > 0 || len(t.ToolCalls) > 0 {
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
			if len(t.Content) > 0 {
				return nil, invalid("tool message %d must come right after the assistant tool call", i)
			}
			t.ToolResults = append(t.ToolResults, claudecli.ToolResult{ToolCallID: m.ToolCallID, Content: content})
		}
	}
	for i, t := range turns {
		if t.Role == claudecli.RoleUser && len(t.Content) == 0 && len(t.ToolResults) == 0 {
			return nil, invalid("turn %d: a user message has no content", i)
		}
	}
	return turns, nil
}

// appendContent appends the content of a user message to the open user
// turn. Text that meets text is joined with a blank line.
func appendContent(dst, src []claudecli.Block) []claudecli.Block {
	if n := len(dst); n > 0 && len(src) > 0 && dst[n-1].Kind == claudecli.BlockText && src[0].Kind == claudecli.BlockText {
		dst[n-1].Text += "\n\n" + src[0].Text
		src = src[1:]
	}
	return append(dst, src...)
}
