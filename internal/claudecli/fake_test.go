package claudecli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"testing"
)

// The test binary doubles as a fake `claude`: when FAKE_CLAUDE names a
// scenario, TestMain speaks the stream-json protocol instead of running
// tests. All fixture data is synthetic.

const (
	envScenario = "FAKE_CLAUDE"
	envRecord   = "FAKE_CLAUDE_RECORD"
	envState    = "FAKE_CLAUDE_STATE"
)

func TestMain(m *testing.M) {
	if scenario := os.Getenv(envScenario); scenario != "" {
		os.Exit(fakeClaude(scenario, os.Args[1:]))
	}
	os.Exit(m.Run())
}

// record is what the fake writes to FAKE_CLAUDE_RECORD.
type record struct {
	Args         []string `json:"args"`
	Cwd          string   `json:"cwd"`
	EffortEnv    bool     `json:"effort_env"`
	NestedEnv    bool     `json:"nested_env"`
	SystemPrompt string   `json:"system_prompt"`
	Prompt       string   `json:"prompt"`
	Tools        []string `json:"tools"`
}

var fakeModels = []map[string]any{
	{"value": "default", "resolvedModel": "claude-opus-5-5", "displayName": "Default", "supportedEffortLevels": []string{"low", "medium", "high", "xhigh", "max"}},
	{"value": "opus", "resolvedModel": "claude-opus-5-5", "displayName": "Opus"},
	{"value": "haiku", "resolvedModel": "claude-haiku-5-5", "displayName": "Haiku"},
}

var fakeUsage = map[string]any{
	"subscription_type":     "max",
	"rate_limits_available": true,
	"rate_limits": map[string]any{
		"extra_usage": map[string]any{"is_enabled": true, "monthly_limit": 5000, "used_credits": 1250, "currency": "USD", "decimal_places": 2},
		"limits": []map[string]any{
			{"kind": "session", "group": "session", "percent": 42, "resets_at": "2030-01-01T00:00:00Z", "is_active": true},
		},
	},
}

func fakeClaude(scenario string, args []string) int {
	rec := record{Args: args, EffortEnv: os.Getenv("CLAUDE_CODE_EFFORT_LEVEL") != "", NestedEnv: os.Getenv("CLAUDECODE") != ""}
	rec.Cwd, _ = os.Getwd()
	for i, a := range args {
		if a == "--system-prompt-file" && i+1 < len(args) {
			b, _ := os.ReadFile(args[i+1])
			rec.SystemPrompt = string(b)
		}
	}
	writeRecord(&rec)
	defer writeRecord(&rec)

	if len(args) > 0 && args[0] == "auth" {
		return fakeAuth(scenario, args[1:])
	}
	if scenario == "exit" {
		fmt.Fprint(os.Stderr, "fatal: synthetic failure")
		return 3
	}

	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 0, 64<<10), 1<<20)
	read := func() map[string]any {
		for in.Scan() {
			var m map[string]any
			if json.Unmarshal(in.Bytes(), &m) == nil {
				return m
			}
		}
		return nil
	}
	for {
		m := read()
		if m == nil {
			return 0
		}
		switch m["type"] {
		case "control_request":
			req, _ := m["request"].(map[string]any)
			id := m["request_id"]
			switch req["subtype"] {
			case "initialize":
				if scenario == "init_error" {
					emit(ctlResponse(id, nil, "synthetic init failure"))
					continue
				}
				if scenario == "tool" {
					emit(mcpRequest("m0", 0, "initialize"))
					if r := read(); mcpResult(r) == nil {
						return fail("bad MCP initialize reply: %v", r)
					}
				}
				if scenario == "unknown_control" {
					emit(map[string]any{"type": "control_request", "request_id": "p1", "request": map[string]any{"subtype": "can_use_tool", "tool_name": "X"}})
					r := read()
					resp, _ := r["response"].(map[string]any)
					if resp["subtype"] != "error" {
						return fail("want an error reply to can_use_tool, got %v", r)
					}
				}
				if scenario == "notjson" {
					fmt.Println("this line is not JSON")
				}
				emit(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": "someone-else"}})
				emit(ctlResponse(id, map[string]any{"models": fakeModels, "account": map[string]any{"email": "user@example.invalid"}}, ""))
			case "get_usage":
				if scenario == "usage_error" {
					emit(ctlResponse(id, nil, "usage unavailable"))
					continue
				}
				emit(ctlResponse(id, fakeUsage, ""))
			}
		case "user":
			msg, _ := m["message"].(map[string]any)
			rec.Prompt, _ = msg["content"].(string)
			if scenario == "tool" {
				names, code := fakeToolHandshake(read)
				if code != 0 {
					return code
				}
				rec.Tools = names
			}
			writeRecord(&rec)
			return playTurn(scenario)
		}
	}
}

