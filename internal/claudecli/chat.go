package claudecli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

const (
	// bridgeServer is the name of the in-process MCP server that exposes a
	// request's client tools. Claude Code names its tools
	// mcp__<server>__<tool>; a one-letter server keeps names short.
	bridgeServer = "c"
	bridgePrefix = "mcp__" + bridgeServer + "__"
)

// bridgeConfig is the --mcp-config value for the tool bridge. "sdk" servers
// are served over the control channel instead of a separate process.
var bridgeConfig = `{"mcpServers":{"` + bridgeServer + `":{"type":"sdk","name":"` + bridgeServer + `","alwaysLoad":true}}}`

// Tool is a client-defined function that Claude may call. The plugin never
// executes it: the run stops at the call and the client runs the tool.
type Tool struct {
	Name        string
	Description string
	// InputSchema is a JSON Schema object for the arguments.
	InputSchema json.RawMessage
}

// Turn roles.
const (
	RoleUser      = "user"
	RoleAssistant = "assistant"
)

// Turn is one message of the conversation.
type Turn struct {
	// Role is RoleUser or RoleAssistant.
	Role string
	// Content is the message in order. Assistant turns hold only text.
	Content []Block
	// ToolCalls are the assistant's calls to client tools.
	ToolCalls []ToolCall
	// ToolResults answer the previous assistant turn's tool calls; only on
	// user turns.
	ToolResults []ToolResult
}

// BlockKind is the Anthropic content block type of a Block.
type BlockKind string

// Block kinds.
const (
	BlockText     BlockKind = "text"
	BlockImage    BlockKind = "image"
	BlockDocument BlockKind = "document"
)

// Block is one piece of message or tool result content.
type Block struct {
	Kind BlockKind
	// Text is the text of a text block.
	Text string
	// MediaType and Data are the type and base64 data of an image or
	// document.
	MediaType string
	Data      string
	// URL is an image that the API fetches, in place of Data.
	URL string
}

// MarshalJSON encodes b as an Anthropic content block.
func (b Block) MarshalJSON() ([]byte, error) {
	switch b.Kind {
	case BlockText:
		return json.Marshal(map[string]any{"type": "text", "text": b.Text})
	case BlockImage, BlockDocument:
		source := map[string]any{"type": "base64", "media_type": b.MediaType, "data": b.Data}
		if b.URL != "" {
			source = map[string]any{"type": "url", "url": b.URL}
		}
		return json.Marshal(map[string]any{"type": string(b.Kind), "source": source})
	}
	return nil, fmt.Errorf("unknown content block kind %q", b.Kind)
}

// ToolCall is a client tool call the assistant made earlier.
type ToolCall struct {
	ID   string
	Name string
	// Input is the JSON object of arguments.
	Input json.RawMessage
}

// ToolResult is the client's result for a tool call.
type ToolResult struct {
	ToolCallID string
	Content    []Block
}

// Request is one chat completion: the conversation so far and the turn to
// answer.
type Request struct {
	// Model is a value for --model.
	Model string
	// Effort is a --effort level, or empty for the model default.
	Effort string
	// SystemPrompt replaces Claude Code's system prompt. Empty is allowed.
	SystemPrompt string
	// Turns is the conversation, oldest first. The last turn must be a user
	// turn: Claude answers it. Earlier turns are replayed as history.
	Turns []Turn
	// Tools are client functions exposed through the tool bridge.
	Tools []Tool
}

// EventKind identifies a streamed Event.
type EventKind int

const (
	// TextDelta carries a piece of assistant text in Text.
	TextDelta EventKind = iota + 1
	// ToolCallStart opens a tool call: ToolIndex, ToolID, and ToolName.
	ToolCallStart
	// ToolCallArgs carries a piece of a tool call's JSON arguments in Text.
	ToolCallArgs
)

// Event is one streamed piece of the assistant's reply.
type Event struct {
	Kind EventKind
	Text string
	// ToolIndex counts tool calls from 0 in the order Claude makes them.
	ToolIndex int
	ToolID    string
	// ToolName is the client's function name (bridge prefix removed).
	ToolName string
}

