package claudecli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fakeDriver returns a Driver that runs the test binary as a fake claude
// playing scenario, and the path of the record file it writes.
func fakeDriver(t *testing.T, scenario string, extraEnv ...string) (d *Driver, recordPath string) {
	t.Helper()
	dir := t.TempDir()
	rec := filepath.Join(dir, "record.json")
	env := append(os.Environ(),
		envScenario+"="+scenario,
		envRecord+"="+rec,
		"CLAUDE_CODE_EFFORT_LEVEL=max",
		"CLAUDECODE=1",
	)
	env = append(env, extraEnv...)
	work := filepath.Join(dir, "work")
	if err := os.Mkdir(work, 0o700); err != nil {
		t.Fatal(err)
	}
	return &Driver{Path: os.Args[0], Env: env, WorkDir: work}, rec
}

func readRecord(t *testing.T, path string) record {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("fake claude wrote no record: %v", err)
	}
	var r record
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

// ask is a conversation of one user message.
func ask(text string) []Turn {
	return []Turn{{Role: RoleUser, Text: text}}
}

func collect(events *[]Event) func(Event) error {
	return func(ev Event) error {
		*events = append(*events, ev)
		return nil
	}
}

func TestParseEffort(t *testing.T) {
	tests := []struct {
		in, want string
		wantErr  bool
	}{
		{in: "", want: ""},
		{in: "none", want: "low"},
		{in: "minimal", want: "low"},
		{in: "low", want: "low"},
		{in: "Medium", want: "medium"},
		{in: "high", want: "high"},
		{in: "xhigh", want: "xhigh"},
		{in: "max", want: "max"},
		{in: "ultra", wantErr: true},
	}
	for _, tt := range tests {
		got, err := ParseEffort(tt.in)
		if (err != nil) != tt.wantErr || got != tt.want {
			t.Errorf("ParseEffort(%q) = %q, %v; want %q, err %t", tt.in, got, err, tt.want, tt.wantErr)
		}
	}
}

func TestChildEnvDropsOverrides(t *testing.T) {
	got := childEnv([]string{"A=1", "CLAUDECODE=1", "CLAUDE_CODE_EFFORT_LEVEL=max", "CLAUDE_CODE_OTHER=x"})
	want := []string{"A=1", "CLAUDE_CODE_OTHER=x"}
	if !slices.Equal(got, want) {
		t.Errorf("childEnv = %v, want %v", got, want)
	}
}

func TestChatStreamsText(t *testing.T) {
	d, recPath := fakeDriver(t, "text")
	var events []Event
	res, err := d.Chat(context.Background(), Request{Model: "haiku", Effort: "low", SystemPrompt: "Be brief.", Turns: ask("hi")}, collect(&events))
	if err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	for _, ev := range events {
		if ev.Kind != TextDelta {
			t.Fatalf("unexpected event %+v", ev)
		}
		text.WriteString(ev.Text)
	}
	if text.String() != "Hello" {
		t.Errorf("text = %q, want Hello", text.String())
	}
	want := Result{StopReason: "end_turn", Usage: TokenUsage{InputTokens: 3, OutputTokens: 7, CacheCreationInputTokens: 5, CacheReadInputTokens: 10}}
	if res != want {
		t.Errorf("result = %+v, want %+v", res, want)
	}

	rec := readRecord(t, recPath)
	if rec.Prompt != "hi" || rec.SystemPrompt != "Be brief." {
		t.Errorf("prompt = %q, system prompt = %q", rec.Prompt, rec.SystemPrompt)
	}
	if rec.EffortEnv || rec.NestedEnv {
		t.Errorf("child saw CLAUDE_CODE_EFFORT_LEVEL=%t CLAUDECODE=%t; both must be removed", rec.EffortEnv, rec.NestedEnv)
	}
	if resolved, _ := filepath.EvalSymlinks(d.WorkDir); rec.Cwd != resolved && rec.Cwd != d.WorkDir {
		t.Errorf("cwd = %q, want %q", rec.Cwd, d.WorkDir)
	}
	for _, flag := range [][]string{{"-p"}, {"--tools", ""}, {"--setting-sources", ""}, {"--no-session-persistence"}, {"--strict-mcp-config"}, {"--disable-slash-commands"}, {"--permission-prompts", "none"}, {"--model", "haiku"}, {"--effort", "low"}} {
		if !containsSeq(rec.Args, flag) {
			t.Errorf("args %q missing %q", rec.Args, flag)
		}
	}
	if slices.Contains(rec.Args, "--mcp-config") {
		t.Errorf("args %q have --mcp-config without tools", rec.Args)
	}
	if entries, _ := os.ReadDir(d.WorkDir); len(entries) != 0 {
		t.Errorf("work dir not cleaned up: %v", entries)
	}
}

