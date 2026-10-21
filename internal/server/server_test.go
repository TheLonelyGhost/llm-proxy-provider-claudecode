package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thelonelyghost/llm-proxy-provider-claudecode/internal/claudecli"
	"github.com/thelonelyghost/llm-proxy-provider-claudecode/internal/config"
)

// fakeDriver stands in for claudecli.Driver.
type fakeDriver struct {
	mu        sync.Mutex
	auth      claudecli.AuthStatus
	authErr   error
	authCalls int
	models    []claudecli.Model
	modelsErr error
	catCalls  int
	events    []claudecli.Event
	result    claudecli.Result
	chatErr   error
	chatCalls int
	gotReq    claudecli.Request
	// started, when set, receives once Chat begins; Chat then waits for
	// its context to end.
	started chan struct{}
}

func newFake() *fakeDriver {
	return &fakeDriver{
		auth:   claudecli.AuthStatus{LoggedIn: true},
		models: []claudecli.Model{{Value: "haiku", ResolvedModel: "claude-haiku-5-5"}},
		events: []claudecli.Event{{Kind: claudecli.TextDelta, Text: "Hel"}, {Kind: claudecli.TextDelta, Text: "lo"}},
		result: claudecli.Result{StopReason: "end_turn", Usage: claudecli.TokenUsage{InputTokens: 2, OutputTokens: 3}},
	}
}

func (f *fakeDriver) AuthStatus(context.Context) (claudecli.AuthStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.authCalls++
	return f.auth, f.authErr
}

func (f *fakeDriver) Catalogue(context.Context) ([]claudecli.Model, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.catCalls++
	return f.models, f.modelsErr
}

func (f *fakeDriver) Chat(ctx context.Context, req claudecli.Request, emit func(claudecli.Event) error) (claudecli.Result, error) {
	f.mu.Lock()
	f.chatCalls++
	f.gotReq = req
	started := f.started
	f.mu.Unlock()
	if started != nil {
		started <- struct{}{}
		<-ctx.Done()
		return claudecli.Result{}, ctx.Err()
	}
	for _, ev := range f.events {
		if err := emit(ev); err != nil {
			return claudecli.Result{}, err
		}
	}
	if f.chatErr != nil {
		return claudecli.Result{}, f.chatErr
	}
	return f.result, nil
}

// counts returns the call counters under the lock.
func (f *fakeDriver) counts() (auth, catalogue, chat int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.authCalls, f.catCalls, f.chatCalls
}

// request returns the last chat request under the lock.
func (f *fakeDriver) request() claudecli.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gotReq
}

func newServer(t *testing.T, f *fakeDriver, mutate ...func(*config.Config)) (s *Server, ts *httptest.Server) {
	t.Helper()
	cfg := config.Config{Name: "claude", MaxConcurrency: 2}
	for _, m := range mutate {
		m(&cfg)
	}
	s = New(cfg, f, nil)
	ts = httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return s, ts
}

// response is the part of an HTTP response the tests read; the helpers
// close the body.
type response struct {
	StatusCode int
	Header     http.Header
}

func post(t *testing.T, url, body string) (resp response, respBody string) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	return do(t, req)
}

func get(t *testing.T, url string) (resp response, respBody string) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	return do(t, req)
}

func do(t *testing.T, req *http.Request) (resp response, body string) {
	t.Helper()
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	return response{StatusCode: r.StatusCode, Header: r.Header}, string(b)
}

const hello = `{"model":"haiku","messages":[{"role":"user","content":"hi"}]}`

func TestManagementEndpoints(t *testing.T) {
	_, ts := newServer(t, newFake())
	if resp, body := get(t, ts.URL+"/healthz"); resp.StatusCode != http.StatusOK || body != "ok\n" {
		t.Errorf("healthz = %d %q", resp.StatusCode, body)
	}
	if resp, _ := post(t, ts.URL+"/management/reload", ""); resp.StatusCode != http.StatusOK {
		t.Errorf("reload = %d", resp.StatusCode)
	}
	for _, p := range []string{"/v1/completions", "/v1/embeddings", "/v1/responses"} {
		resp, body := post(t, ts.URL+p, "{}")
		if resp.StatusCode != http.StatusNotImplemented || !strings.Contains(body, "not_implemented") {
			t.Errorf("%s = %d %s", p, resp.StatusCode, body)
		}
	}
}

func TestModelsFromCatalogueIsCachedUntilReload(t *testing.T) {
	f := newFake()
	_, ts := newServer(t, f)
	for range 2 {
		resp, body := get(t, ts.URL+"/v1/models")
		want := `{"data":[{"id":"haiku","object":"model","created":0,"owned_by":"anthropic"},{"id":"claude-haiku-5-5","object":"model","created":0,"owned_by":"anthropic"}],"object":"list"}` + "\n"
		if resp.StatusCode != http.StatusOK || body != want {
			t.Fatalf("models = %d %s", resp.StatusCode, body)
		}
	}
	post(t, ts.URL+"/management/reload", "")
	get(t, ts.URL+"/v1/models")
	if _, cat, _ := f.counts(); cat != 2 {
		t.Errorf("catalogue calls = %d, want 2 (cached, then reloaded)", cat)
	}
}