// Result summarises a completed turn.
type Result struct {
	// StopReason is Claude's stop reason: end_turn, max_tokens,
	// stop_sequence, refusal, or tool_use when the run stopped at client
	// tool calls.
	StopReason string
	Usage      TokenUsage
}

// streamEvent is the subset of an Anthropic streaming event the driver
// reads.
type streamEvent struct {
	Type         string `json:"type"`
	Index        int    `json:"index"`
	ContentBlock *struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Name string `json:"name"`
		Text string `json:"text"`
	} `json:"content_block"`
	Delta *struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	Usage   *TokenUsage `json:"usage"`
	Message *struct {
		Usage *TokenUsage `json:"usage"`
	} `json:"message"`
}

// chatArgs returns the CLI arguments for a chat turn.
func chatArgs(req Request, systemPromptFile string) []string {
	args := append(baseArgs(),
		"--include-partial-messages",
		// Nothing can approve a tool here, so a bridged call is denied and
		// never runs even if the stop below loses a race with dispatch.
		"--permission-prompts", "none",
		"--disallowedTools", "ToolSearch",
		"--system-prompt-file", systemPromptFile,
		"--model", req.Model,
	)
	if req.Effort != "" {
		args = append(args, "--effort", req.Effort)
	}
	if len(req.Tools) > 0 {
		args = append(args, "--mcp-config", bridgeConfig)
	}
	return args
}

// Chat runs one turn and calls emit for each streamed piece of the reply.
// When Claude calls client tools the run stops after the message that
// holds the calls and Result.StopReason is "tool_use". An error from emit
// stops the run and is returned.
func (d *Driver) Chat(ctx context.Context, req Request, emit func(Event) error) (Result, error) {
	if n := len(req.Turns); n == 0 || req.Turns[n-1].Role != RoleUser {
		return Result{}, fmt.Errorf("chat request must end with a user turn")
	}
	// The system prompt goes through a file: argv has per-argument size
	// limits and a prompt must never be parsed as a flag.
	spFile, err := os.CreateTemp(d.WorkDir, "system-prompt-*.txt")
	if err != nil {
		return Result{}, fmt.Errorf("system prompt file: %w", err)
	}
	defer func() { _ = os.Remove(spFile.Name()) }()
	if _, err := spFile.WriteString(req.SystemPrompt); err != nil {
		_ = spFile.Close()
		return Result{}, fmt.Errorf("write system prompt: %w", err)
	}
	if err := spFile.Close(); err != nil {
		return Result{}, fmt.Errorf("write system prompt: %w", err)
	}

	s, err := d.start(ctx, chatArgs(req, spFile.Name()))
	if err != nil {
		return Result{}, err
	}
	defer s.close()

	bridge := toolBridge(req.Tools)
	if _, err := s.initializeWith(bridge); err != nil {
		return Result{}, err
	}
	if err := s.replay(req.Turns, bridge); err != nil {
		return Result{}, err
	}
	// Without tools nothing else is written, so end input now. With tools,
	// stdin stays open to answer the bridge's MCP requests.
	if len(req.Tools) == 0 {
		s.closeInput()
	}

	t := turn{emit: emit, blocks: map[int]int{}}
	for {
		f, err := s.next()
		if err != nil {
			return Result{}, err
		}
		if f.ParentToolUseID != nil {
			continue
		}
		switch f.Type {
		case "control_request":
			if err := s.answer(f, bridge); err != nil {
				return Result{}, err
			}
		case "assistant":
			if f.Error != "" {
				t.assistantErr = f.Error
			}
		case "stream_event":
			done, err := t.handle(f.Event)
			if err != nil {
				return Result{}, err
			}
			if done {
				return Result{StopReason: "tool_use", Usage: t.usage}, nil
			}
		case "result":
			return t.finish(f)
		}
	}
}