func containsSeq(args, seq []string) bool {
	for i := 0; i+len(seq) <= len(args); i++ {
		if slices.Equal(args[i:i+len(seq)], seq) {
			return true
		}
	}
	return false
}

func TestChatReplaysHistory(t *testing.T) {
	d, recPath := fakeDriver(t, "text")
	turns := []Turn{
		{Role: RoleUser, Text: "My name is Ada."},
		{Role: RoleAssistant, Text: "Checking.", ToolCalls: []ToolCall{{ID: "call_1", Name: "get_time", Input: json.RawMessage(`{"tz":"UTC"}`)}}},
		{Role: RoleUser, ToolResults: []ToolResult{{ToolCallID: "call_1", Content: "12:00"}}, Text: "And my name?"},
	}
	if _, err := d.Chat(context.Background(), Request{Model: "haiku", Turns: turns}, func(Event) error { return nil }); err != nil {
		t.Fatal(err)
	}
	rec := readRecord(t, recPath)
	got, _ := json.Marshal(rec.Frames)
	want := `[` +
		`{"client_composed":true,"message":{"content":[{"text":"My name is Ada.","type":"text"}],"role":"user"},"parent_tool_use_id":null,"session_id":"","shouldQuery":false,"type":"user"},` +
		`{"message":{"content":[{"text":"Checking.","type":"text"},{"id":"call_1","input":{"tz":"UTC"},"name":"mcp__c__get_time","type":"tool_use"}],"id":"msg_replay_1","role":"assistant","type":"message"},"parent_tool_use_id":null,"session_id":"","type":"assistant"},` +
		`{"client_composed":true,"message":{"content":[{"content":"12:00","tool_use_id":"call_1","type":"tool_result"},{"text":"And my name?","type":"text"}],"role":"user"},"parent_tool_use_id":null,"session_id":"","type":"user"}` +
		`]`
	if string(got) != want {
		t.Errorf("frames =\n%s\nwant\n%s", got, want)
	}
	if rec.Prompt != "And my name?" {
		t.Errorf("querying frame text = %q", rec.Prompt)
	}
}

func TestChatHistoryAckError(t *testing.T) {
	d, _ := fakeDriver(t, "ack_error")
	turns := []Turn{{Role: RoleUser, Text: "a"}, {Role: RoleAssistant, Text: "b"}, {Role: RoleUser, Text: "c"}}
	_, err := d.Chat(context.Background(), Request{Model: "haiku", Turns: turns}, func(Event) error { return nil })
	var ue *UpstreamError
	if !errors.As(err, &ue) || !strings.Contains(ue.Message, "replaying conversation history failed") {
		t.Fatalf("err = %v", err)
	}
}

func TestChatNeedsFinalUserTurn(t *testing.T) {
	d := &Driver{Path: os.Args[0], WorkDir: t.TempDir()}
	for _, turns := range [][]Turn{nil, {{Role: RoleUser, Text: "a"}, {Role: RoleAssistant, Text: "b"}}} {
		if _, err := d.Chat(context.Background(), Request{Turns: turns}, func(Event) error { return nil }); err == nil {
			t.Errorf("turns %+v: want an error", turns)
		}
	}
}

