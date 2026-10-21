package chat

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/thelonelyghost/llm-proxy-provider-claudecode/internal/claudecli"
)

// NewID returns a fresh chat completion ID.
func NewID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "chatcmpl-" + hex.EncodeToString(b)
}

// FinishReason maps a Claude stop reason to an OpenAI finish_reason.
func FinishReason(stop string) string {
	switch stop {
	case "max_tokens", "model_context_window_exceeded":
		return "length"
	case "refusal":
		return "content_filter"
	case "tool_use":
		return "tool_calls"
	default:
		return "stop"
	}
}

// Usage is OpenAI token usage.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// UsageFrom converts Claude token accounting. Cached input counts as
// prompt tokens.
func UsageFrom(u claudecli.TokenUsage) Usage {
	prompt := u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
	return Usage{PromptTokens: prompt, CompletionTokens: u.OutputTokens, TotalTokens: prompt + u.OutputTokens}
}

// Completion is a non-streaming chat completion response.
type Completion struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   Usage    `json:"usage"`
}

// Choice is one completion choice.
type Choice struct {
	Index        int             `json:"index"`
	Message      ResponseMessage `json:"message"`
	FinishReason string          `json:"finish_reason"`
}

// ResponseMessage is the assistant message in a completion.
type ResponseMessage struct {
	Role      string     `json:"role"`
	Content   *string    `json:"content"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
}

// Accumulator collects streamed events into a non-streaming reply.
type Accumulator struct {
	text  strings.Builder
	calls []ToolCall
	args  []strings.Builder
}

// Add records one event.
func (a *Accumulator) Add(ev claudecli.Event) {
	switch ev.Kind {
	case claudecli.TextDelta:
		_, _ = a.text.WriteString(ev.Text)
	case claudecli.ToolCallStart:
		var tc ToolCall
		tc.ID, tc.Type, tc.Function.Name = ev.ToolID, "function", ev.ToolName
		a.calls = append(a.calls, tc)
		a.args = append(a.args, strings.Builder{})
	case claudecli.ToolCallArgs:
		if ev.ToolIndex >= 0 && ev.ToolIndex < len(a.args) {
			_, _ = a.args[ev.ToolIndex].WriteString(ev.Text)
		}
	}
}

// Completion builds the response for a finished turn.
func (a *Accumulator) Completion(id, model string, created int64, res claudecli.Result) Completion {
	msg := ResponseMessage{Role: "assistant"}
	if text := a.text.String(); text != "" || len(a.calls) == 0 {
		msg.Content = &text
	}
	for i := range a.calls {
		a.calls[i].Function.Arguments = argsOrEmpty(a.args[i].String())
	}
	msg.ToolCalls = a.calls
	return Completion{
		ID:      id,
		Object:  "chat.completion",
		Created: created,
		Model:   model,
		Choices: []Choice{{Index: 0, Message: msg, FinishReason: FinishReason(res.StopReason)}},
		Usage:   UsageFrom(res.Usage),
	}
}

func argsOrEmpty(s string) string {
	if strings.TrimSpace(s) == "" {
		return "{}"
	}
	return s
}

// chunk is one streaming chat.completion.chunk.
type chunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []chunkChoice `json:"choices"`
	Usage   *Usage        `json:"usage,omitempty"`
}

type chunkChoice struct {
	Index        int     `json:"index"`
	Delta        delta   `json:"delta"`
	FinishReason *string `json:"finish_reason"`
}

type delta struct {
	Role      string          `json:"role,omitempty"`
	Content   *string         `json:"content,omitempty"`
	ToolCalls []toolCallDelta `json:"tool_calls,omitempty"`
}

type toolCallDelta struct {
	Index    int    `json:"index"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// Streamer writes a reply as OpenAI server-sent events.
type Streamer struct {
	w       io.Writer
	flush   func()
	id      string
	model   string
	created int64
	started bool
}

// NewStreamer returns a Streamer that flushes after every event.
func NewStreamer(w io.Writer, flush func(), id, model string, created int64) *Streamer {
	return &Streamer{w: w, flush: flush, id: id, model: model, created: created}
}

// Started reports whether any event was written.
func (s *Streamer) Started() bool { return s.started }

// Event writes one streamed piece of the reply. The first call also writes
// the assistant role chunk.
func (s *Streamer) Event(ev claudecli.Event) error {
	if !s.started {
		s.started = true
		if err := s.write(chunkChoice{Delta: delta{Role: "assistant"}}); err != nil {
			return err
		}
	}
	var d delta
	switch ev.Kind {
	case claudecli.TextDelta:
		text := ev.Text
		d.Content = &text
	case claudecli.ToolCallStart:
		tc := toolCallDelta{Index: ev.ToolIndex, ID: ev.ToolID, Type: "function"}
		tc.Function.Name = ev.ToolName
		d.ToolCalls = []toolCallDelta{tc}
	case claudecli.ToolCallArgs:
		tc := toolCallDelta{Index: ev.ToolIndex}
		tc.Function.Arguments = ev.Text
		d.ToolCalls = []toolCallDelta{tc}
	default:
		return nil
	}
	return s.write(chunkChoice{Delta: d})
}

// Finish writes the final chunk, an optional usage chunk, and [DONE].
func (s *Streamer) Finish(res claudecli.Result, includeUsage bool) error {
	if !s.started {
		s.started = true
		if err := s.write(chunkChoice{Delta: delta{Role: "assistant"}}); err != nil {
			return err
		}
	}
	reason := FinishReason(res.StopReason)
	if err := s.write(chunkChoice{FinishReason: &reason}); err != nil {
		return err
	}
	if includeUsage {
		u := UsageFrom(res.Usage)
		if err := s.writeChunk(chunk{ID: s.id, Object: "chat.completion.chunk", Created: s.created, Model: s.model, Choices: []chunkChoice{}, Usage: &u}); err != nil {
			return err
		}
	}
	return s.raw("data: [DONE]\n\n")
}

// Fail writes an error event after streaming has started; the HTTP status
// can no longer change.
func (s *Streamer) Fail(e *Error) error {
	b, err := json.Marshal(errorBody(e))
	if err != nil {
		return fmt.Errorf("encode error event: %w", err)
	}
	return s.raw("data: " + string(b) + "\n\n")
}

func (s *Streamer) write(c chunkChoice) error {
	return s.writeChunk(chunk{ID: s.id, Object: "chat.completion.chunk", Created: s.created, Model: s.model, Choices: []chunkChoice{c}})
}

func (s *Streamer) writeChunk(c chunk) error {
	b, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("encode chunk: %w", err)
	}
	return s.raw("data: " + string(b) + "\n\n")
}

func (s *Streamer) raw(line string) error {
	if _, err := io.WriteString(s.w, line); err != nil {
		return fmt.Errorf("write event: %w", err)
	}
	s.flush()
	return nil
}

type errorEnvelope struct {
	Error struct {
		Message string  `json:"message"`
		Type    string  `json:"type"`
		Code    *string `json:"code"`
	} `json:"error"`
}

func errorBody(e *Error) errorEnvelope {
	var env errorEnvelope
	env.Error.Message = e.Msg
	env.Error.Type = e.Type
	if e.Code != "" {
		code := e.Code
		env.Error.Code = &code
	}
	return env
}

// WriteError writes an OpenAI error response.
func WriteError(w http.ResponseWriter, e *Error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.Status)
	_ = json.NewEncoder(w).Encode(errorBody(e))
}