func fakeAuth(scenario string, args []string) int {
	statePath := os.Getenv(envState)
	loggedIn := func() bool {
		if scenario == "auth_in" {
			return true
		}
		_, err := os.Stat(statePath)
		return statePath != "" && err == nil
	}
	switch {
	case len(args) > 0 && args[0] == "status":
		if scenario == "auth_garbage" {
			fmt.Print("not json")
			return 1
		}
		if loggedIn() {
			emit(map[string]any{"loggedIn": true, "authMethod": "claude.ai", "apiProvider": "firstParty", "subscriptionType": "max", "email": "user@example.invalid"})
			return 0
		}
		emit(map[string]any{"loggedIn": false, "authMethod": "none", "apiProvider": "firstParty"})
		return 1
	case len(args) > 0 && args[0] == "login":
		if scenario == "login_fail" {
			fmt.Fprintln(os.Stderr, "login aborted")
			return 1
		}
		fmt.Printf("login args: %s\n", strings.Join(args[1:], " "))
		if scenario != "login_noop" && statePath != "" {
			_ = os.WriteFile(statePath, []byte("in"), 0o600)
		}
		return 0
	}
	return fail("unexpected auth args %v", args)
}

// fakeToolHandshake plays the CLI side of the bridge's MCP handshake and
// returns the tool names it listed.
func fakeToolHandshake(read func() map[string]any) (names []string, code int) {
	emit(mcpRequest("m1", nil, "notifications/initialized"))
	if r := read(); mcpResult(r) == nil {
		return nil, fail("bad notifications/initialized reply: %v", r)
	}
	emit(mcpRequest("m2", 1, "tools/list"))
	res := mcpResult(read())
	list, _ := res["tools"].([]any)
	names = make([]string, 0, len(list))
	for _, t := range list {
		tm, _ := t.(map[string]any)
		name, _ := tm["name"].(string)
		names = append(names, name)
	}
	emit(mcpRequest("m3", 2, "resources/list"))
	r := read()
	resp, _ := r["response"].(map[string]any)
	inner, _ := resp["response"].(map[string]any)
	mcp, _ := inner["mcp_response"].(map[string]any)
	if mcp["error"] == nil {
		return nil, fail("want an MCP error for resources/list, got %v", r)
	}
	emit(map[string]any{"type": "control_request", "request_id": "m4", "request": map[string]any{"subtype": "mcp_message", "server_name": "other", "message": map[string]any{"jsonrpc": "2.0", "id": 3, "method": "tools/list"}}})
	r = read()
	resp, _ = r["response"].(map[string]any)
	if resp["subtype"] != "error" {
		return nil, fail("want an error for an unknown MCP server, got %v", r)
	}
	return names, 0
}