func TestChatScenarios(t *testing.T) {
	tests := []struct {
		scenario string
		wantText string
		wantStop string
	}{
		{scenario: "subagent", wantText: "Hello", wantStop: "end_turn"},
		{scenario: "notjson", wantText: "Hello", wantStop: "end_turn"},
		{scenario: "unknown_control", wantText: "Hello", wantStop: "end_turn"},
		{scenario: "no_result_stop", wantText: "Hello", wantStop: "end_turn"},
	}
	for _, tt := range tests {
		t.Run(tt.scenario, func(t *testing.T) {
			d, _ := fakeDriver(t, tt.scenario)
			var events []Event
			res, err := d.Chat(context.Background(), Request{Model: "haiku", Turns: ask("hi")}, collect(&events))
			if err != nil {
				t.Fatal(err)
			}
			var text strings.Builder
			for _, ev := range events {
				text.WriteString(ev.Text)
			}
			if text.String() != tt.wantText || res.StopReason != tt.wantStop {
				t.Errorf("text %q stop %q; want %q %q", text.String(), res.StopReason, tt.wantText, tt.wantStop)
			}
		})
	}
}

func TestChatStopsAtToolCalls(t *testing.T) {
	d, recPath := fakeDriver(t, "tool")
	tools := []Tool{
		{Name: "get_time", Description: "time", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "get_date", Description: "date", InputSchema: json.RawMessage(`{"type":"object"}`)},
	}
	var events []Event
	res, err := d.Chat(context.Background(), Request{Model: "opus", Turns: ask("time?"), Tools: tools}, collect(&events))
	if err != nil {
		t.Fatal(err)
	}
	want := []Event{
		{Kind: TextDelta, Text: "Checking."},
		{Kind: ToolCallStart, ToolIndex: 0, ToolID: "toolu_1", ToolName: "get_time"},
		{Kind: ToolCallArgs, ToolIndex: 0, Text: `{"tz":`},
		{Kind: ToolCallArgs, ToolIndex: 0, Text: `"UTC"}`},
		{Kind: ToolCallStart, ToolIndex: 1, ToolID: "toolu_2", ToolName: "get_date"},
	}
	if !slices.Equal(events, want) {
		t.Errorf("events =\n%+v\nwant\n%+v", events, want)
	}
	if res.StopReason != "tool_use" || res.Usage.OutputTokens != 7 {
		t.Errorf("result = %+v", res)
	}
	rec := readRecord(t, recPath)
	if !slices.Equal(rec.Tools, []string{"get_time", "get_date"}) {
		t.Errorf("bridge listed %v", rec.Tools)
	}
	if !containsSeq(rec.Args, []string{"--mcp-config", bridgeConfig}) {
		t.Errorf("args %q missing the bridge --mcp-config", rec.Args)
	}
}

func TestChatErrors(t *testing.T) {
	tests := []struct {
		scenario string
		wantCode string
		wantMsg  string
	}{
		{scenario: "error", wantCode: "rate_limit", wantMsg: "synthetic rate limit"},
		{scenario: "error_bare", wantMsg: "first; second"},
		{scenario: "error_subtype", wantMsg: "error_during_execution"},
		{scenario: "init_error", wantMsg: "synthetic init failure"},
		{scenario: "exit", wantMsg: "fatal: synthetic failure"},
		{scenario: "truncated", wantMsg: "synthetic crash"},
		{scenario: "silent_exit", wantMsg: "claude exited before completing the request"},
	}
	for _, tt := range tests {
		t.Run(tt.scenario, func(t *testing.T) {
			d, _ := fakeDriver(t, tt.scenario)
			_, err := d.Chat(context.Background(), Request{Model: "haiku", Turns: ask("hi")}, func(Event) error { return nil })
			var ue *UpstreamError
			if !errors.As(err, &ue) {
				t.Fatalf("err = %v, want *UpstreamError", err)
			}
			if ue.Code != tt.wantCode || !strings.Contains(ue.Message, tt.wantMsg) {
				t.Errorf("err = %+v, want code %q message containing %q", ue, tt.wantCode, tt.wantMsg)
			}
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("Error() = %q", err.Error())
			}
		})
	}
}

