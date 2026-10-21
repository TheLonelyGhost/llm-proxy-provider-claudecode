package chat

import (
	"errors"
	"strings"
	"testing"

	"github.com/thelonelyghost/llm-proxy-provider-claudecode/internal/claudecli"
)

func TestDecodeRejects(t *testing.T) {
	tests := []struct {
		name, body, want string
	}{
		{name: "bad json", body: `{`, want: "invalid JSON body"},
		{name: "no model", body: `{"messages":[{"role":"user","content":"hi"}]}`, want: "model is required"},
		{name: "no messages", body: `{"model":"m","messages":[]}`, want: "messages must not be empty"},
		{name: "n", body: `{"model":"m","n":2,"messages":[{"role":"user","content":"hi"}]}`, want: "n must be 1"},
		{name: "functions", body: `{"model":"m","functions":[{"name":"f"}],"messages":[{"role":"user","content":"hi"}]}`, want: "functions is not supported"},
		{name: "json mode", body: `{"model":"m","response_format":{"type":"json_object"},"messages":[{"role":"user","content":"hi"}]}`, want: `response_format "json_object"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Decode([]byte(tt.body))
			var ce *Error
			if !errors.As(err, &ce) || ce.Status != 400 || ce.Type != "invalid_request_error" || !strings.Contains(ce.Error(), tt.want) {
				t.Fatalf("err = %v, want 400 containing %q", err, tt.want)
			}
		})
	}
}

func TestDecodeAccepts(t *testing.T) {
	body := `{"model":"haiku","n":1,"functions":null,"response_format":{"type":"text"},"stream":true,
		"stream_options":{"include_usage":true},"temperature":0.2,"messages":[{"role":"user","content":"hi"}]}`
	req, err := Decode([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if req.Model != "haiku" || !req.Stream || req.StreamOptions == nil || !req.StreamOptions.IncludeUsage {
		t.Errorf("req = %+v", req)
	}
}

func mustDecode(t *testing.T, body string) *Request {
	t.Helper()
	req, err := Decode([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func TestTranslateSingleTurn(t *testing.T) {
	req := mustDecode(t, `{"model":"opus","reasoning_effort":"minimal","messages":[
		{"role":"system","content":"Be brief."},
		{"role":"developer","content":[{"type":"text","text":"Use metric."}]},
		{"role":"system","content":""},
		{"role":"user","content":[{"type":"text","text":"line one"},{"type":"text","text":"line two"}]}]}`)
	got, err := Translate(req, "high")
	if err != nil {
		t.Fatal(err)
	}
	want := claudecli.Request{Model: "opus", Effort: "low", SystemPrompt: "Be brief.\n\nUse metric.", Prompt: "line one\nline two"}
	if got.Model != want.Model || got.Effort != want.Effort || got.SystemPrompt != want.SystemPrompt || got.Prompt != want.Prompt || got.Tools != nil {
		t.Errorf("Translate = %+v, want %+v", got, want)
	}
}

func TestTranslateDefaultEffort(t *testing.T) {
	req := mustDecode(t, `{"model":"opus","messages":[{"role":"user","content":"hi"}]}`)
	got, err := Translate(req, "max")
	if err != nil || got.Effort != "max" {
		t.Fatalf("effort = %q, %v; want max", got.Effort, err)
	}
}

func TestTranslateRendersHistoryWithToolResults(t *testing.T) {
	req := mustDecode(t, `{"model":"opus","tools":[{"type":"function","function":{"name":"get_time","description":"Get the time","parameters":{"type":"object","properties":{"tz":{"type":"string"}}}}}],
	"messages":[
		{"role":"user","content":"What time is it?"},
		{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_time","arguments":"{\"tz\":\"UTC\"}"}}]},
		{"role":"tool","tool_call_id":"call_1","content":"12:00"}]}`)
	got, err := Translate(req, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		transcriptPreamble,
		"<user>\nWhat time is it?\n</user>",
		`[called tool get_time (call id call_1) with arguments {"tz":"UTC"}]`,
		"<tool_result name=\"get_time\" call_id=\"call_1\">\n12:00\n</tool_result>",
	} {
		if !strings.Contains(got.Prompt, want) {
			t.Errorf("prompt missing %q:\n%s", want, got.Prompt)
		}
	}
	if len(got.Tools) != 1 || got.Tools[0].Name != "get_time" || got.Tools[0].Description != "Get the time" {
		t.Fatalf("tools = %+v", got.Tools)
	}
}

func TestTranslateToolDefaults(t *testing.T) {
	req := mustDecode(t, `{"model":"m","tool_choice":"auto","tools":[{"type":"function","function":{"name":"ping"}}],"messages":[{"role":"user","content":"hi"}]}`)
	got, err := Translate(req, "")
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Tools[0].InputSchema) != string(emptySchema) {
		t.Errorf("schema = %s, want the empty object schema", got.Tools[0].InputSchema)
	}
}

func TestTranslateToolChoiceNoneDropsTools(t *testing.T) {
	req := mustDecode(t, `{"model":"m","tool_choice":"none","tools":[{"type":"function","function":{"name":"ping"}}],"messages":[{"role":"user","content":"hi"}]}`)
	got, err := Translate(req, "")
	if err != nil || got.Tools != nil {
		t.Fatalf("tools = %+v, %v; want none", got.Tools, err)
	}
}

func TestTranslateRejects(t *testing.T) {
	user := `{"role":"user","content":"hi"}`
	tests := []struct {
		name, body, want string
	}{
		{name: "effort", body: `{"model":"m","reasoning_effort":"turbo","messages":[` + user + `]}`, want: "unsupported reasoning effort"},
		{name: "tool_choice required", body: `{"model":"m","tool_choice":"required","messages":[` + user + `]}`, want: `tool_choice "required"`},
		{name: "tool_choice function", body: `{"model":"m","tool_choice":{"type":"function","function":{"name":"x"}},"messages":[` + user + `]}`, want: "naming a function"},
		{name: "tool type", body: `{"model":"m","tools":[{"type":"code"}],"messages":[` + user + `]}`, want: `unsupported type "code"`},
		{name: "tool name", body: `{"model":"m","tools":[{"type":"function","function":{"name":"bad name"}}],"messages":[` + user + `]}`, want: "function name"},
		{name: "dup tool", body: `{"model":"m","tools":[{"type":"function","function":{"name":"a"}},{"type":"function","function":{"name":"a"}}],"messages":[` + user + `]}`, want: "duplicate"},
		{name: "schema not object", body: `{"model":"m","tools":[{"type":"function","function":{"name":"a","parameters":[1]}}],"messages":[` + user + `]}`, want: "JSON Schema object"},
		{name: "schema type", body: `{"model":"m","tools":[{"type":"function","function":{"name":"a","parameters":{"type":"string"}}}],"messages":[` + user + `]}`, want: `must have type "object"`},
		{name: "role", body: `{"model":"m","messages":[{"role":"function","content":"x"}]}`, want: `unsupported role "function"`},
		{name: "only system", body: `{"model":"m","messages":[{"role":"system","content":"x"}]}`, want: "must include a user message"},
		{name: "ends with assistant", body: `{"model":"m","messages":[` + user + `,{"role":"assistant","content":"x"}]}`, want: "last message"},
		{name: "image", body: `{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:,"}}]}]}`, want: "only text"},
		{name: "bad content", body: `{"model":"m","messages":[{"role":"user","content":42}]}`, want: "string or an array"},
		{name: "bad system content", body: `{"model":"m","messages":[{"role":"system","content":42},` + user + `]}`, want: "string or an array"},
		{name: "bad history content", body: `{"model":"m","messages":[{"role":"assistant","content":42},` + user + `]}`, want: "string or an array"},
		{name: "tool without id", body: `{"model":"m","messages":[` + user + `,{"role":"tool","content":"x"}]}`, want: "no tool_call_id"},
		{name: "tool unknown id", body: `{"model":"m","messages":[` + user + `,{"role":"tool","tool_call_id":"nope","content":"x"}]}`, want: "unknown tool_call_id"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Translate(mustDecode(t, tt.body), "")
			var ce *Error
			if !errors.As(err, &ce) || ce.Status != 400 || !strings.Contains(ce.Msg, tt.want) {
				t.Fatalf("err = %v, want 400 containing %q", err, tt.want)
			}
		})
	}
}

