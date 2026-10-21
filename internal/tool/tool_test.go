package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/thelonelyghost/llm-proxy-provider-claudecode/internal/claudecli"
)

type fakeDriver struct {
	statuses  []claudecli.AuthStatus
	authErr   error
	loginErr  error
	loginArgs []string
	logins    int
	models    []claudecli.Model
	modelsErr error
	usage     claudecli.Usage
	usageErr  error
}

func (f *fakeDriver) Login(_ context.Context, _ io.Reader, stdout, _ io.Writer, args []string) error {
	f.logins++
	f.loginArgs = args
	_, _ = io.WriteString(stdout, "browser flow\n")
	return f.loginErr
}

func (f *fakeDriver) AuthStatus(context.Context) (claudecli.AuthStatus, error) {
	if f.authErr != nil {
		return claudecli.AuthStatus{}, f.authErr
	}
	st := f.statuses[0]
	if len(f.statuses) > 1 {
		f.statuses = f.statuses[1:]
	}
	return st, nil
}

func (f *fakeDriver) Catalogue(context.Context) ([]claudecli.Model, error) {
	return f.models, f.modelsErr
}

func (f *fakeDriver) Usage(context.Context) (claudecli.Usage, error) {
	return f.usage, f.usageErr
}

func run(f *fakeDriver, name string, args ...string) (string, error) {
	var out bytes.Buffer
	err := Run(context.Background(), f, IO{Stdin: strings.NewReader(""), Stdout: &out, Stderr: io.Discard}, name, args)
	return out.String(), err
}

var (
	loggedOut = claudecli.AuthStatus{AuthMethod: "none"}
	loggedIn  = claudecli.AuthStatus{LoggedIn: true, AuthMethod: "claude.ai", SubscriptionType: "max"}
)

func TestLoginRunsClaudeLogin(t *testing.T) {
	f := &fakeDriver{statuses: []claudecli.AuthStatus{loggedOut, loggedIn}}
	out, err := run(f, "login", "--sso", "--email", "user@example.invalid")
	if err != nil {
		t.Fatal(err)
	}
	if f.logins != 1 || !slices.Equal(f.loginArgs, []string{"--claudeai", "--sso", "--email", "user@example.invalid"}) {
		t.Errorf("login calls %d args %v", f.logins, f.loginArgs)
	}
	if !strings.Contains(out, "browser flow") || !strings.Contains(out, "logged in (method: claude.ai, subscription: max)") {
		t.Errorf("stdout = %q", out)
	}
}

func TestLoginWhenAlreadyLoggedIn(t *testing.T) {
	f := &fakeDriver{statuses: []claudecli.AuthStatus{{LoggedIn: true}}}
	out, err := run(f, "login")
	if err != nil || f.logins != 0 || !strings.Contains(out, "method: unknown, subscription: unknown") {
		t.Fatalf("out %q err %v logins %d", out, err, f.logins)
	}
}

