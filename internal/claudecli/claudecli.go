// Package claudecli drives the official Claude Code CLI (`claude`) in
// headless stream-json mode.
//
// Every operation starts a fresh `claude -p` process with a fixed set of
// isolation flags: no built-in tools, no settings sources, no MCP servers
// other than the per-request tool bridge, no slash commands, and no session
// files. The package never reads, stores, or forwards Claude credentials;
// the CLI authenticates itself with whatever login the operator made.
package claudecli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// DefaultPath is the CLI looked up on PATH when no explicit path is set.
const DefaultPath = "claude"

const (
	// maxLineBytes caps one stream-json line. Initialize responses and
	// large assistant messages fit well below this.
	maxLineBytes = 16 << 20
	// stderrTail is how much CLI stderr is kept for error messages.
	stderrTail = 2048
	// waitDelay bounds how long Wait blocks on pipes after a kill.
	waitDelay = 2 * time.Second
)

// ErrCLINotFound reports that the claude binary could not be started.
var ErrCLINotFound = errors.New("claude CLI not found")

// UpstreamError is a failure that Claude Code reported for a request.
type UpstreamError struct {
	// Code is the CLI's assistant error class (e.g. "authentication_failed",
	// "rate_limit", "invalid_request"), or empty when the CLI gave none.
	Code string
	// Message is a human-readable description.
	Message string
}

func (e *UpstreamError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("claude: %s: %s", e.Code, e.Message)
	}
	return "claude: " + e.Message
}

// Driver starts claude processes. The zero value uses DefaultPath, the
// current process environment is not inherited unless Env is set.
type Driver struct {
	// Path is the claude executable (a name on PATH or an absolute path).
	Path string
	// Env is the base environment for the child process, typically
	// os.Environ(). Variables that would override the driver's flags are
	// removed (see childEnv).
	Env []string
	// WorkDir is the child's working directory. Use an empty directory so
	// that no project files reach the model.
	WorkDir string
}

func (d *Driver) path() string {
	if d.Path == "" {
		return DefaultPath
	}
	return d.Path
}

