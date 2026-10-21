package cli

import (
	"bytes"
	"context"
	"crypto/fips140"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/thelonelyghost/llm-proxy-provider-claudecode/internal/claudecli"
	"github.com/thelonelyghost/llm-proxy-provider-claudecode/internal/config"
)

// fakeDriver stands in for claudecli.Driver.
type fakeDriver struct {
	mu       sync.Mutex
	loggedIn bool
	authErr  error
	cfg      config.Config
	workDir  string
}

func (f *fakeDriver) AuthStatus(context.Context) (claudecli.AuthStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return claudecli.AuthStatus{LoggedIn: f.loggedIn, AuthMethod: "claude.ai"}, f.authErr
}

func (f *fakeDriver) Login(context.Context, io.Reader, io.Writer, io.Writer, []string) error {
	return nil
}

func (f *fakeDriver) Catalogue(context.Context) ([]claudecli.Model, error) {
	return []claudecli.Model{{Value: "haiku"}}, nil
}

func (f *fakeDriver) Usage(context.Context) (claudecli.Usage, error) {
	return claudecli.Usage{}, errors.New("no usage")
}

func (f *fakeDriver) Chat(_ context.Context, _ claudecli.Request, emit func(claudecli.Event) error) (claudecli.Result, error) {
	if err := emit(claudecli.Event{Kind: claudecli.TextDelta, Text: "pong"}); err != nil {
		return claudecli.Result{}, err
	}
	return claudecli.Result{StopReason: "end_turn"}, nil
}

type harness struct {
	app    *App
	driver *fakeDriver
	out    bytes.Buffer
	errOut syncBuffer
	env    map[string]string
	// addr receives the sidecar's listen address.
	addr chan string
}

// syncBuffer is a bytes.Buffer safe for the server goroutine and the test.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func newHarness(env map[string]string) *harness {
	h := &harness{driver: &fakeDriver{loggedIn: true}, env: env, addr: make(chan string, 1)}
	h.app = &App{
		Stdin:  strings.NewReader(""),
		Stdout: &h.out,
		Stderr: &h.errOut,
		Getenv: func(k string) string { return h.env[k] },
		Listen: func(network, address string) (net.Listener, error) {
			host, _, err := net.SplitHostPort(address)
			if err != nil || host != "127.0.0.1" {
				return nil, errors.New("sidecar must bind 127.0.0.1, got " + address)
			}
			ln, err := (&net.ListenConfig{}).Listen(context.Background(), network, "127.0.0.1:0")
			if err == nil {
				h.addr <- ln.Addr().String()
			}
			return ln, err
		},
		NewDriver: func(cfg config.Config, workDir string) Driver {
			h.driver.mu.Lock()
			h.driver.cfg, h.driver.workDir = cfg, workDir
			h.driver.mu.Unlock()
			return h.driver
		},
	}
	return h
}

func (h *harness) run(args ...string) int {
	return h.app.Run(context.Background(), args)
}

