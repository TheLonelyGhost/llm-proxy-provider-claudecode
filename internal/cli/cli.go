// Package cli implements the llm-proxy-provider-claudecode command tree on
// top of the standard library flag package.
//
// With no arguments the binary runs as an llm-proxy sidecar. With
// `--tool <name>` it runs one tooling subcommand and exits.
package cli

import (
	"context"
	"crypto/fips140"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/thelonelyghost/llm-proxy-provider-claudecode/internal/claudecli"
	"github.com/thelonelyghost/llm-proxy-provider-claudecode/internal/config"
	"github.com/thelonelyghost/llm-proxy-provider-claudecode/internal/server"
	"github.com/thelonelyghost/llm-proxy-provider-claudecode/internal/tool"
)

// version is overridden at build time via -ldflags "-X ...cli.version=v1.2.3".
var version = "dev"

// PortEnvVar carries the port llm-proxy assigns to the sidecar.
const PortEnvVar = "LLM_PROXY_PLUGIN_PORT"

// shutdownTimeout is how long in-flight requests may drain after SIGTERM.
// llm-proxy sends SIGKILL after its own 30s default.
const shutdownTimeout = 25 * time.Second

// Driver is everything the sidecar and the tools need from Claude Code.
type Driver interface {
	server.Driver
	tool.Driver
}

// App wires the command tree to its environment so tests can substitute fakes.
type App struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	// Getenv reads configuration (os.Getenv in production).
	Getenv func(string) string
	// Listen opens the sidecar listener (net.Listen in production).
	Listen func(network, address string) (net.Listener, error)
	// NewDriver builds the Claude Code driver for cfg, running the CLI in
	// workDir.
	NewDriver func(cfg config.Config, workDir string) Driver
}

// New returns an App bound to the given output streams and the real
// process environment.
func New(stdout, stderr io.Writer) *App {
	return &App{
		Stdin:  os.Stdin,
		Stdout: stdout,
		Stderr: stderr,
		Getenv: os.Getenv,
		Listen: net.Listen,
		NewDriver: func(cfg config.Config, workDir string) Driver {
			return &claudecli.Driver{Path: cfg.CLIPath, Env: os.Environ(), WorkDir: workDir}
		},
	}
}

const usage = `llm-proxy-provider-claudecode: Claude Code backend for llm-proxy

Usage:
  llm-proxy-provider-claudecode                  run the sidecar (needs ` + PortEnvVar + `)
  llm-proxy-provider-claudecode --tool <name>    run a tool: login [--sso] [--email ADDR], list-models, budget
  llm-proxy-provider-claudecode version [--json]
  llm-proxy-provider-claudecode help
`

// usageError marks bad input so Run exits 2 and prints usage.
type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

// Run executes args (without the program name) and returns the exit code.
func (a *App) Run(ctx context.Context, args []string) int {
	if err := a.dispatch(ctx, args); err != nil {
		var ue usageError
		var te tool.UsageError
		if errors.As(err, &ue) || errors.As(err, &te) || errors.Is(err, tool.ErrUnknown) {
			fmt.Fprintf(a.Stderr, "llm-proxy-provider-claudecode: %v\n\n%s", err, usage)
			return 2
		}
		fmt.Fprintf(a.Stderr, "llm-proxy-provider-claudecode: %v\n", err)
		return 1
	}
	return 0
}

func (a *App) dispatch(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return a.serve(ctx)
	}
	if name, rest, ok := toolFlag(args); ok {
		return a.runTool(ctx, name, rest)
	}
	switch args[0] {
	case "version":
		return a.runVersion(args[1:])
	case "help", "-h", "--help":
		_, err := fmt.Fprint(a.Stdout, usage)
		return err
	default:
		return usageError{fmt.Sprintf("unknown command %q", args[0])}
	}
}

// toolFlag recognises `--tool <name> [args...]` and `--tool=<name> [args...]`
// as the first argument.
func toolFlag(args []string) (name string, rest []string, ok bool) {
	switch {
	case args[0] == "--tool":
		if len(args) < 2 {
			return "", nil, true
		}
		return args[1], args[2:], true
	case strings.HasPrefix(args[0], "--tool="):
		return strings.TrimPrefix(args[0], "--tool="), args[1:], true
	}
	return "", nil, false
}

func (a *App) runTool(ctx context.Context, name string, args []string) error {
	if name == "" {
		return usageError{"--tool needs a tool name"}
	}
	cfg, err := config.Load(a.Getenv)
	if err != nil {
		return err
	}
	workDir, cleanup, err := makeWorkDir()
	if err != nil {
		return err
	}
	defer cleanup()
	d := a.NewDriver(cfg, workDir)
	return tool.Run(ctx, d, tool.IO{Stdin: a.Stdin, Stdout: a.Stdout, Stderr: a.Stderr}, name, args)
}

// serve runs the sidecar on 127.0.0.1 until ctx is cancelled.
func (a *App) serve(ctx context.Context) error {
	port := a.Getenv(PortEnvVar)
	if port == "" {
		return usageError{PortEnvVar + " is not set; llm-proxy sets it when it starts the plugin"}
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return usageError{fmt.Sprintf("%s=%q is not a valid port", PortEnvVar, port)}
	}
	cfg, err := config.Load(a.Getenv)
	if err != nil {
		return err
	}
	workDir, cleanup, err := makeWorkDir()
	if err != nil {
		return err
	}
	defer cleanup()

	logger := slog.New(slog.NewTextHandler(a.Stderr, nil))
	d := a.NewDriver(cfg, workDir)
	if st, err := d.AuthStatus(ctx); err != nil {
		logger.Warn("cannot check Claude Code login", "backend", cfg.Name, "error", err)
	} else if !st.LoggedIn {
		logger.Warn("Claude Code is not logged in; chat requests fail until login",
			"backend", cfg.Name, "fix", "llm-proxy plugin run "+cfg.Name+" login")
	}

	ln, err := a.Listen("tcp", net.JoinHostPort("127.0.0.1", port))
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	srv := &http.Server{
		Handler:           server.New(cfg, d, logger).Handler(),
		ReadHeaderTimeout: 15 * time.Second,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	logger.Info("llm-proxy-provider-claudecode listening", "version", version, "addr", ln.Addr().String(), "backend", cfg.Name)

	select {
	case err := <-errc:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}

// makeWorkDir creates the empty directory the CLI runs in, so that no
// project files or CLAUDE.md reach the model.
func makeWorkDir() (dir string, cleanup func(), err error) {
	dir, err = os.MkdirTemp("", "llm-proxy-provider-claudecode-")
	if err != nil {
		return "", nil, fmt.Errorf("create work dir: %w", err)
	}
	return dir, func() { _ = os.RemoveAll(dir) }, nil
}

func (a *App) runVersion(args []string) error {
	fs := flag.NewFlagSet("version", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return usageError{err.Error()}
	}
	if fs.NArg() != 0 {
		return usageError{"version takes no arguments"}
	}
	if *asJSON {
		return json.NewEncoder(a.Stdout).Encode(struct {
			Version string `json:"version"`
			Go      string `json:"go"`
			FIPS140 bool   `json:"fips140"`
		}{version, runtime.Version(), fips140.Enabled()})
	}
	_, err := fmt.Fprintf(a.Stdout, "llm-proxy-provider-claudecode %s (%s, fips140=%t)\n",
		version, runtime.Version(), fips140.Enabled())
	return err
}