// childEnv drops variables that would change the child's behaviour behind
// the driver's back: CLAUDECODE marks a nested Claude Code session, and
// CLAUDE_CODE_EFFORT_LEVEL takes precedence over --effort.
func childEnv(base []string) []string {
	out := make([]string, 0, len(base))
	for _, kv := range base {
		if strings.HasPrefix(kv, "CLAUDECODE=") || strings.HasPrefix(kv, "CLAUDE_CODE_EFFORT_LEVEL=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// efforts maps OpenAI reasoning_effort values and Claude Code effort levels
// to the CLI's --effort vocabulary. Current Claude models cannot turn
// thinking off, so "none" and "minimal" run at the lowest level.
var efforts = map[string]string{
	"none":    "low",
	"minimal": "low",
	"low":     "low",
	"medium":  "medium",
	"high":    "high",
	"xhigh":   "xhigh",
	"max":     "max",
}

// ParseEffort maps a reasoning effort value to a claude --effort level. An
// empty value returns an empty level (use the model's default).
func ParseEffort(v string) (string, error) {
	if v == "" {
		return "", nil
	}
	level, ok := efforts[strings.ToLower(v)]
	if !ok {
		return "", fmt.Errorf("unsupported reasoning effort %q (accepted: none, minimal, low, medium, high, xhigh, max)", v)
	}
	return level, nil
}

// baseArgs are the isolation flags shared by every stream-json session.
func baseArgs() []string {
	return []string{
		"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--no-session-persistence",
		"--setting-sources", "",
		"--strict-mcp-config",
		"--disable-slash-commands",
		"--tools", "",
	}
}

// AuthStatus is the subset of `claude auth status --json` the plugin uses.
// Account identifiers (email, organization) are deliberately not decoded.
type AuthStatus struct {
	LoggedIn         bool   `json:"loggedIn"`
	AuthMethod       string `json:"authMethod"`
	APIProvider      string `json:"apiProvider"`
	SubscriptionType string `json:"subscriptionType"`
}

// AuthStatus runs `claude auth status --json`. The CLI exits 1 when logged
// out but still prints JSON, so the exit code alone is not an error.
func (d *Driver) AuthStatus(ctx context.Context) (AuthStatus, error) {
	cmd := d.command(ctx, "auth", "status", "--json")
	var stdout bytes.Buffer
	stderr := &tailBuffer{max: stderrTail}
	cmd.Stdout = &stdout
	cmd.Stderr = stderr
	runErr := cmd.Run()
	if err := startError(runErr, d.path()); err != nil {
		return AuthStatus{}, err
	}
	var st AuthStatus
	if err := json.Unmarshal(stdout.Bytes(), &st); err != nil {
		if runErr != nil {
			return AuthStatus{}, fmt.Errorf("claude auth status: %w: %s", runErr, stderr.String())
		}
		return AuthStatus{}, fmt.Errorf("claude auth status: decode: %w", err)
	}
	return st, nil
}

// Login runs `claude auth login` interactively with the given streams.
func (d *Driver) Login(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, args []string) error {
	cmd := d.command(ctx, append([]string{"auth", "login"}, args...)...)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		if serr := startError(err, d.path()); serr != nil {
			return serr
		}
		return fmt.Errorf("claude auth login: %w", err)
	}
	return nil
}

// Model is one entry of the account's model catalogue.
type Model struct {
	// Value is what --model accepts (an alias such as "opus" or a full ID).
	Value string `json:"value"`
	// ResolvedModel is the full model ID the value currently points to.
	ResolvedModel string `json:"resolvedModel"`
	// DisplayName is the human-readable name.
	DisplayName string `json:"displayName"`
	// SupportedEffortLevels lists the --effort levels the model accepts.
	SupportedEffortLevels []string `json:"supportedEffortLevels"`
}

// Catalogue returns the models the logged-in account may use, as reported by
// the CLI's initialize handshake. No message is sent to the model.
func (d *Driver) Catalogue(ctx context.Context) ([]Model, error) {
	s, err := d.start(ctx, baseArgs())
	if err != nil {
		return nil, err
	}
	defer s.close()
	raw, err := s.initialize()
	if err != nil {
		return nil, err
	}
	var init struct {
		Models []Model `json:"models"`
	}
	if err := json.Unmarshal(raw, &init); err != nil {
		return nil, fmt.Errorf("decode initialize response: %w", err)
	}
	return init.Models, nil
}

// ModelIDs returns every ID a client may send for the catalogue: each
// alias value first, then each distinct resolved model ID.
func ModelIDs(models []Model) []string {
	seen := make(map[string]struct{}, 2*len(models))
	ids := make([]string, 0, 2*len(models))
	add := func(id string) {
		if id == "" {
			return
		}
		if _, ok := seen[id]; ok {
			return
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	for _, m := range models {
		add(m.Value)
	}
	for _, m := range models {
		add(m.ResolvedModel)
	}
	return ids
}

// Usage is the subset of the CLI's get_usage control response that the
// budget tool reports.
type Usage struct {
	SubscriptionType    string      `json:"subscription_type"`
	RateLimitsAvailable bool        `json:"rate_limits_available"`
	RateLimits          *RateLimits `json:"rate_limits"`
}

// RateLimits holds plan limits and usage credits.
type RateLimits struct {
	ExtraUsage *ExtraUsage `json:"extra_usage"`
	Limits     []Limit     `json:"limits"`
}

// ExtraUsage describes usage credits that cover use past plan limits.
// Amounts are in minor units; DecimalPlaces gives the scale.
type ExtraUsage struct {
	IsEnabled     bool    `json:"is_enabled"`
	MonthlyLimit  float64 `json:"monthly_limit"`
	UsedCredits   float64 `json:"used_credits"`
	Currency      string  `json:"currency"`
	DecimalPlaces int     `json:"decimal_places"`
}

// Limit is one plan limit window.
type Limit struct {
	Kind     string      `json:"kind"`
	Group    string      `json:"group"`
	Percent  float64     `json:"percent"`
	ResetsAt string      `json:"resets_at"`
	IsActive bool        `json:"is_active"`
	Scope    *LimitScope `json:"scope"`
}

// LimitScope narrows a limit to one model.
type LimitScope struct {
	Model *struct {
		DisplayName string `json:"display_name"`
	} `json:"model"`
}

// Usage returns plan usage through the CLI's get_usage control request.
// Anthropic marks this request experimental. No message is sent to the
// model.
func (d *Driver) Usage(ctx context.Context) (Usage, error) {
	s, err := d.start(ctx, baseArgs())
	if err != nil {
		return Usage{}, err
	}
	defer s.close()
	if _, err := s.initialize(); err != nil {
		return Usage{}, err
	}
	raw, err := s.control(map[string]any{"subtype": "get_usage", "skip_behaviors": true}, nil)
	if err != nil {
		return Usage{}, err
	}
	var u Usage
	if err := json.Unmarshal(raw, &u); err != nil {
		return Usage{}, fmt.Errorf("decode get_usage response: %w", err)
	}
	return u, nil
}

// command builds an exec.Cmd with the driver's path, env, and work dir.
func (d *Driver) command(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, d.path(), args...) //nolint:gosec // operator-configured CLI path; arguments are fixed flags
	cmd.Dir = d.WorkDir
	cmd.Env = childEnv(d.Env)
	cmd.WaitDelay = waitDelay
	return cmd
}

// startError converts a failure to start the CLI into ErrCLINotFound. It
// returns nil for any other error (including a non-zero exit).
func startError(err error, path string) error {
	if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%w: %s (install Claude Code or set cli_path)", ErrCLINotFound, path)
	}
	return nil
}

// frame is one stream-json line from the CLI. Only the fields the driver
// reads are decoded.
type frame struct {
	Type      string          `json:"type"`
	Subtype   string          `json:"subtype"`
	RequestID string          `json:"request_id"`
	Request   json.RawMessage `json:"request"`
	Response  json.RawMessage `json:"response"`
	Event     json.RawMessage `json:"event"`
	// Error is the assistant error class on "assistant" frames.
	Error string `json:"error"`
	// ParentToolUseID is set on frames from sub-agents.
	ParentToolUseID *string `json:"parent_tool_use_id"`
	// Result frame fields.
	IsError    bool        `json:"is_error"`
	Result     string      `json:"result"`
	StopReason string      `json:"stop_reason"`
	Errors     []string    `json:"errors"`
	Usage      *TokenUsage `json:"usage"`
}

// TokenUsage is the token accounting the CLI reports for a message.
type TokenUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

// controlHandler answers a control request from the CLI. It returns the
// response payload, or ok=false to send no response at all.
type controlHandler func(subtype string, request json.RawMessage) (response any, ok bool, err error)

// session is one running claude process.
type session struct {
	ctx    context.Context
	cmd    *exec.Cmd
	cancel context.CancelFunc
	stdin  io.WriteCloser
	out    *bufio.Scanner
	stderr *tailBuffer
	nextID int
	// inputClosed records that stdin was closed after the prompt.
	inputClosed bool
}

func (d *Driver) start(ctx context.Context, args []string) (*session, error) {
	ctx, cancel := context.WithCancel(ctx)
	cmd := d.command(ctx, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("claude stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("claude stdout: %w", err)
	}
	stderr := &tailBuffer{max: stderrTail}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		cancel()
		if serr := startError(err, d.path()); serr != nil {
			return nil, serr
		}
		return nil, fmt.Errorf("start claude: %w", err)
	}
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64<<10), maxLineBytes)
	return &session{ctx: ctx, cmd: cmd, cancel: cancel, stdin: stdin, out: sc, stderr: stderr}, nil
}

