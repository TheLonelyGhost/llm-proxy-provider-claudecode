package config

import (
	"slices"
	"strings"
	"testing"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Name != DefaultName || cfg.CLIPath != "claude" || cfg.Models != nil || cfg.Effort != "" || cfg.MaxConcurrency != DefaultMaxConcurrency {
		t.Errorf("defaults = %+v", cfg)
	}
}

func TestLoadLayers(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want Config
	}{
		{
			name: "backend config JSON with native types",
			env: map[string]string{
				EnvBackendConfig: `{"name":"claude","type":"claudecode","cli_path":"/opt/claude","models":["opus"," haiku ",""],"effort":"High","max_concurrency":2}`,
				EnvBackendLabel:  "ignored",
			},
			want: Config{Name: "claude", CLIPath: "/opt/claude", Models: []string{"opus", "haiku"}, Effort: "high", MaxConcurrency: 2},
		},
		{
			name: "backend config JSON with string forms",
			env:  map[string]string{EnvBackendConfig: `{"models":"opus, sonnet","max_concurrency":"3","effort":"minimal"}`, EnvBackendLabel: "cc"},
			want: Config{Name: "cc", CLIPath: "claude", Models: []string{"opus", "sonnet"}, Effort: "low", MaxConcurrency: 3},
		},
		{
			name: "env overrides win",
			env: map[string]string{
				EnvBackendConfig:  `{"name":"claude","models":["opus"],"effort":"low","max_concurrency":2}`,
				EnvName:           "override",
				EnvCLIPath:        "/usr/local/bin/claude",
				EnvModels:         "haiku",
				EnvEffort:         "max",
				EnvMaxConcurrency: "8",
			},
			want: Config{Name: "override", CLIPath: "/usr/local/bin/claude", Models: []string{"haiku"}, Effort: "max", MaxConcurrency: 8},
		},
		{
			name: "empty list means catalogue",
			env:  map[string]string{EnvBackendConfig: `{"models":[]}`},
			want: Config{Name: DefaultName, CLIPath: "claude", MaxConcurrency: DefaultMaxConcurrency},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Load(env(tt.env))
			if err != nil {
				t.Fatal(err)
			}
			if got.Name != tt.want.Name || got.CLIPath != tt.want.CLIPath || !slices.Equal(got.Models, tt.want.Models) ||
				got.Effort != tt.want.Effort || got.MaxConcurrency != tt.want.MaxConcurrency {
				t.Errorf("Load = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestLoadErrors(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{name: "bad json", env: map[string]string{EnvBackendConfig: `{`}, want: EnvBackendConfig},
		{name: "name type", env: map[string]string{EnvBackendConfig: `{"name":1}`}, want: "name: want a string"},
		{name: "cli_path type", env: map[string]string{EnvBackendConfig: `{"cli_path":true}`}, want: "cli_path: want a string"},
		{name: "effort type", env: map[string]string{EnvBackendConfig: `{"effort":3}`}, want: "effort: want a string"},
		{name: "models type", env: map[string]string{EnvBackendConfig: `{"models":{"a":1}}`}, want: "models: want a list"},
		{name: "concurrency type", env: map[string]string{EnvBackendConfig: `{"max_concurrency":[1]}`}, want: "max_concurrency: want an integer"},
		{name: "concurrency string", env: map[string]string{EnvBackendConfig: `{"max_concurrency":"many"}`}, want: "is not an integer"},
		{name: "concurrency env", env: map[string]string{EnvMaxConcurrency: "x"}, want: EnvMaxConcurrency},
		{name: "concurrency zero", env: map[string]string{EnvMaxConcurrency: "0"}, want: "at least 1"},
		{name: "effort value", env: map[string]string{EnvEffort: "turbo"}, want: "unsupported reasoning effort"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(env(tt.env))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}
