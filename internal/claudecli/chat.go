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

// Request is one chat turn.
type Request struct {
	// Model is a value for --model.
	Model string
	// Effort is a --effort level, or empty for the model default.
	Effort string
	// SystemPrompt replaces Claude Code's system prompt. Empty is allowed.
	SystemPrompt string
	// Prompt is the user message text.
	Prompt string
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
	user := map[string]any{
		"type":               "user",
		"message":            map[string]any{"role": "user", "content": req.Prompt},
		"parent_tool_use_id": nil,
		"session_id":         "",
	}
	if err := s.send(user); err != nil {
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
