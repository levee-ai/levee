// Package config handles YAML configuration parsing and validation for Levee.
package config

import (
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the top-level configuration structure for Levee.
type Config struct {
	Listen    ListenConfig    `yaml:"listen"`
	State     StateConfig     `yaml:"state"`
	Providers []ProviderConfig `yaml:"providers"`
	Agents    []AgentConfig   `yaml:"agents"`
	Defaults  DefaultsConfig  `yaml:"defaults"`
}

// ListenConfig defines the network listeners.
type ListenConfig struct {
	ProxyPort int    `yaml:"proxy_port"`
	AdminPort int    `yaml:"admin_port"`
	AdminBind string `yaml:"admin_bind"`
}

// StateConfig defines state persistence settings.
type StateConfig struct {
	SnapshotPath     string `yaml:"snapshot_path"`
	SnapshotInterval string `yaml:"snapshot_interval"`
}

// ProviderConfig defines an upstream LLM provider.
type ProviderConfig struct {
	Name     string `yaml:"name"`
	Upstream string `yaml:"upstream"`
	Timeout  string `yaml:"timeout"`
}

// AgentConfig defines a configured agent.
type AgentConfig struct {
	Name       string           `yaml:"name"`
	Identifier IdentifierConfig `yaml:"identifier"`
	Budgets    []BudgetConfig   `yaml:"budgets"`
	OnBreach   string           `yaml:"on_breach"`
}

// IdentifierConfig defines how to identify an agent.
type IdentifierConfig struct {
	Type        string `yaml:"type"`
	HeaderName  string `yaml:"header_name"`
	HeaderValue string `yaml:"header_value"`
	Prefix      string `yaml:"prefix"`
}

// BudgetConfig defines a budget constraint.
type BudgetConfig struct {
	Type       string  `yaml:"type"`
	Limit      float64 `yaml:"limit"`
	Window     string  `yaml:"window"`
	WindowType string  `yaml:"window_type"`
	ResetAt    string  `yaml:"reset_at"`
}

// DefaultsConfig defines default behaviors.
type DefaultsConfig struct {
	UnknownAgent          string `yaml:"unknown_agent"`
	UnknownModelTokenizer string `yaml:"unknown_model_tokenizer"`
}

// validTokenizers is the set of recognized tiktoken encoding names.
var validTokenizers = map[string]bool{
	"cl100k_base": true,
	"p50k_base":   true,
	"p50k_edit":   true,
	"r50k_base":   true,
	"gpt2":        true,
	"o200k_base":  true,
}

// Load reads and validates a configuration file at the given path.
// It returns the parsed config or a list of validation errors.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read config file: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("cannot parse YAML: %w", err)
	}

	if errs := Validate(&cfg); len(errs) > 0 {
		return nil, &ValidationErrors{Errors: errs}
	}

	return &cfg, nil
}

// Validate checks the config against all validation rules.
// Returns all errors found (does not stop at first error).
func Validate(cfg *Config) []string {
	var errs []string

	errs = append(errs, validateListen(cfg)...)
	errs = append(errs, validateState(cfg)...)
	errs = append(errs, validateProviders(cfg)...)
	errs = append(errs, validateAgents(cfg)...)
	errs = append(errs, validateDefaults(cfg)...)

	return errs
}

func validateListen(cfg *Config) []string {
	var errs []string

	if cfg.Listen.ProxyPort < 1 || cfg.Listen.ProxyPort > 65535 {
		errs = append(errs, "config error: listen.proxy_port: must be 1-65535")
	}
	if cfg.Listen.AdminPort < 1 || cfg.Listen.AdminPort > 65535 {
		errs = append(errs, "config error: listen.admin_port: must be 1-65535")
	}
	if cfg.Listen.ProxyPort == cfg.Listen.AdminPort && cfg.Listen.ProxyPort != 0 {
		errs = append(errs, fmt.Sprintf(
			"config error: listen.proxy_port: must differ from admin_port (both are %d)",
			cfg.Listen.ProxyPort,
		))
	}

	return errs
}

func validateState(cfg *Config) []string {
	var errs []string

	if cfg.State.SnapshotPath == "" {
		errs = append(errs, "config error: state.snapshot_path: required")
		return errs
	}

	dir := filepath.Dir(cfg.State.SnapshotPath)
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		errs = append(errs, fmt.Sprintf(
			"config error: state.snapshot_path: parent directory %q does not exist", dir,
		))
	}

	interval, err := time.ParseDuration(cfg.State.SnapshotInterval)
	if err != nil {
		errs = append(errs, fmt.Sprintf(
			"config error: state.snapshot_interval: invalid duration %q", cfg.State.SnapshotInterval,
		))
	} else {
		if interval < 1*time.Second {
			errs = append(errs, "config error: state.snapshot_interval: must be >= 1s")
		}
		if interval > 5*time.Minute {
			errs = append(errs, "config error: state.snapshot_interval: must be <= 5m")
		}
	}

	return errs
}

