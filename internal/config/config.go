// Package config resolves the plugin's backend configuration from the
// environment that llm-proxy provides.
//
// Sources, in increasing priority:
//
//  1. LLM_PROXY_BACKEND_CONFIG: a JSON object with the backend block's
//     attributes, set by llm-proxy.
//  2. LLM_PROXY_BACKEND_LABEL: the backend block label, used as the name
//     when the JSON object has none.
//  3. CLAUDECODE_* variables: per-setting overrides.
package config

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/thelonelyghost/llm-proxy-provider-claudecode/internal/claudecli"
)

// Environment variables read by Load.
const (
	EnvBackendConfig  = "LLM_PROXY_BACKEND_CONFIG"
	EnvBackendLabel   = "LLM_PROXY_BACKEND_LABEL"
	EnvName           = "CLAUDECODE_NAME"
	EnvCLIPath        = "CLAUDECODE_CLI_PATH"
	EnvModels         = "CLAUDECODE_MODELS"
	EnvEffort         = "CLAUDECODE_EFFORT"
	EnvMaxConcurrency = "CLAUDECODE_MAX_CONCURRENCY"
)

// Defaults for settings the operator does not set.
const (
	DefaultName           = "claudecode"
	DefaultMaxConcurrency = 4
)

// Config is the resolved backend configuration.
type Config struct {
	// Name is the backend label (llm-proxy routing prefix).
	Name string
	// CLIPath is the claude executable.
	CLIPath string
	// Models is the operator allow-list. Empty means every model in the
	// account's catalogue.
	Models []string
	// Effort is the default --effort level. Empty means the model default.
	Effort string
	// MaxConcurrency caps concurrent claude processes for chat requests.
	MaxConcurrency int
}

// attrs mirrors the backend block attributes in LLM_PROXY_BACKEND_CONFIG.
// Each value is kept raw so that strings and native JSON types both work.
type attrs struct {
	Name           json.RawMessage `json:"name"`
	CLIPath        json.RawMessage `json:"cli_path"`
	Models         json.RawMessage `json:"models"`
	Effort         json.RawMessage `json:"effort"`
	MaxConcurrency json.RawMessage `json:"max_concurrency"`
}

// Load resolves the configuration through getenv (os.Getenv in production).
func Load(getenv func(string) string) (Config, error) {
	cfg := Config{CLIPath: claudecli.DefaultPath, MaxConcurrency: DefaultMaxConcurrency}

	if raw := getenv(EnvBackendConfig); raw != "" {
		var a attrs
		if err := json.Unmarshal([]byte(raw), &a); err != nil {
			return Config{}, fmt.Errorf("%s: %w", EnvBackendConfig, err)
		}
		if err := a.apply(&cfg); err != nil {
			return Config{}, fmt.Errorf("%s: %w", EnvBackendConfig, err)
		}
	}
	if cfg.Name == "" {
		cfg.Name = getenv(EnvBackendLabel)
	}

	if v := getenv(EnvName); v != "" {
		cfg.Name = v
	}
	if v := getenv(EnvCLIPath); v != "" {
		cfg.CLIPath = v
	}
	if v := getenv(EnvModels); v != "" {
		cfg.Models = splitList(v)
	}
	if v := getenv(EnvEffort); v != "" {
		cfg.Effort = v
	}
	if v := getenv(EnvMaxConcurrency); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return Config{}, fmt.Errorf("%s: %q is not an integer", EnvMaxConcurrency, v)
		}
		cfg.MaxConcurrency = n
	}

	if cfg.Name == "" {
		cfg.Name = DefaultName
	}
	effort, err := claudecli.ParseEffort(cfg.Effort)
	if err != nil {
		return Config{}, fmt.Errorf("effort: %w", err)
	}
	cfg.Effort = effort
	if cfg.MaxConcurrency < 1 {
		return Config{}, fmt.Errorf("max_concurrency must be at least 1, got %d", cfg.MaxConcurrency)
	}
	return cfg, nil
}

func (a attrs) apply(cfg *Config) error {
	if err := decodeString(a.Name, "name", &cfg.Name); err != nil {
		return err
	}
	if err := decodeString(a.CLIPath, "cli_path", &cfg.CLIPath); err != nil {
		return err
	}
	if err := decodeString(a.Effort, "effort", &cfg.Effort); err != nil {
		return err
	}
	if len(a.Models) > 0 {
		var list []string
		var csv string
		switch {
		case json.Unmarshal(a.Models, &list) == nil:
			cfg.Models = cleanList(list)
		case json.Unmarshal(a.Models, &csv) == nil:
			cfg.Models = splitList(csv)
		default:
			return fmt.Errorf("models: want a list of strings or a comma-separated string")
		}
	}
	if len(a.MaxConcurrency) > 0 {
		var n int
		var s string
		switch {
		case json.Unmarshal(a.MaxConcurrency, &n) == nil:
			cfg.MaxConcurrency = n
		case json.Unmarshal(a.MaxConcurrency, &s) == nil:
			v, err := strconv.Atoi(s)
			if err != nil {
				return fmt.Errorf("max_concurrency: %q is not an integer", s)
			}
			cfg.MaxConcurrency = v
		default:
			return fmt.Errorf("max_concurrency: want an integer")
		}
	}
	return nil
}

func decodeString(raw json.RawMessage, key string, dst *string) error {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return fmt.Errorf("%s: want a string", key)
	}
	if s != "" {
		*dst = s
	}
	return nil
}

func splitList(s string) []string {
	return cleanList(strings.Split(s, ","))
}

func cleanList(in []string) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