func TestLoginFailures(t *testing.T) {
	tests := []struct {
		name string
		f    *fakeDriver
		args []string
		want string
	}{
		{name: "status error", f: &fakeDriver{authErr: claudecli.ErrCLINotFound}, want: "claude CLI not found"},
		{name: "login error", f: &fakeDriver{statuses: []claudecli.AuthStatus{loggedOut}, loginErr: errors.New("aborted")}, want: "aborted"},
		{name: "still logged out", f: &fakeDriver{statuses: []claudecli.AuthStatus{loggedOut, loggedOut}}, want: "still reports no login"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := run(tt.f, "login", tt.args...); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestLoginStatusErrorAfterLogin(t *testing.T) {
	f := &fakeDriver{statuses: []claudecli.AuthStatus{loggedOut}}
	f.loginErr = nil
	calls := 0
	wrapped := &statusFailsSecond{fakeDriver: f, calls: &calls}
	var out bytes.Buffer
	err := Run(context.Background(), wrapped, IO{Stdout: &out, Stderr: io.Discard}, "login", nil)
	if err == nil || !strings.Contains(err.Error(), "second status failed") {
		t.Fatalf("err = %v", err)
	}
}

type statusFailsSecond struct {
	*fakeDriver
	calls *int
}

func (s *statusFailsSecond) AuthStatus(ctx context.Context) (claudecli.AuthStatus, error) {
	*s.calls++
	if *s.calls > 1 {
		return claudecli.AuthStatus{}, errors.New("second status failed")
	}
	return s.fakeDriver.AuthStatus(ctx)
}

func TestUsageErrors(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "login", args: []string{"--nope"}},
		{name: "login", args: []string{"extra"}},
		{name: "list-models", args: []string{"x"}},
		{name: "budget", args: []string{"x"}},
	}
	for _, tt := range tests {
		_, err := run(&fakeDriver{statuses: []claudecli.AuthStatus{loggedIn}}, tt.name, tt.args...)
		var ue UsageError
		if !errors.As(err, &ue) {
			t.Errorf("%s %v: err = %v, want UsageError", tt.name, tt.args, err)
		}
	}
	if _, err := run(&fakeDriver{}, "logout"); !errors.Is(err, ErrUnknown) {
		t.Errorf("unknown tool: err = %v", err)
	}
}

func TestListModels(t *testing.T) {
	f := &fakeDriver{models: []claudecli.Model{{Value: "opus", ResolvedModel: "claude-opus-5-5"}, {Value: "haiku"}}}
	out, err := run(f, "list-models")
	if err != nil || out != "opus\nhaiku\nclaude-opus-5-5\n" {
		t.Fatalf("out %q err %v", out, err)
	}
	f.modelsErr = errors.New("boom")
	if _, err := run(f, "list-models"); err == nil {
		t.Error("want the catalogue error")
	}
}

func limits(pcts ...float64) []claudecli.Limit {
	out := make([]claudecli.Limit, 0, len(pcts))
	for i, p := range pcts {
		out = append(out, claudecli.Limit{Kind: []string{"session", "weekly"}[i%2], Percent: p, IsActive: true, ResetsAt: "2030-01-01T00:00:00Z"})
	}
	return out
}

func TestBudgetFrom(t *testing.T) {
	scoped := claudecli.Limit{Kind: "weekly_scoped", Percent: 90, IsActive: false, Scope: &claudecli.LimitScope{}}
	scoped.Scope.Model = &struct {
		DisplayName string `json:"display_name"`
	}{DisplayName: "Fable 5"}
	tests := []struct {
		name string
		in   claudecli.Usage
		want Budget
	}{
		{
			name: "usage credits",
			in: claudecli.Usage{SubscriptionType: "enterprise", RateLimitsAvailable: true, RateLimits: &claudecli.RateLimits{
				ExtraUsage: &claudecli.ExtraUsage{IsEnabled: true, MonthlyLimit: 200000, UsedCredits: 12345, Currency: "USD", DecimalPlaces: 2},
				Limits:     append(limits(20), scoped),
			}},
			want: Budget{Object: "usage.budget", Currency: "USD", MaxBudget: 2000, Spend: 123.45, Remaining: 1876.55, Extras: map[string]string{
				"subscription_type": "enterprise", "plan_limit_percent": "20",
				"session_percent": "20", "session_resets_at": "2030-01-01T00:00:00Z",
				"weekly_scoped_fable_5_percent": "90",
			}},
		},
		{
			name: "plan limits only",
			in: claudecli.Usage{RateLimitsAvailable: true, RateLimits: &claudecli.RateLimits{
				ExtraUsage: &claudecli.ExtraUsage{IsEnabled: false},
				Limits:     limits(20, 65.5),
			}},
			want: Budget{Object: "usage.budget", Currency: "percent", MaxBudget: 100, Spend: 65.5, Remaining: 34.5, Extras: map[string]string{
				"session_percent": "20", "session_resets_at": "2030-01-01T00:00:00Z",
				"weekly_percent": "65.5", "weekly_resets_at": "2030-01-01T00:00:00Z",
			}},
		},
		{
			name: "group name when kind is missing",
			in:   claudecli.Usage{RateLimitsAvailable: true, RateLimits: &claudecli.RateLimits{Limits: []claudecli.Limit{{Group: "Monthly Spend", Percent: 5}}}},
			want: Budget{Object: "usage.budget", Currency: "percent", MaxBudget: 100, Remaining: 100, Extras: map[string]string{"monthly_spend_percent": "5"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := BudgetFrom(tt.in)
			if err != nil {
				t.Fatal(err)
			}
			gj, _ := json.Marshal(got)
			wj, _ := json.Marshal(tt.want)
			if !bytes.Equal(gj, wj) {
				t.Errorf("BudgetFrom =\n%s\nwant\n%s", gj, wj)
			}
		})
	}
	if _, err := BudgetFrom(claudecli.Usage{}); err == nil {
		t.Error("no rate limits: want an error")
	}
}

// schema is the subset of assets/budget.schema.json the conformance check
// reads.
type schema struct {
	OneOf []struct {
		Title      string                     `json:"title"`
		Required   []string                   `json:"required"`
		Properties map[string]json.RawMessage `json:"properties"`
	} `json:"oneOf"`
}

func loadSchema(t *testing.T) schema {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "assets", "budget.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var s schema
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

// conforms checks required and allowed top-level keys of one oneOf branch.
func conforms(t *testing.T, s schema, title, out string) {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal([]byte(out), &obj); err != nil {
		t.Fatalf("output is not JSON: %q", out)
	}
	for _, branch := range s.OneOf {
		if branch.Title != title {
			continue
		}
		for _, k := range branch.Required {
			if _, ok := obj[k]; !ok {
				t.Errorf("%s output lacks required %q: %s", title, k, out)
			}
		}
		for k := range obj {
			if _, ok := branch.Properties[k]; !ok {
				t.Errorf("%s output has key %q the schema does not allow: %s", title, k, out)
			}
		}
		if obj["object"] != "usage.budget" {
			t.Errorf("object = %v", obj["object"])
		}
		return
	}
	t.Fatalf("schema has no %q branch", title)
}

func TestBudgetToolMatchesSchema(t *testing.T) {
	s := loadSchema(t)
	f := &fakeDriver{usage: claudecli.Usage{RateLimitsAvailable: true, RateLimits: &claudecli.RateLimits{Limits: limits(10)}}}
	out, err := run(f, "budget")
	if err != nil {
		t.Fatal(err)
	}
	conforms(t, s, "BudgetResponse", out)

	for _, bad := range []*fakeDriver{{usageErr: errors.New("usage unavailable")}, {}} {
		out, err := run(bad, "budget")
		if err == nil {
			t.Error("a failed budget must exit non-zero")
		}
		conforms(t, s, "BudgetErrorResponse", out)
	}
}

func TestBudgetEncodeError(t *testing.T) {
	f := &fakeDriver{usageErr: errors.New("x")}
	err := Run(context.Background(), f, IO{Stdout: errWriter{}}, "budget", nil)
	if err == nil || !strings.Contains(err.Error(), "write failed") {
		t.Fatalf("err = %v", err)
	}
}

type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }

func TestListModelsWriteError(t *testing.T) {
	f := &fakeDriver{models: []claudecli.Model{{Value: "opus"}}}
	if err := Run(context.Background(), f, IO{Stdout: errWriter{}}, "list-models", nil); err == nil {
		t.Fatal("want the write error")
	}
}

func TestManifestMatchesTools(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "manifest", "llm-proxy-provider-claudecode.manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Name            string `json:"name"`
		Version         string `json:"version"`
		ProtocolVersion int    `json:"protocol_version"`
		PortEnvVar      string `json:"port_env_var"`
		Tools           []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m.Name != "claudecode" || m.Version == "" || m.ProtocolVersion != 1 || m.PortEnvVar != "LLM_PROXY_PLUGIN_PORT" {
		t.Errorf("manifest = %+v", m)
	}
	names := make([]string, 0, len(m.Tools))
	for _, tl := range m.Tools {
		if tl.Description == "" {
			t.Errorf("tool %q has no description", tl.Name)
		}
		names = append(names, tl.Name)
	}
	if !slices.Equal(names, Names) {
		t.Errorf("manifest tools %v, implemented %v", names, Names)
	}
}