func validateProviders(cfg *Config) []string {
	var errs []string

	if len(cfg.Providers) == 0 {
		errs = append(errs, "config error: providers: at least one provider must be defined")
		return errs
	}

	names := make(map[string]bool)
	for i, p := range cfg.Providers {
		prefix := fmt.Sprintf("config error: providers[%d]", i)

		if p.Name == "" {
			errs = append(errs, prefix+".name: required")
		} else if names[p.Name] {
			errs = append(errs, fmt.Sprintf("%s.name: duplicate provider name %q", prefix, p.Name))
		} else {
			names[p.Name] = true
		}

		if p.Upstream == "" {
			errs = append(errs, prefix+".upstream: required")
		} else {
			u, err := url.Parse(p.Upstream)
			if err != nil || u.Scheme != "https" || u.Host == "" {
				errs = append(errs, fmt.Sprintf(
					"%s.upstream: must be a valid URL with https:// scheme", prefix,
				))
			}
		}

		if p.Timeout == "" {
			errs = append(errs, prefix+".timeout: required")
		} else {
			d, err := time.ParseDuration(p.Timeout)
			if err != nil {
				errs = append(errs, fmt.Sprintf("%s.timeout: invalid duration %q", prefix, p.Timeout))
			} else {
				if d < 5*time.Second {
					errs = append(errs, prefix+".timeout: must be >= 5s")
				}
				if d > 600*time.Second {
					errs = append(errs, prefix+".timeout: must be <= 600s")
				}
			}
		}
	}

	return errs
}

func validateAgents(cfg *Config) []string {
	var errs []string

	if len(cfg.Agents) == 0 {
		errs = append(errs, "config error: agents: at least one agent must be defined")
		return errs
	}

	names := make(map[string]bool)
	identifiers := make(map[string]int) // maps identifier key to agent index

	for i, a := range cfg.Agents {
		prefix := fmt.Sprintf("config error: agents[%d]", i)

		if a.Name == "" {
			errs = append(errs, prefix+".name: required")
		} else if names[a.Name] {
			errs = append(errs, fmt.Sprintf("%s.name: duplicate agent name %q", prefix, a.Name))
		} else {
			names[a.Name] = true
		}

		errs = append(errs, validateIdentifier(i, a, identifiers)...)

		if len(a.Budgets) == 0 {
			errs = append(errs, prefix+".budgets: at least one budget must be defined")
		} else {
			for j, b := range a.Budgets {
				errs = append(errs, validateBudget(i, j, b)...)
			}
		}

		if a.OnBreach == "" {
			errs = append(errs, prefix+".on_breach: required")
		} else if a.OnBreach != "block" {
			errs = append(errs, fmt.Sprintf(
				"%s.on_breach: must be \"block\", got %q", prefix, a.OnBreach,
			))
		}
	}

	return errs
}

func validateIdentifier(agentIdx int, a AgentConfig, seen map[string]int) []string {
	var errs []string
	prefix := fmt.Sprintf("config error: agents[%d].identifier", agentIdx)

	validTypes := map[string]bool{
		"header":         true,
		"api_key_prefix": true,
		"path_prefix":    true,
	}

	if a.Identifier.Type == "" {
		errs = append(errs, prefix+".type: required")
		return errs
	}

	if !validTypes[a.Identifier.Type] {
		errs = append(errs, fmt.Sprintf(
			"%s.type: must be one of header, api_key_prefix, path_prefix, got %q",
			prefix, a.Identifier.Type,
		))
		return errs
	}

	var identKey string

	switch a.Identifier.Type {
	case "header":
		if a.Identifier.HeaderName == "" {
			errs = append(errs, prefix+".header_name: required for type \"header\"")
		}
		if a.Identifier.HeaderValue == "" {
			errs = append(errs, prefix+".header_value: required for type \"header\"")
		}
		if a.Identifier.HeaderName != "" && a.Identifier.HeaderValue != "" {
			identKey = fmt.Sprintf("header:%s=%s", a.Identifier.HeaderName, a.Identifier.HeaderValue)
		}
	case "api_key_prefix":
		if a.Identifier.Prefix == "" {
			errs = append(errs, prefix+".prefix: required for type \"api_key_prefix\"")
		} else {
			identKey = fmt.Sprintf("api_key_prefix:%s", a.Identifier.Prefix)
		}
	case "path_prefix":
		if a.Identifier.Prefix == "" {
			errs = append(errs, prefix+".prefix: required for type \"path_prefix\"")
		} else {
			identKey = fmt.Sprintf("path_prefix:%s", a.Identifier.Prefix)
		}
	}

	if identKey != "" {
		if otherIdx, exists := seen[identKey]; exists {
			errs = append(errs, fmt.Sprintf(
				"%s: duplicate identifier matches agents[%d] (%s)",
				prefix, otherIdx, identKey,
			))
		} else {
			seen[identKey] = agentIdx
		}
	}

	return errs
}