func TestModelsAllowList(t *testing.T) {
	f := newFake()
	_, ts := newServer(t, f, func(c *config.Config) { c.Models = []string{"opus"} })
	_, body := get(t, ts.URL+"/v1/models")
	if _, cat, _ := f.counts(); !strings.Contains(body, `"id":"opus"`) || strings.Contains(body, "haiku") || cat != 0 {
		t.Errorf("models = %s (catalogue calls %d)", body, cat)
	}
}

func TestModelsCatalogueErrorStill200(t *testing.T) {
	f := newFake()
	f.modelsErr = errors.New("cli broke")
	_, ts := newServer(t, f)
	resp, body := get(t, ts.URL+"/v1/models")
	if resp.StatusCode != http.StatusOK || body != `{"data":[],"object":"list"}`+"\n" {
		t.Errorf("models = %d %s", resp.StatusCode, body)
	}
}

func TestChatNonStreaming(t *testing.T) {
	f := newFake()
	_, ts := newServer(t, f, func(c *config.Config) { c.Effort = "high" })
	resp, body := post(t, ts.URL+"/v1/chat/completions", `{"model":"haiku","messages":[{"role":"system","content":"S"},{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	var c struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			TotalTokens int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(body), &c); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(c.ID, "chatcmpl-") || c.Model != "haiku" || c.Choices[0].Message.Content != "Hello" || c.Choices[0].FinishReason != "stop" || c.Usage.TotalTokens != 5 {
		t.Errorf("completion = %s", body)
	}
	if got := f.request(); got.SystemPrompt != "S" || len(got.Turns) != 1 || got.Turns[0].Text != "hi" || got.Effort != "high" || got.Model != "haiku" {
		t.Errorf("driver got %+v", got)
	}
}

func TestChatStreaming(t *testing.T) {
	f := newFake()
	_, ts := newServer(t, f)
	resp, body := post(t, ts.URL+"/v1/chat/completions", `{"model":"haiku","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("status %d content-type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	for _, want := range []string{`"delta":{"role":"assistant"}`, `"delta":{"content":"Hel"}`, `"finish_reason":"stop"`, `"usage":{"prompt_tokens":2`, "data: [DONE]\n\n"} {
		if !strings.Contains(body, want) {
			t.Errorf("stream missing %q:\n%s", want, body)
		}
	}
}

func TestChatStreamingWithoutEvents(t *testing.T) {
	f := newFake()
	f.events = nil
	_, ts := newServer(t, f)
	resp, body := post(t, ts.URL+"/v1/chat/completions", `{"model":"haiku","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK || !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
}

func TestChatStreamingErrorBeforeFirstEvent(t *testing.T) {
	f := newFake()
	f.events = nil
	f.chatErr = &claudecli.UpstreamError{Code: "rate_limit", Message: "slow down"}
	_, ts := newServer(t, f)
	resp, body := post(t, ts.URL+"/v1/chat/completions", `{"model":"haiku","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusTooManyRequests || !strings.Contains(body, "slow down") {
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
}

func TestChatStreamingErrorAfterFirstEvent(t *testing.T) {
	f := newFake()
	f.chatErr = &claudecli.UpstreamError{Message: "cut off"}
	_, ts := newServer(t, f)
	resp, body := post(t, ts.URL+"/v1/chat/completions", `{"model":"haiku","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `data: {"error":{"message":"claude: cut off","type":"upstream_error"`) || strings.Contains(body, "[DONE]") {
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
}

func TestChatErrors(t *testing.T) {
	tests := []struct {
		name       string
		setup      func(*fakeDriver)
		body       string
		wantStatus int
		wantBody   string
		wantChat   bool
	}{
		{name: "bad request", body: `{"model":"haiku","messages":[]}`, wantStatus: 400, wantBody: "messages must not be empty"},
		{name: "bad translate", body: `{"model":"haiku","reasoning_effort":"turbo","messages":[{"role":"user","content":"hi"}]}`, wantStatus: 400, wantBody: "unsupported reasoning effort"},
		{name: "not logged in", setup: func(f *fakeDriver) { f.auth.LoggedIn = false }, body: hello, wantStatus: 401, wantBody: "llm-proxy plugin run claude login"},
		{name: "auth check fails", setup: func(f *fakeDriver) { f.authErr = claudecli.ErrCLINotFound }, body: hello, wantStatus: 503, wantBody: "claude CLI not found"},
		{name: "unknown model", body: `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`, wantStatus: 404, wantBody: "model_not_found"},
		{name: "catalogue fails", setup: func(f *fakeDriver) { f.modelsErr = errors.New("boom") }, body: hello, wantStatus: 500, wantBody: "internal error"},
		{name: "auth upstream", setup: func(f *fakeDriver) {
			f.chatErr = &claudecli.UpstreamError{Code: "authentication_failed", Message: "expired"}
		}, body: hello, wantStatus: 401, wantBody: "expired", wantChat: true},
		{name: "invalid upstream", setup: func(f *fakeDriver) {
			f.chatErr = &claudecli.UpstreamError{Code: "invalid_request", Message: "bad model"}
		}, body: hello, wantStatus: 400, wantBody: "bad model", wantChat: true},
		{name: "other upstream", setup: func(f *fakeDriver) { f.chatErr = &claudecli.UpstreamError{Message: "boom"} }, body: hello, wantStatus: 502, wantBody: "upstream_error", wantChat: true},
		{name: "deadline", setup: func(f *fakeDriver) { f.chatErr = context.DeadlineExceeded }, body: hello, wantStatus: 504, wantBody: "request cancelled", wantChat: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake()
			if tt.setup != nil {
				tt.setup(f)
			}
			_, ts := newServer(t, f)
			resp, body := post(t, ts.URL+"/v1/chat/completions", tt.body)
			if resp.StatusCode != tt.wantStatus || !strings.Contains(body, tt.wantBody) {
				t.Errorf("status %d body %s; want %d containing %q", resp.StatusCode, body, tt.wantStatus, tt.wantBody)
			}
			if _, _, chats := f.counts(); (chats > 0) != tt.wantChat {
				t.Errorf("chat calls = %d, want called = %t (no message may be sent before checks pass)", chats, tt.wantChat)
			}
		})
	}
}

func TestChatBodyLimit(t *testing.T) {
	_, ts := newServer(t, newFake())
	resp, _ := post(t, ts.URL+"/v1/chat/completions", `{"model":"`+strings.Repeat("x", maxBodyBytes)+`"}`)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", resp.StatusCode)
	}
}

func TestChatMethodNotAllowed(t *testing.T) {
	_, ts := newServer(t, newFake())
	if resp, _ := get(t, ts.URL+"/v1/chat/completions"); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET chat = %d, want 405", resp.StatusCode)
	}
}

func TestAuthCheckIsCached(t *testing.T) {
	f := newFake()
	s, ts := newServer(t, f)
	now := time.Unix(1000, 0)
	s.now = func() time.Time { return now }
	post(t, ts.URL+"/v1/chat/completions", hello)
	post(t, ts.URL+"/v1/chat/completions", hello)
	if auth, _, _ := f.counts(); auth != 1 {
		t.Errorf("auth calls = %d, want 1 within the TTL", auth)
	}
	now = now.Add(authTTL)
	post(t, ts.URL+"/v1/chat/completions", hello)
	if auth, _, _ := f.counts(); auth != 2 {
		t.Errorf("auth calls = %d, want 2 after the TTL", auth)
	}
}

func TestFailedAuthIsNotCached(t *testing.T) {
	f := newFake()
	f.auth.LoggedIn = false
	_, ts := newServer(t, f)
	post(t, ts.URL+"/v1/chat/completions", hello)
	f.mu.Lock()
	f.auth.LoggedIn = true
	f.mu.Unlock()
	if resp, body := post(t, ts.URL+"/v1/chat/completions", hello); resp.StatusCode != http.StatusOK {
		t.Errorf("after login: %d %s", resp.StatusCode, body)
	}
}

func TestConcurrencyWaitEndsWithClient(t *testing.T) {
	f := newFake()
	f.started = make(chan struct{}, 1)
	s, ts := newServer(t, f, func(c *config.Config) { c.MaxConcurrency = 1 })

	// First request takes the only slot and holds it.
	ctx1, cancel1 := context.WithCancel(context.Background())
	done1 := make(chan struct{})
	go func() {
		defer close(done1)
		req, _ := http.NewRequestWithContext(ctx1, http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(hello))
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}()
	<-f.started

	// Second request waits for a slot; its client gives up.
	ctx2, cancel2 := context.WithCancel(context.Background())
	rec := httptest.NewRecorder()
	req2 := httptest.NewRequestWithContext(ctx2, http.MethodPost, "/v1/chat/completions", strings.NewReader(hello))
	done2 := make(chan struct{})
	go func() {
		defer close(done2)
		s.Handler().ServeHTTP(rec, req2)
	}()
	cancel2()
	<-done2
	if _, _, chats := f.counts(); chats != 1 {
		t.Errorf("chat calls = %d, want 1: the waiting request must not run", chats)
	}
	cancel1()
	<-done1
}