// replay writes the conversation to the CLI as native messages. Every turn
// but the last is history: user turns are sent with shouldQuery false and
// acknowledged before the next frame (assistant frames are applied as they
// are read, user frames are queued), assistant turns as replay frames. The
// last user turn starts the reply. client_composed keeps Claude Code from
// expanding @file mentions or running /commands in message text.
func (s *session) replay(turns []Turn, h controlHandler) error {
	last := len(turns) - 1
	for i, t := range turns {
		if t.Role == RoleAssistant {
			msg := map[string]any{
				"id":      fmt.Sprintf("msg_replay_%d", i),
				"type":    "message",
				"role":    "assistant",
				"content": assistantBlocks(t),
			}
			if err := s.send(map[string]any{"type": "assistant", "message": msg, "parent_tool_use_id": nil, "session_id": ""}); err != nil {
				return err
			}
			continue
		}
		frame := map[string]any{
			"type":               "user",
			"message":            map[string]any{"role": "user", "content": userBlocks(t)},
			"parent_tool_use_id": nil,
			"session_id":         "",
			"client_composed":    true,
		}
		if i == last {
			return s.send(frame)
		}
		frame["shouldQuery"] = false
		if err := s.send(frame); err != nil {
			return err
		}
		if err := s.awaitAck(h); err != nil {
			return err
		}
	}
	return nil
}

// awaitAck reads frames until the CLI acknowledges a history frame with a
// result that ran no model turn.
func (s *session) awaitAck(h controlHandler) error {
	for {
		f, err := s.next()
		if err != nil {
			return err
		}
		switch f.Type {
		case "control_request":
			if err := s.answer(f, h); err != nil {
				return err
			}
		case "result":
			if f.IsError {
				return &UpstreamError{Message: "replaying conversation history failed: " + f.Subtype}
			}
			return nil
		}
	}
}

func assistantBlocks(t Turn) []any {
	blocks := make([]any, 0, len(t.Content)+len(t.ToolCalls))
	for _, b := range t.Content {
		blocks = append(blocks, b)
	}
	for _, tc := range t.ToolCalls {
		blocks = append(blocks, map[string]any{"type": "tool_use", "id": tc.ID, "name": bridgePrefix + tc.Name, "input": tc.Input})
	}
	return blocks
}

func userBlocks(t Turn) []any {
	blocks := make([]any, 0, len(t.ToolResults)+len(t.Content))
	for _, r := range t.ToolResults {
		blocks = append(blocks, map[string]any{"type": "tool_result", "tool_use_id": r.ToolCallID, "content": resultContent(r.Content)})
	}
	for _, b := range t.Content {
		blocks = append(blocks, b)
	}
	return blocks
}

// resultContent is a tool result's content: a string when it is only text,
// or content blocks.
func resultContent(c []Block) any {
	switch {
	case len(c) == 0:
		return ""
	case len(c) == 1 && c[0].Kind == BlockText:
		return c[0].Text
	}
	return c
}

// turn tracks one assistant turn while its stream events arrive.
type turn struct {
	emit func(Event) error
	// blocks maps a content block index in the current message to its tool
	// call index; only bridged tool_use blocks are present.
	blocks       map[int]int
	tools        int
	usage        TokenUsage
	stop         string
	assistantErr string
}