func TestRun(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		env        map[string]string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{name: "no port", args: nil, wantCode: 2, wantStderr: "LLM_PROXY_PLUGIN_PORT is not set"},
		{name: "bad port", args: nil, env: map[string]string{PortEnvVar: "99999"}, wantCode: 2, wantStderr: "is not a valid port"},
		{name: "bad config", args: nil, env: map[string]string{PortEnvVar: "4000", config.EnvEffort: "turbo"}, wantCode: 1, wantStderr: "unsupported reasoning effort"},
		{name: "unknown command", args: []string{"nope"}, wantCode: 2, wantStderr: `unknown command "nope"`},
		{name: "help", args: []string{"help"}, wantCode: 0, wantStdout: "Usage:"},
		{name: "help flag", args: []string{"--help"}, wantCode: 0, wantStdout: "Usage:"},
		{name: "version", args: []string{"version"}, wantCode: 0, wantStdout: "llm-proxy-provider-claudecode dev ("},
		{name: "version bad flag", args: []string{"version", "--nope"}, wantCode: 2, wantStderr: "flag provided but not defined"},
		{name: "version extra arg", args: []string{"version", "x"}, wantCode: 2, wantStderr: "version takes no arguments"},
		{name: "tool without name", args: []string{"--tool"}, wantCode: 2, wantStderr: "--tool needs a tool name"},
		{name: "unknown tool", args: []string{"--tool", "logout"}, wantCode: 2, wantStderr: `unknown tool "logout"`},
		{name: "tool usage error", args: []string{"--tool=list-models", "x"}, wantCode: 2, wantStderr: "list-models takes no arguments"},
		{name: "tool config error", args: []string{"--tool", "list-models"}, env: map[string]string{config.EnvMaxConcurrency: "0"}, wantCode: 1, wantStderr: "max_concurrency"},
		{name: "tool runs", args: []string{"--tool", "list-models"}, wantCode: 0, wantStdout: "haiku\n"},
		{name: "tool failure", args: []string{"--tool=budget"}, wantCode: 1, wantStdout: `"error":"no usage"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(tt.env)
			code := h.run(tt.args...)
			stdout, stderr := h.out.String(), h.errOut.String()
			if code != tt.wantCode {
				t.Fatalf("exit code = %d, want %d (stderr %q)", code, tt.wantCode, stderr)
			}
			if !strings.Contains(stdout, tt.wantStdout) {
				t.Errorf("stdout = %q, want substring %q", stdout, tt.wantStdout)
			}
			if !strings.Contains(stderr, tt.wantStderr) {
				t.Errorf("stderr = %q, want substring %q", stderr, tt.wantStderr)
			}
			if tt.wantCode == 2 && !strings.Contains(stderr, "Usage:") {
				t.Errorf("usage error must print usage, stderr = %q", stderr)
			}
		})
	}
}

func TestVersionJSON(t *testing.T) {
	h := newHarness(nil)
	if code := h.run("version", "--json"); code != 0 {
		t.Fatalf("exit code = %d, stderr %q", code, h.errOut.String())
	}
	var got struct {
		Version string `json:"version"`
		Go      string `json:"go"`
		FIPS140 bool   `json:"fips140"`
	}
	if err := json.Unmarshal(h.out.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v (%q)", err, h.out.String())
	}
	if got.Version != version || got.Go == "" || got.FIPS140 != fips140.Enabled() {
		t.Errorf("version JSON = %+v", got)
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }

func TestRuntimeFailureExitsOne(t *testing.T) {
	var errOut bytes.Buffer
	code := New(failWriter{}, &errOut).Run(context.Background(), []string{"version"})
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "write failed") {
		t.Errorf("stderr = %q, want the write error", errOut.String())
	}
}

func TestNewUsesRealDriver(t *testing.T) {
	app := New(io.Discard, io.Discard)
	d := app.NewDriver(config.Config{CLIPath: "/opt/claude"}, "/tmp/work")
	cd, ok := d.(*claudecli.Driver)
	if !ok || cd.Path != "/opt/claude" || cd.WorkDir != "/tmp/work" || len(cd.Env) == 0 {
		t.Errorf("driver = %#v", d)
	}
}

func httpDo(t *testing.T, method, url, body string) (status int, respBody string) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// TestServeConformance runs the sidecar the way llm-proxy does and checks
// the plugin protocol: required endpoints, 501 for optional ones, chat, and
// a clean exit when the process is told to stop.
func TestServeConformance(t *testing.T) {
	h := newHarness(map[string]string{PortEnvVar: "4999", "LLM_PROXY_BACKEND_LABEL": "claude"})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- h.app.Run(ctx, nil) }()
	base := "http://" + <-h.addr

	checks := []struct {
		method, path, body string
		wantStatus         int
		wantBody           string
	}{
		{"GET", "/healthz", "", 200, "ok"},
		{"POST", "/management/reload", "", 200, "reloaded"},
		{"GET", "/v1/models", "", 200, `"id":"haiku"`},
		{"POST", "/v1/chat/completions", `{"model":"haiku","messages":[{"role":"user","content":"ping"}]}`, 200, `"content":"pong"`},
		{"POST", "/v1/chat/completions", `{"model":"haiku","stream":true,"messages":[{"role":"user","content":"ping"}]}`, 200, "data: [DONE]"},
		{"POST", "/v1/completions", "{}", 501, "not_implemented"},
		{"POST", "/v1/embeddings", "{}", 501, "not_implemented"},
		{"POST", "/v1/responses", "{}", 501, "not_implemented"},
	}
	for _, c := range checks {
		status, body := httpDo(t, c.method, base+c.path, c.body)
		if status != c.wantStatus || !strings.Contains(body, c.wantBody) {
			t.Errorf("%s %s = %d %q; want %d containing %q", c.method, c.path, status, body, c.wantStatus, c.wantBody)
		}
	}
	h.driver.mu.Lock()
	cfg, workDir := h.driver.cfg, h.driver.workDir
	h.driver.mu.Unlock()
	if cfg.Name != "claude" || workDir == "" {
		t.Errorf("driver built with cfg %+v work dir %q", cfg, workDir)
	}

	cancel()
	if code := <-done; code != 0 {
		t.Fatalf("exit code = %d, stderr %q", code, h.errOut.String())
	}
	if !strings.Contains(h.errOut.String(), "listening") {
		t.Errorf("stderr = %q, want the listening log line", h.errOut.String())
	}
}

func TestServeWarnsWhenNotLoggedIn(t *testing.T) {
	for name, setup := range map[string]func(*fakeDriver){
		"logged out":   func(f *fakeDriver) { f.loggedIn = false },
		"status error": func(f *fakeDriver) { f.authErr = errors.New("no cli") },
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(map[string]string{PortEnvVar: "4999"})
			setup(h.driver)
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan int, 1)
			go func() { done <- h.app.Run(ctx, nil) }()
			<-h.addr
			cancel()
			<-done
			if !strings.Contains(h.errOut.String(), "level=WARN") {
				t.Errorf("stderr = %q, want a warning", h.errOut.String())
			}
		})
	}
}

func TestServeListenError(t *testing.T) {
	h := newHarness(map[string]string{PortEnvVar: "4999"})
	h.app.Listen = func(string, string) (net.Listener, error) { return nil, errors.New("port in use") }
	if code := h.run(); code != 1 || !strings.Contains(h.errOut.String(), "listen: port in use") {
		t.Fatalf("exit code %d, stderr %q", code, h.errOut.String())
	}
}

func TestServeStopsWhenListenerFails(t *testing.T) {
	h := newHarness(map[string]string{PortEnvVar: "4999"})
	h.app.Listen = func(network, _ string) (net.Listener, error) {
		ln, err := (&net.ListenConfig{}).Listen(context.Background(), network, "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		_ = ln.Close()
		return ln, nil
	}
	if code := h.run(); code != 1 || !strings.Contains(h.errOut.String(), "serve:") {
		t.Fatalf("exit code %d, stderr %q", code, h.errOut.String())
	}
}
