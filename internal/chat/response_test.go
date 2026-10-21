package chat

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/thelonelyghost/llm-proxy-provider-claudecode/internal/claudecli"
)

func TestFinishReason(t *testing.T) {
	tests := map[string]string{
		"end_turn":                      "stop",
		"stop_sequence":                 "stop",
		"":                              "stop",
		"max_tokens":                    "length",
		"model_context_window_exceeded": "length",
		"refusal":                       "content_filter",
		"tool_use":                      "tool_calls",
	}
	for in, want := range tests {
		if got := FinishReason(in); got != want {
			t.Errorf("FinishReason(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNewIDIsUnique(t *testing.T) {
	a, b := NewID(), NewID()
	if a == b || !strings.HasPrefix(a, "chatcmpl-") || len(a) != len("chatcmpl-")+24 {
		t.Errorf("ids %q %q", a, b)
	}
}

var usage = claudecli.TokenUsage{InputTokens: 3, OutputTokens: 7, CacheCreationInputTokens: 5, CacheReadInputTokens: 10}

func TestAccumulatorText(t *testing.T) {
	var acc Accumulator
	acc.Add(claudecli.Event{Kind: claudecli.TextDelta, Text: "Hel"})
	acc.Add(claudecli.Event{Kind: claudecli.TextDelta, Text: "lo"})
	c := acc.Completion("id1", "haiku", 42, claudecli.Result{StopReason: "end_turn", Usage: usage})
	b, _ := json.Marshal(c)
	want := `{"id":"id1","object":"chat.completion","created":42,"model":"haiku","choices":[{"index":0,"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":18,"completion_tokens":7,"total_tokens":25}}`
	if string(b) != want {
		t.Errorf("completion =\n%s\nwant\n%s", b, want)
	}
}

func TestAccumulatorToolCalls(t *testing.T) {
	var acc Accumulator
	acc.Add(claudecli.Event{Kind: claudecli.ToolCallStart, ToolIndex: 0, ToolID: "t1", ToolName: "get_time"})
	acc.Add(claudecli.Event{Kind: claudecli.ToolCallArgs, ToolIndex: 0, Text: `{"tz":"UTC"}`})
	acc.Add(claudecli.Event{Kind: claudecli.ToolCallStart, ToolIndex: 1, ToolID: "t2", ToolName: "ping"})
	acc.Add(claudecli.Event{Kind: claudecli.ToolCallArgs, ToolIndex: 7, Text: "ignored"})
	c := acc.Completion("id1", "opus", 1, claudecli.Result{StopReason: "tool_use"})
	msg := c.Choices[0].Message
	if msg.Content != nil || c.Choices[0].FinishReason != "tool_calls" || len(msg.ToolCalls) != 2 {
		t.Fatalf("message = %+v", c.Choices[0])
	}
	if got := msg.ToolCalls[0]; got.ID != "t1" || got.Type != "function" || got.Function.Name != "get_time" || got.Function.Arguments != `{"tz":"UTC"}` {
		t.Errorf("call 0 = %+v", got)
	}
	if got := msg.ToolCalls[1].Function.Arguments; got != "{}" {
		t.Errorf("call 1 arguments = %q, want {}", got)
	}
}

// events splits an SSE body into its data payloads.
func events(t *testing.T, body string) []string {
	t.Helper()
	parts := strings.Split(strings.TrimSuffix(body, "\n\n"), "\n\n")
	out := make([]string, 0, len(parts))
	for _, ev := range parts {
		data, ok := strings.CutPrefix(ev, "data: ")
		if !ok {
			t.Fatalf("event %q has no data: prefix", ev)
		}
		out = append(out, data)
	}
	return out
}

func TestStreamerText(t *testing.T) {
	var buf bytes.Buffer
	flushes := 0
	st := NewStreamer(&buf, func() { flushes++ }, "id1", "haiku", 42)
	if st.Started() {
		t.Fatal("Started before any event")
	}
	_ = st.Event(claudecli.Event{Kind: claudecli.TextDelta, Text: "Hi"})
	_ = st.Event(claudecli.Event{Kind: 99})
	if err := st.Finish(claudecli.Result{StopReason: "end_turn", Usage: usage}, true); err != nil {
		t.Fatal(err)
	}
	got := events(t, buf.String())
	want := []string{
		`{"id":"id1","object":"chat.completion.chunk","created":42,"model":"haiku","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
		`{"id":"id1","object":"chat.completion.chunk","created":42,"model":"haiku","choices":[{"index":0,"delta":{"content":"Hi"},"finish_reason":null}]}`,
		`{"id":"id1","object":"chat.completion.chunk","created":42,"model":"haiku","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`{"id":"id1","object":"chat.completion.chunk","created":42,"model":"haiku","choices":[],"usage":{"prompt_tokens":18,"completion_tokens":7,"total_tokens":25}}`,
		`[DONE]`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("events =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if flushes != len(want) {
		t.Errorf("flushes = %d, want one per event (%d)", flushes, len(want))
	}
}

func TestStreamerToolCalls(t *testing.T) {
	var buf bytes.Buffer
	st := NewStreamer(&buf, func() {}, "id1", "opus", 1)
	_ = st.Event(claudecli.Event{Kind: claudecli.ToolCallStart, ToolIndex: 0, ToolID: "t1", ToolName: "get_time"})
	_ = st.Event(claudecli.Event{Kind: claudecli.ToolCallArgs, ToolIndex: 0, Text: `{"tz":`})
	_ = st.Finish(claudecli.Result{StopReason: "tool_use"}, false)
	got := events(t, buf.String())
	if len(got) != 5 {
		t.Fatalf("events = %v", got)
	}
	for i, want := range map[int]string{
		1: `"delta":{"tool_calls":[{"index":0,"id":"t1","type":"function","function":{"name":"get_time","arguments":""}}]}`,
		2: `"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"tz\":"}}]}`,
		3: `"finish_reason":"tool_calls"`,
	} {
		if !strings.Contains(got[i], want) {
			t.Errorf("event %d = %s, want %s", i, got[i], want)
		}
	}
}

func TestStreamerFinishWithoutEvents(t *testing.T) {
	var buf bytes.Buffer
	st := NewStreamer(&buf, func() {}, "id1", "m", 1)
	_ = st.Finish(claudecli.Result{StopReason: "end_turn"}, false)
	if got := events(t, buf.String()); len(got) != 3 || !strings.Contains(got[0], `"role":"assistant"`) {
		t.Errorf("events = %v", got)
	}
}

func TestStreamerFail(t *testing.T) {
	var buf bytes.Buffer
	st := NewStreamer(&buf, func() {}, "id1", "m", 1)
	_ = st.Fail(&Error{Status: 502, Type: "upstream_error", Msg: "boom"})
	if got := buf.String(); got != "data: {\"error\":{\"message\":\"boom\",\"type\":\"upstream_error\",\"code\":null}}\n\n" {
		t.Errorf("fail event = %q", got)
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("broken") }

func TestStreamerWriteErrors(t *testing.T) {
	st := NewStreamer(failWriter{}, func() {}, "id", "m", 1)
	if err := st.Event(claudecli.Event{Kind: claudecli.TextDelta, Text: "x"}); err == nil {
		t.Error("Event: want a write error")
	}
	st = NewStreamer(failWriter{}, func() {}, "id", "m", 1)
	if err := st.Finish(claudecli.Result{}, true); err == nil {
		t.Error("Finish: want a write error")
	}
}

func TestWriteError(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteError(rec, &Error{Status: 404, Type: "invalid_request_error", Code: "model_not_found", Msg: "no such model"})
	if rec.Code != 404 || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("status %d, headers %v", rec.Code, rec.Header())
	}
	want := `{"error":{"message":"no such model","type":"invalid_request_error","code":"model_not_found"}}` + "\n"
	if rec.Body.String() != want {
		t.Errorf("body = %q, want %q", rec.Body.String(), want)
	}
}