func TestChatEmitErrorStops(t *testing.T) {
	d, _ := fakeDriver(t, "hang")
	stop := errors.New("client went away")
	_, err := d.Chat(context.Background(), Request{Model: "haiku", Turns: ask("hi")}, func(Event) error { return stop })
	if !errors.Is(err, stop) {
		t.Fatalf("err = %v, want %v", err, stop)
	}
}

func TestChatContextCancelKillsCLI(t *testing.T) {
	d, _ := fakeDriver(t, "hang")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := d.Chat(ctx, Request{Model: "haiku", Turns: ask("hi")}, func(Event) error {
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestCLINotFound(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-claude")
	d := &Driver{Path: missing, WorkDir: t.TempDir()}
	ctx := context.Background()
	_, chatErr := d.Chat(ctx, Request{Model: "haiku", Turns: ask("hi")}, func(Event) error { return nil })
	_, catErr := d.Catalogue(ctx)
	_, usageErr := d.Usage(ctx)
	_, authErr := d.AuthStatus(ctx)
	loginErr := d.Login(ctx, nil, &bytes.Buffer{}, &bytes.Buffer{}, nil)
	for name, err := range map[string]error{"chat": chatErr, "catalogue": catErr, "usage": usageErr, "auth": authErr, "login": loginErr} {
		if !errors.Is(err, ErrCLINotFound) {
			t.Errorf("%s: err = %v, want ErrCLINotFound", name, err)
		}
	}
}

func TestChatNeedsWorkDir(t *testing.T) {
	d := &Driver{Path: os.Args[0], WorkDir: filepath.Join(t.TempDir(), "missing")}
	if _, err := d.Chat(context.Background(), Request{Turns: ask("hi")}, func(Event) error { return nil }); err == nil {
		t.Fatal("want an error for a missing work dir")
	}
}

func TestDefaultPath(t *testing.T) {
	if got := (&Driver{}).path(); got != DefaultPath {
		t.Errorf("path = %q, want %q", got, DefaultPath)
	}
}

func TestCatalogue(t *testing.T) {
	d, recPath := fakeDriver(t, "text")
	models, err := d.Catalogue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 3 || models[0].Value != "default" || models[0].ResolvedModel != "claude-opus-5-5" || len(models[0].SupportedEffortLevels) != 5 {
		t.Errorf("models = %+v", models)
	}
	want := []string{"default", "opus", "haiku", "claude-opus-5-5", "claude-haiku-5-5"}
	if got := ModelIDs(models); !slices.Equal(got, want) {
		t.Errorf("ModelIDs = %v, want %v", got, want)
	}
	if rec := readRecord(t, recPath); slices.Contains(rec.Args, "--model") || rec.Prompt != "" {
		t.Errorf("catalogue must send no message: args %q prompt %q", rec.Args, rec.Prompt)
	}
}

func TestModelIDsSkipsEmpty(t *testing.T) {
	got := ModelIDs([]Model{{Value: "a"}, {Value: ""}, {ResolvedModel: "a"}})
	if !slices.Equal(got, []string{"a"}) {
		t.Errorf("ModelIDs = %v", got)
	}
}

func TestCatalogueErrors(t *testing.T) {
	for _, scenario := range []string{"init_error", "unknown_control"} {
		t.Run(scenario, func(t *testing.T) {
			d, _ := fakeDriver(t, scenario)
			_, err := d.Catalogue(context.Background())
			if scenario == "init_error" && err == nil {
				t.Fatal("want an error")
			}
			if scenario == "unknown_control" && err != nil {
				t.Fatalf("unknown control requests must be refused, not fatal: %v", err)
			}
		})
	}
}

func TestUsage(t *testing.T) {
	d, _ := fakeDriver(t, "text")
	u, err := d.Usage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if u.SubscriptionType != "max" || !u.RateLimitsAvailable || u.RateLimits == nil || u.RateLimits.ExtraUsage == nil {
		t.Fatalf("usage = %+v", u)
	}
	x := u.RateLimits.ExtraUsage
	if !x.IsEnabled || x.MonthlyLimit != 5000 || x.UsedCredits != 1250 || x.Currency != "USD" || x.DecimalPlaces != 2 {
		t.Errorf("extra usage = %+v", x)
	}
	if len(u.RateLimits.Limits) != 1 || u.RateLimits.Limits[0].Percent != 42 {
		t.Errorf("limits = %+v", u.RateLimits.Limits)
	}
}

func TestUsageErrors(t *testing.T) {
	for _, scenario := range []string{"usage_error", "init_error"} {
		t.Run(scenario, func(t *testing.T) {
			d, _ := fakeDriver(t, scenario)
			if _, err := d.Usage(context.Background()); err == nil {
				t.Fatal("want an error")
			}
		})
	}
}

func TestAuthStatus(t *testing.T) {
	tests := []struct {
		scenario string
		want     AuthStatus
		wantErr  bool
	}{
		{scenario: "auth_in", want: AuthStatus{LoggedIn: true, AuthMethod: "claude.ai", APIProvider: "firstParty", SubscriptionType: "max"}},
		{scenario: "auth_out", want: AuthStatus{AuthMethod: "none", APIProvider: "firstParty"}},
		{scenario: "auth_garbage", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.scenario, func(t *testing.T) {
			d, _ := fakeDriver(t, tt.scenario)
			got, err := d.AuthStatus(context.Background())
			if (err != nil) != tt.wantErr || got != tt.want {
				t.Errorf("AuthStatus = %+v, %v; want %+v, err %t", got, err, tt.want, tt.wantErr)
			}
		})
	}
}

func TestLogin(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	d, _ := fakeDriver(t, "auth_out", envState+"="+state)
	var out, errOut bytes.Buffer
	if err := d.Login(context.Background(), strings.NewReader(""), &out, &errOut, []string{"--claudeai", "--sso"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "login args: --claudeai --sso") {
		t.Errorf("stdout = %q", out.String())
	}
	st, err := d.AuthStatus(context.Background())
	if err != nil || !st.LoggedIn {
		t.Errorf("after login: %+v, %v", st, err)
	}
}

func TestLoginFailure(t *testing.T) {
	d, _ := fakeDriver(t, "login_fail")
	var errOut bytes.Buffer
	err := d.Login(context.Background(), strings.NewReader(""), &bytes.Buffer{}, &errOut, nil)
	if err == nil || !strings.Contains(errOut.String(), "login aborted") {
		t.Fatalf("err = %v, stderr = %q", err, errOut.String())
	}
}

func TestToolBridge(t *testing.T) {
	if toolBridge(nil) != nil {
		t.Fatal("no tools must mean no bridge")
	}
	h := toolBridge([]Tool{{Name: "x", InputSchema: json.RawMessage(`{"type":"object"}`)}})
	call := json.RawMessage(`{"subtype":"mcp_message","server_name":"c","message":{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"x"}}}`)
	if resp, ok, err := h("mcp_message", call); err != nil || ok || resp != nil {
		t.Errorf("tools/call must get no reply: %v %t %v", resp, ok, err)
	}
	if _, _, err := h("can_use_tool", json.RawMessage(`{}`)); err == nil {
		t.Error("non-MCP control requests must be refused")
	}
	if _, _, err := h("mcp_message", json.RawMessage(`not json`)); err == nil {
		t.Error("undecodable requests must be refused")
	}
}

func TestTailBufferKeepsEnd(t *testing.T) {
	tb := &tailBuffer{max: 4}
	_, _ = tb.Write([]byte("abc"))
	_, _ = tb.Write([]byte("defg"))
	if got := tb.String(); got != "defg" {
		t.Errorf("tail = %q", got)
	}
}