// handle processes one stream event. It reports done when the message
// ended with client tool calls.
func (t *turn) handle(raw json.RawMessage) (bool, error) {
	ev, ok := decodeEvent(raw)
	if !ok {
		return false, nil
	}
	switch ev.Type {
	case "message_start":
		t.blocks = map[int]int{}
		if ev.Message != nil && ev.Message.Usage != nil {
			t.usage = *ev.Message.Usage
		}
	case "content_block_start":
		cb := ev.ContentBlock
		if cb == nil {
			return false, nil
		}
		switch cb.Type {
		case "text":
			if cb.Text != "" {
				return false, t.emit(Event{Kind: TextDelta, Text: cb.Text})
			}
		case "tool_use":
			name, ok := strings.CutPrefix(cb.Name, bridgePrefix)
			if !ok {
				return false, nil
			}
			idx := t.tools
			t.tools++
			t.blocks[ev.Index] = idx
			return false, t.emit(Event{Kind: ToolCallStart, ToolIndex: idx, ToolID: cb.ID, ToolName: name})
		}
	case "content_block_delta":
		if ev.Delta == nil {
			return false, nil
		}
		switch ev.Delta.Type {
		case "text_delta":
			if ev.Delta.Text != "" {
				return false, t.emit(Event{Kind: TextDelta, Text: ev.Delta.Text})
			}
		case "input_json_delta":
			idx, ok := t.blocks[ev.Index]
			if ok && ev.Delta.PartialJSON != "" {
				return false, t.emit(Event{Kind: ToolCallArgs, ToolIndex: idx, Text: ev.Delta.PartialJSON})
			}
		}
	case "message_delta":
		if ev.Usage != nil {
			t.usage = *ev.Usage
		}
		if ev.Delta != nil && ev.Delta.StopReason != "" {
			t.stop = ev.Delta.StopReason
			if t.stop == "tool_use" && len(t.blocks) > 0 {
				return true, nil
			}
		}
	}
	return false, nil
}

// decodeEvent decodes a stream event. Events that do not decode are
// skipped: they carry nothing the driver reads.
func decodeEvent(raw json.RawMessage) (streamEvent, bool) {
	var ev streamEvent
	err := json.Unmarshal(raw, &ev)
	return ev, err == nil
}

// finish turns the CLI's result frame into a Result or an error.
func (t *turn) finish(f frame) (Result, error) {
	if f.IsError {
		msg := f.Result
		if msg == "" {
			msg = strings.Join(f.Errors, "; ")
		}
		if msg == "" {
			msg = f.Subtype
		}
		return Result{}, &UpstreamError{Code: t.assistantErr, Message: msg}
	}
	res := Result{StopReason: f.StopReason, Usage: t.usage}
	if res.StopReason == "" {
		res.StopReason = t.stop
	}
	if f.Usage != nil {
		res.Usage = *f.Usage
	}
	return res, nil
}

// toolBridge returns a control handler that serves tools as an MCP server
// over the control channel. tools/call is never answered: the run stops at
// the call and the client executes the tool. A nil handler is returned when
// there are no tools.
func toolBridge(tools []Tool) controlHandler {
	if len(tools) == 0 {
		return nil
	}
	list := make([]map[string]any, len(tools))
	for i, tl := range tools {
		list[i] = map[string]any{"name": tl.Name, "description": tl.Description, "inputSchema": tl.InputSchema}
	}
	return func(subtype string, raw json.RawMessage) (any, bool, error) {
		if subtype != "mcp_message" {
			return nil, false, fmt.Errorf("unsupported control request: %s", subtype)
		}
		var req struct {
			ServerName string `json:"server_name"`
			Message    struct {
				ID     any    `json:"id"`
				Method string `json:"method"`
			} `json:"message"`
		}
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, false, fmt.Errorf("decode mcp_message: %w", err)
		}
		if req.ServerName != bridgeServer {
			return nil, false, fmt.Errorf("unknown MCP server %q", req.ServerName)
		}
		var result any
		switch req.Message.Method {
		case "initialize":
			result = map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "llm-proxy-client-tools", "version": "1"},
			}
		case "tools/list":
			result = map[string]any{"tools": list}
		case "tools/call":
			return nil, false, nil
		case "notifications/initialized":
			result = map[string]any{}
		default:
			mcp := map[string]any{"jsonrpc": "2.0", "id": req.Message.ID,
				"error": map[string]any{"code": -32601, "message": "method not found: " + req.Message.Method}}
			return map[string]any{"mcp_response": mcp}, true, nil
		}
		mcp := map[string]any{"jsonrpc": "2.0", "result": result}
		if req.Message.ID != nil {
			mcp["id"] = req.Message.ID
		}
		return map[string]any{"mcp_response": mcp}, true, nil
	}
}