func TestTranslateSlashPromptIsNeverACommand(t *testing.T) {
	for _, content := range []string{"/usage", "  /logout"} {
		req := mustDecode(t, `{"model":"m","messages":[{"role":"user","content":"`+content+`"}]}`)
		got, err := Translate(req, "")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(got.Prompt, transcriptPreamble) || !strings.Contains(got.Prompt, "<user>\n"+content+"\n</user>") {
			t.Errorf("prompt for %q = %q, want the transcript form", content, got.Prompt)
		}
	}
}

func TestTranslateAssistantTextInHistory(t *testing.T) {
	req := mustDecode(t, `{"model":"m","messages":[{"role":"user","content":"a"},{"role":"assistant","content":"b"},{"role":"user","content":"c"}]}`)
	got, err := Translate(req, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Prompt, "<assistant>\nb\n</assistant>\n<user>\nc\n</user>") {
		t.Errorf("prompt = %q", got.Prompt)
	}
}

// FuzzChatRequest checks that decoding and translating never panic and
// that every rejection is a client error.
func FuzzChatRequest(f *testing.F) {
	f.Add(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	f.Add(`{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"a"}]},{"role":"assistant","tool_calls":[{"id":"1","function":{"name":"f","arguments":"{}"}}]},{"role":"tool","tool_call_id":"1","content":"r"}]}`)
	f.Add(`{"model":"m","tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}],"tool_choice":"auto","messages":[{"role":"user","content":"x"}]}`)
	f.Add(`{"model":"m","reasoning_effort":"max","messages":[{"role":"system","content":"s"},{"role":"user","content":"x"}]}`)
	f.Add(`[]`)
	f.Fuzz(func(t *testing.T, body string) {
		req, err := Decode([]byte(body))
		if err != nil {
			var ce *Error
			if !errors.As(err, &ce) || ce.Status != 400 {
				t.Fatalf("Decode error %v is not a 400", err)
			}
			return
		}
		if _, err := Translate(req, ""); err != nil {
			var ce *Error
			if !errors.As(err, &ce) || ce.Status != 400 {
				t.Fatalf("Translate error %v is not a 400", err)
			}
		}
	})
}