// close stops the process and releases its resources.
func (s *session) close() {
	_ = s.stdin.Close()
	s.cancel()
	_ = s.cmd.Wait()
}

// send writes one stream-json line.
func (s *session) send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encode frame: %w", err)
	}
	if _, err := s.stdin.Write(append(b, '\n')); err != nil {
		// A write fails when the CLI already exited; report why it exited.
		return s.exited()
	}
	return nil
}

// closeInput closes stdin so the CLI ends once the turn completes.
func (s *session) closeInput() {
	if !s.inputClosed {
		s.inputClosed = true
		_ = s.stdin.Close()
	}
}

// next reads the next frame. Lines that are not JSON are skipped. At end
// of output it returns an error that carries the CLI's stderr tail.
func (s *session) next() (frame, error) {
	for s.out.Scan() {
		var f frame
		if err := json.Unmarshal(s.out.Bytes(), &f); err != nil {
			continue
		}
		return f, nil
	}
	if err := s.out.Err(); err != nil {
		return frame{}, fmt.Errorf("read from claude: %w", err)
	}
	return frame{}, s.exited()
}

// exited waits for the CLI to stop and explains why it stopped early: the
// session's context ended, or the CLI failed (its stderr tail is the
// message).
func (s *session) exited() error {
	waitErr := s.cmd.Wait()
	if err := s.ctx.Err(); err != nil {
		return err
	}
	msg := strings.TrimSpace(s.stderr.String())
	if msg == "" {
		msg = "claude exited before completing the request"
		if waitErr != nil {
			msg += ": " + waitErr.Error()
		}
	}
	return &UpstreamError{Message: msg}
}

