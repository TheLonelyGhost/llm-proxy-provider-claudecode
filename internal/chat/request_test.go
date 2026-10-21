package chat

import (
	"encoding/json"
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
	if got.Model != "opus" || got.Effort != "low" || got.SystemPrompt != "Be brief.\n\nUse metric." || got.Tools != nil {
		t.Errorf("Translate = %+v", got)
	}
	assertTurns(t, got.Turns, `[{"Role":"user","Content":[{"text":"line one\nline two","type":"text"}],"ToolCalls":null,"ToolResults":null}]`)
}

// assertTurns compares turns by their JSON form.
func assertTurns(t *testing.T, got []claudecli.Turn, want string) {
	t.Helper()
	b, _ := json.Marshal(got)
	if string(b) != want {
		t.Errorf("turns =\n%s\nwant\n%s", b, want)
	}
}

func TestTranslateDefaultEffort(t *testing.T) {
	req := mustDecode(t, `{"model":"opus","messages":[{"role":"user","content":"hi"}]}`)
	got, err := Translate(req, "max")
	if err != nil || got.Effort != "max" {
		t.Fatalf("effort = %q, %v; want max", got.Effort, err)
	}
}

func TestTranslateBuildsTurnsWithToolResults(t *testing.T) {
	req := mustDecode(t, `{"model":"opus","tools":[{"type":"function","function":{"name":"get_time","description":"Get the time","parameters":{"type":"object","properties":{"tz":{"type":"string"}}}}}],
	"messages":[
		{"role":"user","content":"What time is it?"},
		{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_time","arguments":"{\"tz\":\"UTC\"}"}},{"id":"call_2","type":"function","function":{"name":"get_time","arguments":""}}]},
		{"role":"tool","tool_call_id":"call_1","content":"12:00"},
		{"role":"tool","tool_call_id":"call_2","content":"12:00"},
		{"role":"user","content":"Thanks. And in words?"}]}`)
	got, err := Translate(req, "")
	if err != nil {
		t.Fatal(err)
	}
	assertTurns(t, got.Turns, `[`+
		`{"Role":"user","Content":[{"text":"What time is it?","type":"text"}],"ToolCalls":null,"ToolResults":null},`+
		`{"Role":"assistant","Content":null,"ToolCalls":[{"ID":"call_1","Name":"get_time","Input":{"tz":"UTC"}},{"ID":"call_2","Name":"get_time","Input":{}}],"ToolResults":null},`+
		`{"Role":"user","Content":[{"text":"Thanks. And in words?","type":"text"}],"ToolCalls":null,"ToolResults":[{"ToolCallID":"call_1","Content":[{"text":"12:00","type":"text"}]},{"ToolCallID":"call_2","Content":[{"text":"12:00","type":"text"}]}]}]`)
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
		{name: "bad content", body: `{"model":"m","messages":[{"role":"user","content":42}]}`, want: "string or an array"},
		{name: "bad system content", body: `{"model":"m","messages":[{"role":"system","content":42},` + user + `]}`, want: "string or an array"},
		{name: "bad history content", body: `{"model":"m","messages":[{"role":"assistant","content":42},` + user + `]}`, want: "string or an array"},
		{name: "tool without id", body: `{"model":"m","messages":[` + user + `,{"role":"tool","content":"x"}]}`, want: "no tool_call_id"},
		{name: "tool unknown id", body: `{"model":"m","messages":[` + user + `,{"role":"tool","tool_call_id":"nope","content":"x"}]}`, want: "unknown tool_call_id"},
		{name: "bad tool args", body: `{"model":"m","messages":[` + user + `,{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"[1]"}}]},{"role":"tool","tool_call_id":"c1","content":"x"}]}`, want: "arguments must be a JSON object"},
		{name: "tool call without id", body: `{"model":"m","messages":[` + user + `,{"role":"assistant","tool_calls":[{"type":"function","function":{"name":"f","arguments":"{}"}}]},` + user + `]}`, want: "needs an id"},
		{name: "tool result after user text", body: `{"model":"m","messages":[` + user + `,{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]},` + user + `,{"role":"tool","tool_call_id":"c1","content":"x"}]}`, want: "right after the assistant tool call"},
		{name: "empty user", body: `{"model":"m","messages":[{"role":"user","content":""}]}`, want: "has no content"},
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

func TestTranslateKeepsSlashTextVerbatim(t *testing.T) {
	req := mustDecode(t, `{"model":"m","messages":[{"role":"user","content":"/usage"}]}`)
	got, err := Translate(req, "")
	if err != nil {
		t.Fatal(err)
	}
	assertTurns(t, got.Turns, `[{"Role":"user","Content":[{"text":"/usage","type":"text"}],"ToolCalls":null,"ToolResults":null}]`)
}

func TestTranslateHistoryTurns(t *testing.T) {
	req := mustDecode(t, `{"model":"m","messages":[{"role":"user","content":"a"},{"role":"user","content":"a2"},{"role":"assistant","content":"b"},{"role":"assistant","content":""},{"role":"user","content":"c"}]}`)
	got, err := Translate(req, "")
	if err != nil {
		t.Fatal(err)
	}
	assertTurns(t, got.Turns, `[`+
		`{"Role":"user","Content":[{"text":"a\n\na2","type":"text"}],"ToolCalls":null,"ToolResults":null},`+
		`{"Role":"assistant","Content":[{"text":"b","type":"text"}],"ToolCalls":null,"ToolResults":null},`+
		`{"Role":"user","Content":[{"text":"c","type":"text"}],"ToolCalls":null,"ToolResults":null}]`)
}

func TestTranslateImagesAndFiles(t *testing.T) {
	req := mustDecode(t, `{"model":"m","messages":[
		{"role":"user","content":"Look:"},
		{"role":"user","content":[
			{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgo=","detail":"high"}},
			{"type":"text","text":"and"},{"type":"text","text":"this"},
			{"type":"image_url","image_url":{"url":"https://example.invalid/a.png"}}]},
		{"role":"user","content":[{"type":"file","file":{"filename":"a.pdf","file_data":"data:application/pdf;base64,JVBERi0xLjQ="}}]},
		{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"shot","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"call_1","content":[{"type":"text","text":"shot"},{"type":"image_url","image_url":{"url":"data:IMAGE/JPEG;base64,/9j/"}}]}]}`)
	got, err := Translate(req, "")
	if err != nil {
		t.Fatal(err)
	}
	assertTurns(t, got.Turns, `[`+
		`{"Role":"user","Content":[{"text":"Look:","type":"text"},`+
		`{"source":{"data":"iVBORw0KGgo=","media_type":"image/png","type":"base64"},"type":"image"},`+
		`{"text":"and\nthis","type":"text"},`+
		`{"source":{"type":"url","url":"https://example.invalid/a.png"},"type":"image"},`+
		`{"source":{"data":"JVBERi0xLjQ=","media_type":"application/pdf","type":"base64"},"type":"document"}],"ToolCalls":null,"ToolResults":null},`+
		`{"Role":"assistant","Content":null,"ToolCalls":[{"ID":"call_1","Name":"shot","Input":{}}],"ToolResults":null},`+
		`{"Role":"user","Content":null,"ToolCalls":null,"ToolResults":[{"ToolCallID":"call_1","Content":[{"text":"shot","type":"text"},`+
		`{"source":{"data":"/9j/","media_type":"image/jpeg","type":"base64"},"type":"image"}]}]}]`)
}

func TestTranslateRejectsMedia(t *testing.T) {
	user := `{"role":"user","content":"hi"}`
	part := func(p string) string { return `{"model":"m","messages":[{"role":"user","content":[` + p + `]}]}` }
	image := func(url string) string { return part(`{"type":"image_url","image_url":{"url":"` + url + `"}}`) }
	tests := []struct {
		name, body, want string
	}{
		{name: "image in system", body: `{"model":"m","messages":[{"role":"system","content":[{"type":"image_url","image_url":{"url":"https://example.invalid/a.png"}}]},` + user + `]}`, want: `"image_url" is not supported here; only text`},
		{name: "image in assistant", body: `{"model":"m","messages":[` + user + `,{"role":"assistant","content":[{"type":"image_url","image_url":{"url":"https://example.invalid/a.png"}}]},` + user + `]}`, want: `"image_url" is not supported here; only text`},
		{name: "no image_url", body: part(`{"type":"image_url"}`), want: "has no image_url"},
		{name: "http url", body: image("http://example.invalid/a.png"), want: "not a data URL"},
		{name: "not base64", body: image("data:image/png,abc"), want: "must be base64 encoded"},
		{name: "no data", body: image("data:image/png;base64,"), want: "must be base64 encoded"},
		{name: "image type", body: image("data:image/bmp;base64,AAAA"), want: `media type "image/bmp" is not supported (accepted: image/gif, image/jpeg, image/png, image/webp)`},
		{name: "bad base64", body: image("data:image/png;base64,!!!!"), want: "invalid base64"},
		{name: "no file", body: part(`{"type":"file"}`), want: "has no file"},
		{name: "file_id", body: part(`{"type":"file","file":{"file_id":"file-1"}}`), want: "file_id is not supported"},
		{name: "file type", body: part(`{"type":"file","file":{"file_data":"data:text/plain;base64,aGk="}}`), want: `media type "text/plain" is not supported (accepted: application/pdf)`},
		{name: "audio", body: part(`{"type":"input_audio","input_audio":{"data":"AAAA","format":"wav"}}`), want: "audio input is not supported"},
		{name: "unknown part", body: part(`{"type":"video"}`), want: `content part type "video" is not supported`},
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

// FuzzChatRequest checks that decoding and translating never panic and
// that every rejection is a client error.
func FuzzChatRequest(f *testing.F) {
	f.Add(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	f.Add(`{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"a"}]},{"role":"assistant","tool_calls":[{"id":"1","function":{"name":"f","arguments":"{}"}}]},{"role":"tool","tool_call_id":"1","content":"r"}]}`)
	f.Add(`{"model":"m","tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}],"tool_choice":"auto","messages":[{"role":"user","content":"x"}]}`)
	f.Add(`{"model":"m","reasoning_effort":"max","messages":[{"role":"system","content":"s"},{"role":"user","content":"x"}]}`)
	f.Add(`{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}},{"type":"file","file":{"file_data":"data:application/pdf;base64,AAAA"}}]}]}`)
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