func playTurn(scenario string) int {
	usage := map[string]any{"input_tokens": 3, "cache_read_input_tokens": 10, "cache_creation_input_tokens": 5, "output_tokens": 7}
	emit(map[string]any{"type": "system", "subtype": "init", "tools": []string{}})
	emit(event(map[string]any{"type": "message_start", "message": map[string]any{"usage": map[string]any{"input_tokens": 3}}}))
	switch scenario {
	case "tool":
		emit(event(map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": "Checking."}}))
		emit(event(blockStart(1, "toolu_1", "mcp__c__get_time")))
		emit(event(jsonDelta(1, "")))
		emit(event(jsonDelta(1, `{"tz":`)))
		emit(event(jsonDelta(1, `"UTC"}`)))
		emit(event(blockStart(2, "toolu_2", "mcp__c__get_date")))
		emit(event(blockStart(3, "toolu_3", "Bash")))
		emit(event(jsonDelta(3, `{"command":"true"}`)))
		emit(event(map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "tool_use"}, "usage": usage}))
		return waitForKill()
	case "error":
		emit(map[string]any{"type": "assistant", "error": "rate_limit", "message": map[string]any{"content": []any{}}})
		emit(map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true, "result": "synthetic rate limit"})
		return 0
	case "error_bare":
		emit(map[string]any{"type": "result", "subtype": "error_max_turns", "is_error": true, "errors": []string{"first", "second"}})
		return 0
	case "error_subtype":
		emit(map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true})
		return 0
	case "hang":
		emit(event(textDelta("partial")))
		return waitForKill()
	case "truncated":
		emit(event(textDelta("partial")))
		fmt.Fprint(os.Stderr, "synthetic crash")
		return 4
	case "silent_exit":
		return 5
	}
	if scenario == "subagent" {
		e := event(textDelta("IGNORED"))
		e["parent_tool_use_id"] = "toolu_parent"
		emit(e)
	}
	emit(event(map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}}))
	emit(event(textDelta("Hel")))
	emit(event(textDelta("lo")))
	emit(event(map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "thinking_delta", "thinking": "hidden"}}))
	emit(event(map[string]any{"type": "unknown_event"}))
	emit(map[string]any{"type": "stream_event", "event": "not an object"})
	emit(map[string]any{"type": "rate_limit_event", "rate_limit_info": map[string]any{"status": "allowed"}})
	emit(event(map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn"}, "usage": usage}))
	result := map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": "Hello", "stop_reason": "end_turn", "usage": usage}
	if scenario == "no_result_stop" {
		delete(result, "stop_reason")
		delete(result, "usage")
	}
	emit(result)
	return 0
}

func waitForKill() int {
	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGTERM, os.Interrupt)
	<-c
	return 0
}

func event(ev map[string]any) map[string]any {
	return map[string]any{"type": "stream_event", "event": ev, "parent_tool_use_id": nil}
}

func textDelta(s string) map[string]any {
	return map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": s}}
}

func blockStart(idx int, id, name string) map[string]any {
	return map[string]any{"type": "content_block_start", "index": idx, "content_block": map[string]any{"type": "tool_use", "id": id, "name": name, "input": map[string]any{}}}
}

func jsonDelta(idx int, s string) map[string]any {
	return map[string]any{"type": "content_block_delta", "index": idx, "delta": map[string]any{"type": "input_json_delta", "partial_json": s}}
}

func ctlResponse(id, response any, errMsg string) map[string]any {
	inner := map[string]any{"request_id": id, "subtype": "success", "response": response}
	if errMsg != "" {
		inner = map[string]any{"request_id": id, "subtype": "error", "error": errMsg}
	}
	return map[string]any{"type": "control_response", "response": inner}
}

func mcpRequest(reqID string, msgID any, method string) map[string]any {
	msg := map[string]any{"jsonrpc": "2.0", "method": method}
	if msgID != nil {
		msg["id"] = msgID
	}
	return map[string]any{"type": "control_request", "request_id": reqID, "request": map[string]any{"subtype": "mcp_message", "server_name": "c", "message": msg}}
}

// mcpResult extracts the JSON-RPC result from a control response, or nil.
func mcpResult(r map[string]any) map[string]any {
	resp, _ := r["response"].(map[string]any)
	inner, _ := resp["response"].(map[string]any)
	mcp, _ := inner["mcp_response"].(map[string]any)
	res, _ := mcp["result"].(map[string]any)
	return res
}

func emit(v any) {
	b, _ := json.Marshal(v)
	_, _ = os.Stdout.Write(append(b, '\n'))
}

func fail(format string, args ...any) int {
	fmt.Fprintf(os.Stderr, "fake claude: "+format+"\n", args...)
	return 9
}

func writeRecord(rec *record) {
	if p := os.Getenv(envRecord); p != "" {
		// Write and rename so a kill never leaves a half-written record.
		b, _ := json.Marshal(rec)
		if os.WriteFile(p+".tmp", b, 0o600) == nil {
			_ = os.Rename(p+".tmp", p)
		}
	}
}