// control sends a control request and reads frames until its response
// arrives. Control requests from the CLI in between go to h (nil answers
// every one with an error).
func (s *session) control(request map[string]any, h controlHandler) (json.RawMessage, error) {
	s.nextID++
	id := fmt.Sprintf("req_%d", s.nextID)
	if err := s.send(map[string]any{"type": "control_request", "request_id": id, "request": request}); err != nil {
		return nil, err
	}
	for {
		f, err := s.next()
		if err != nil {
			return nil, err
		}
		switch f.Type {
		case "control_request":
			if err := s.answer(f, h); err != nil {
				return nil, err
			}
		case "control_response":
			var resp struct {
				Subtype   string          `json:"subtype"`
				RequestID string          `json:"request_id"`
				Response  json.RawMessage `json:"response"`
				Error     string          `json:"error"`
			}
			if err := json.Unmarshal(f.Response, &resp); err != nil {
				return nil, fmt.Errorf("decode control response: %w", err)
			}
			if resp.RequestID != id {
				continue
			}
			if resp.Subtype == "error" {
				return nil, &UpstreamError{Message: fmt.Sprintf("%v request failed: %s", request["subtype"], resp.Error)}
			}
			return resp.Response, nil
		}
	}
}

// initialize performs the control-protocol handshake.
func (s *session) initialize() (json.RawMessage, error) {
	return s.initializeWith(nil)
}

func (s *session) initializeWith(h controlHandler) (json.RawMessage, error) {
	return s.control(map[string]any{"subtype": "initialize", "hooks": nil}, h)
}

// answer replies to a control request from the CLI.
func (s *session) answer(f frame, h controlHandler) error {
	var req struct {
		Subtype string `json:"subtype"`
	}
	_ = json.Unmarshal(f.Request, &req)
	if h != nil {
		resp, ok, err := h(req.Subtype, f.Request)
		if err != nil {
			return s.send(controlResponse(f.RequestID, nil, err.Error()))
		}
		if !ok {
			return nil
		}
		return s.send(controlResponse(f.RequestID, resp, ""))
	}
	return s.send(controlResponse(f.RequestID, nil, "unsupported control request: "+req.Subtype))
}

func controlResponse(id string, response any, errMsg string) map[string]any {
	inner := map[string]any{"request_id": id}
	if errMsg != "" {
		inner["subtype"] = "error"
		inner["error"] = errMsg
	} else {
		inner["subtype"] = "success"
		inner["response"] = response
	}
	return map[string]any{"type": "control_response", "response": inner}
}

// tailBuffer keeps the last max bytes written to it. It is safe for use by
// the exec package's stderr copier and a reader at the same time.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - t.max; over > 0 {
		t.buf = t.buf[over:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}
