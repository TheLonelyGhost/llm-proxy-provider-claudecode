// Package tool implements the plugin's tooling subcommands, which llm-proxy
// runs as `<binary> --tool <name> [args...]`.
package tool

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"

	"github.com/thelonelyghost/llm-proxy-provider-claudecode/internal/claudecli"
)

// Names lists the tools, in the order the manifest declares them.
var Names = []string{"login", "list-models", "budget"}

// ErrUnknown reports a tool name the plugin does not implement.
var ErrUnknown = errors.New("unknown tool")

// UsageError reports bad tool arguments.
type UsageError struct{ Msg string }

func (e UsageError) Error() string { return e.Msg }

// Driver is the part of claudecli.Driver the tools use.
type Driver interface {
	Login(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, args []string) error
	AuthStatus(ctx context.Context) (claudecli.AuthStatus, error)
	Catalogue(ctx context.Context) ([]claudecli.Model, error)
	Usage(ctx context.Context) (claudecli.Usage, error)
}

// IO holds a tool's standard streams.
type IO struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// Run executes the named tool.
func Run(ctx context.Context, d Driver, stdio IO, name string, args []string) error {
	switch name {
	case "login":
		return login(ctx, d, stdio, args)
	case "list-models":
		if len(args) != 0 {
			return UsageError{"list-models takes no arguments"}
		}
		return listModels(ctx, d, stdio.Stdout)
	case "budget":
		if len(args) != 0 {
			return UsageError{"budget takes no arguments"}
		}
		return budget(ctx, d, stdio.Stdout)
	default:
		return fmt.Errorf("%w %q (available: %s)", ErrUnknown, name, strings.Join(Names, ", "))
	}
}

// login runs Claude Code's own login and confirms the result. The plugin
// never sees or stores the credential: the CLI keeps it.
func login(ctx context.Context, d Driver, stdio IO, args []string) error {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	sso := fs.Bool("sso", false, "force the SSO login flow")
	email := fs.String("email", "", "pre-fill the email address on the login page")
	if err := fs.Parse(args); err != nil {
		return UsageError{err.Error()}
	}
	if fs.NArg() != 0 {
		return UsageError{"login takes only --sso and --email"}
	}

	st, err := d.AuthStatus(ctx)
	if err != nil {
		return err
	}
	if !st.LoggedIn {
		loginArgs := []string{"--claudeai"}
		if *sso {
			loginArgs = append(loginArgs, "--sso")
		}
		if *email != "" {
			loginArgs = append(loginArgs, "--email", *email)
		}
		if err := d.Login(ctx, stdio.Stdin, stdio.Stdout, stdio.Stderr, loginArgs); err != nil {
			return err
		}
		if st, err = d.AuthStatus(ctx); err != nil {
			return err
		}
		if !st.LoggedIn {
			return errors.New("claude auth login finished but Claude Code still reports no login")
		}
	}
	_, err = fmt.Fprintf(stdio.Stdout, "Claude Code is logged in (method: %s, subscription: %s).\n",
		orUnknown(st.AuthMethod), orUnknown(st.SubscriptionType))
	return err
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// listModels prints every model ID the account may use, one per line. It
// is not filtered by the operator allow-list.
func listModels(ctx context.Context, d Driver, out io.Writer) error {
	models, err := d.Catalogue(ctx)
	if err != nil {
		return err
	}
	for _, id := range claudecli.ModelIDs(models) {
		if _, err := fmt.Fprintln(out, id); err != nil {
			return err
		}
	}
	return nil
}

// Budget is the JSON object llm-proxy reads from `--tool budget`
// (assets/budget.schema.json).
type Budget struct {
	Object    string            `json:"object"`
	Currency  string            `json:"currency,omitempty"`
	MaxBudget float64           `json:"max_budget"`
	Spend     float64           `json:"spend"`
	Remaining float64           `json:"remaining"`
	Unlimited bool              `json:"unlimited,omitempty"`
	Extras    map[string]string `json:"extras,omitempty"`
}

type budgetError struct {
	Object string `json:"object"`
	Error  string `json:"error"`
}

// budget prints plan usage. It always writes a JSON object; on failure the
// object carries "error" and the tool exits non-zero.
func budget(ctx context.Context, d Driver, out io.Writer) error {
	u, err := d.Usage(ctx)
	var b Budget
	if err == nil {
		b, err = BudgetFrom(u)
	}
	enc := json.NewEncoder(out)
	if err != nil {
		if encErr := enc.Encode(budgetError{Object: "usage.budget", Error: err.Error()}); encErr != nil {
			return encErr
		}
		return err
	}
	return enc.Encode(b)
}

// BudgetFrom maps the CLI's usage report to a Budget. Usage credits, when
// the organisation enables them, are the spending ceiling. Otherwise the
// busiest plan limit window is reported as a percentage.
func BudgetFrom(u claudecli.Usage) (Budget, error) {
	if !u.RateLimitsAvailable || u.RateLimits == nil {
		return Budget{}, errors.New("no plan limits reported for this Claude Code login (API key or third-party provider?)")
	}
	b := Budget{Object: "usage.budget", Extras: map[string]string{}}
	if u.SubscriptionType != "" {
		b.Extras["subscription_type"] = u.SubscriptionType
	}
	var busiest float64
	for _, l := range u.RateLimits.Limits {
		key := limitKey(l)
		b.Extras[key+"_percent"] = formatNumber(l.Percent)
		if l.ResetsAt != "" {
			b.Extras[key+"_resets_at"] = l.ResetsAt
		}
		if l.IsActive {
			busiest = math.Max(busiest, l.Percent)
		}
	}

	if x := u.RateLimits.ExtraUsage; x != nil && x.IsEnabled && x.MonthlyLimit > 0 {
		scale := math.Pow10(x.DecimalPlaces)
		b.Currency = x.Currency
		b.MaxBudget = x.MonthlyLimit / scale
		b.Spend = x.UsedCredits / scale
		// Subtract in minor units so the result has no float residue.
		b.Remaining = math.Max(0, x.MonthlyLimit-x.UsedCredits) / scale
		b.Extras["plan_limit_percent"] = formatNumber(busiest)
		return b, nil
	}
	b.Currency = "percent"
	b.MaxBudget = 100
	b.Spend = busiest
	b.Remaining = math.Max(0, 100-busiest)
	return b, nil
}

// limitKey names a limit window for extras, e.g. "session" or
// "weekly_scoped_fable".
func limitKey(l claudecli.Limit) string {
	key := l.Kind
	if key == "" {
		key = l.Group
	}
	if l.Scope != nil && l.Scope.Model != nil && l.Scope.Model.DisplayName != "" {
		key += "_" + l.Scope.Model.DisplayName
	}
	return strings.ToLower(strings.ReplaceAll(key, " ", "_"))
}

func formatNumber(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}