func validateBudget(agentIdx, budgetIdx int, b BudgetConfig) []string {
	var errs []string
	prefix := fmt.Sprintf("config error: agents[%d].budgets[%d]", agentIdx, budgetIdx)

	validTypes := map[string]bool{"tokens": true, "dollars": true}
	if b.Type == "" {
		errs = append(errs, prefix+".type: required")
	} else if !validTypes[b.Type] {
		errs = append(errs, fmt.Sprintf(
			"%s.type: must be one of tokens, dollars, got %q", prefix, b.Type,
		))
	}

	if b.Limit <= 0 {
		errs = append(errs, prefix+".limit: must be > 0")
	} else if b.Type == "tokens" && b.Limit != math.Floor(b.Limit) {
		errs = append(errs, prefix+".limit: must be an integer for token budgets")
	} else if b.Type == "dollars" {
		// Check at most 2 decimal places
		scaled := b.Limit * 100
		if math.Abs(scaled-math.Round(scaled)) > 0.001 {
			errs = append(errs, prefix+".limit: must have at most 2 decimal places for dollar budgets")
		}
	}

	if b.Window == "" {
		errs = append(errs, prefix+".window: required")
	} else {
		d, err := time.ParseDuration(b.Window)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s.window: invalid duration %q", prefix, b.Window))
		} else if d < 1*time.Second {
			errs = append(errs, fmt.Sprintf("%s.window: must be >= 1s, got %q", prefix, b.Window))
		}
	}

	validWindowTypes := map[string]bool{"rolling": true, "fixed": true}
	if b.WindowType == "" {
		errs = append(errs, prefix+".window_type: required")
	} else if !validWindowTypes[b.WindowType] {
		errs = append(errs, fmt.Sprintf(
			"%s.window_type: must be one of rolling, fixed, got %q", prefix, b.WindowType,
		))
	}

	if b.WindowType == "fixed" {
		if b.ResetAt == "" {
			errs = append(errs, prefix+".reset_at: required for fixed window_type")
		} else if !isValidResetAt(b.ResetAt) {
			errs = append(errs, fmt.Sprintf(
				"%s.reset_at: must be in HH:MMZ format, got %q", prefix, b.ResetAt,
			))
		}
	}

	return errs
}

func validateDefaults(cfg *Config) []string {
	var errs []string

	validActions := map[string]bool{"block": true, "passthrough": true}
	if cfg.Defaults.UnknownAgent == "" {
		errs = append(errs, "config error: defaults.unknown_agent: required")
	} else if !validActions[cfg.Defaults.UnknownAgent] {
		errs = append(errs, fmt.Sprintf(
			"config error: defaults.unknown_agent: must be one of block, passthrough, got %q",
			cfg.Defaults.UnknownAgent,
		))
	}

	if cfg.Defaults.UnknownModelTokenizer == "" {
		errs = append(errs, "config error: defaults.unknown_model_tokenizer: required")
	} else if !validTokenizers[cfg.Defaults.UnknownModelTokenizer] {
		errs = append(errs, fmt.Sprintf(
			"config error: defaults.unknown_model_tokenizer: must be a recognized tiktoken encoding, got %q",
			cfg.Defaults.UnknownModelTokenizer,
		))
	}

	return errs
}

// isValidResetAt checks if a string is in HH:MMZ format.
func isValidResetAt(s string) bool {
	if !strings.HasSuffix(s, "Z") {
		return false
	}
	s = strings.TrimSuffix(s, "Z")
	parts := strings.Split(s, ":")
	if len(parts) != 2 {
		return false
	}
	if len(parts[0]) != 2 || len(parts[1]) != 2 {
		return false
	}
	hour := 0
	minute := 0
	if _, err := fmt.Sscanf(parts[0], "%d", &hour); err != nil {
		return false
	}
	if _, err := fmt.Sscanf(parts[1], "%d", &minute); err != nil {
		return false
	}
	return hour >= 0 && hour <= 23 && minute >= 0 && minute <= 59
}

// ValidationErrors wraps multiple validation errors.
type ValidationErrors struct {
	Errors []string
}

func (e *ValidationErrors) Error() string {
	return strings.Join(e.Errors, "\n")
}
