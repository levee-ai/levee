// Package config handles YAML configuration parsing and validation for Levee.
package config

import (
	"bytes"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the top-level configuration structure for Levee.
type Config struct {
	Listen    ListenConfig     `yaml:"listen"`
	State     StateConfig      `yaml:"state"`
	Providers []ProviderConfig `yaml:"providers"`
	Agents    []AgentConfig    `yaml:"agents"`
	Defaults  DefaultsConfig   `yaml:"defaults"`
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
	Name     string         `yaml:"name"`
	Upstream string         `yaml:"upstream"`
	Timeouts TimeoutsConfig `yaml:"timeouts"`
}

// TimeoutsConfig defines the phase-split timeout policy for a provider (ADR-005).
// connect:         TCP connect (net.Dialer.Timeout).
// response_header: time to first byte; set on the streaming client only.
// idle:            streaming inter-chunk gap (consumed by the Session 6 watchdog).
// request:         non-streaming total duration cap (context.WithTimeout).
// Any empty field is filled with its default by normalize().
type TimeoutsConfig struct {
	Connect        string `yaml:"connect"`
	ResponseHeader string `yaml:"response_header"`
	Idle           string `yaml:"idle"`
	Request        string `yaml:"request"`
}

// AgentConfig defines a configured agent.
type AgentConfig struct {
	Name       string           `yaml:"name"`
	Identifier IdentifierConfig `yaml:"identifier"`
	Mode       string           `yaml:"mode"`
	Budgets    []BudgetConfig   `yaml:"budgets"`
}

// IdentifierConfig defines how to identify an agent.
// Only header-based identification is supported. The agent sets a custom header
// (recommended: X-Levee-Agent) and Levee matches the value against config.
type IdentifierConfig struct {
	Type        string `yaml:"type"`
	HeaderName  string `yaml:"header_name"`
	HeaderValue string `yaml:"header_value"`
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
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("cannot parse YAML: %w", err)
	}

	if errs := Validate(&cfg); len(errs) > 0 {
		return nil, &ValidationErrors{Errors: errs}
	}

	return &cfg, nil
}

// Validate checks the config against all validation rules and normalizes
// default values. After Validate returns with no errors, the config struct
// is fully normalized (e.g., empty mode fields are set to "enforce").
//
// NOTE: Validate mutates cfg even if validation fails. Normalization
// (whitespace trimming, default assignment) runs unconditionally before
// error checking. If Validate returns errors, the struct may be partially
// normalized and must not be used.
//
// Returns all errors found (does not stop at first error).
func Validate(cfg *Config) []string {
	normalize(cfg)
	var errs []string

	errs = append(errs, validateListen(cfg)...)
	errs = append(errs, validateState(cfg)...)
	errs = append(errs, validateProviders(cfg)...)
	errs = append(errs, validateAgents(cfg)...)
	errs = append(errs, validateDefaults(cfg)...)

	return errs
}

// normalize applies default values and trims whitespace on fields where
// user typos are common (currently: agent Mode only, since it is a new
// field likely typed by hand, unlike identifier.type which is always
// "header" and rarely edited). Called at the start of Validate() so that
// any caller of Validate() gets a fully-normalized struct on success.
// This function is idempotent: calling it multiple times produces the
// same result.
func normalize(cfg *Config) {
	for i := range cfg.Agents {
		cfg.Agents[i].Mode = strings.TrimSpace(cfg.Agents[i].Mode)
		if cfg.Agents[i].Mode == "" {
			cfg.Agents[i].Mode = "enforce"
		}
	}
	for i := range cfg.Providers {
		normalizeTimeouts(&cfg.Providers[i].Timeouts)
	}
}

// normalizeTimeouts fills any empty timeout field with its ADR-005 default.
// Trimmed and applied per-field so a partial timeouts block is completed, not
// rejected. Idempotent.
func normalizeTimeouts(timeouts *TimeoutsConfig) {
	timeouts.Connect = strings.TrimSpace(timeouts.Connect)
	if timeouts.Connect == "" {
		timeouts.Connect = "10s"
	}
	timeouts.ResponseHeader = strings.TrimSpace(timeouts.ResponseHeader)
	if timeouts.ResponseHeader == "" {
		timeouts.ResponseHeader = "120s"
	}
	timeouts.Idle = strings.TrimSpace(timeouts.Idle)
	if timeouts.Idle == "" {
		timeouts.Idle = "120s"
	}
	timeouts.Request = strings.TrimSpace(timeouts.Request)
	if timeouts.Request == "" {
		timeouts.Request = "600s"
	}
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

		errs = append(errs, validateTimeouts(prefix, p.Timeouts)...)
	}

	return errs
}

// timeoutBound describes one timeout field's validation limits.
type timeoutBound struct {
	name  string
	value string
	min   time.Duration
	max   time.Duration
}

// validateTimeouts checks each timeout field against its ADR-005 bounds.
// PRECONDITION: normalize() has run, so empty fields are already defaulted.
// Any field still empty here is a programmer error, not user input, but is
// reported rather than panicking.
func validateTimeouts(prefix string, timeouts TimeoutsConfig) []string {
	var errs []string
	bounds := []timeoutBound{
		{"connect", timeouts.Connect, 1 * time.Second, 60 * time.Second},
		{"response_header", timeouts.ResponseHeader, 5 * time.Second, 600 * time.Second},
		{"idle", timeouts.Idle, 5 * time.Second, 600 * time.Second},
		{"request", timeouts.Request, 5 * time.Second, 900 * time.Second},
	}
	for _, bound := range bounds {
		fieldPrefix := fmt.Sprintf("%s.timeouts.%s", prefix, bound.name)
		if bound.value == "" {
			errs = append(errs, fieldPrefix+": required")
			continue
		}
		duration, err := time.ParseDuration(bound.value)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: invalid duration %q", fieldPrefix, bound.value))
			continue
		}
		if duration < bound.min {
			errs = append(errs, fmt.Sprintf("%s: must be >= %s", fieldPrefix, bound.min))
		}
		if duration > bound.max {
			errs = append(errs, fmt.Sprintf("%s: must be <= %s", fieldPrefix, bound.max))
		}
	}
	return errs
}

// validModes defines the accepted values for the agent mode field.
var validModes = map[string]bool{
	"enforce":     true,
	"observe":     true,
	"passthrough": true,
}

// PRECONDITION: normalize() must be called before validateAgents().
// This function reads a.Mode assuming it is already trimmed and defaulted.
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

		// Mode is already normalized by normalize(). Validate its value.
		if !validModes[a.Mode] {
			errs = append(errs, fmt.Sprintf(
				"%s.mode: must be one of enforce, observe, passthrough, got %q", prefix, a.Mode,
			))
		}

		// Budgets required for enforce and observe, optional for passthrough.
		if a.Mode == "enforce" || a.Mode == "observe" {
			if len(a.Budgets) == 0 {
				errs = append(errs, fmt.Sprintf(
					"%s.budgets: at least one budget must be defined when mode is %q", prefix, a.Mode,
				))
			} else {
				for j, b := range a.Budgets {
					errs = append(errs, validateBudget(i, j, b)...)
				}
			}
		} else if a.Mode == "passthrough" && len(a.Budgets) > 0 {
			// Passthrough with budgets: validate them to catch typos,
			// but they are ignored at runtime.
			for j, b := range a.Budgets {
				errs = append(errs, validateBudget(i, j, b)...)
			}
		}
	}

	return errs
}

func validateIdentifier(agentIdx int, a AgentConfig, seen map[string]int) []string {
	var errs []string
	prefix := fmt.Sprintf("config error: agents[%d].identifier", agentIdx)

	if a.Identifier.Type == "" {
		errs = append(errs, prefix+".type: required")
		return errs
	}

	if a.Identifier.Type != "header" {
		errs = append(errs, fmt.Sprintf(
			"%s.type: must be \"header\", got %q",
			prefix, a.Identifier.Type,
		))
		return errs
	}

	if a.Identifier.HeaderName == "" {
		errs = append(errs, prefix+".header_name: required")
	}
	if a.Identifier.HeaderValue == "" {
		errs = append(errs, prefix+".header_value: required")
	}

	var identKey string
	if a.Identifier.HeaderName != "" && a.Identifier.HeaderValue != "" {
		// Canonicalize header name for duplicate detection since HTTP headers
		// are case-insensitive per RFC 9110.
		canonicalName := http.CanonicalHeaderKey(a.Identifier.HeaderName)
		identKey = fmt.Sprintf("header:%s=%s", canonicalName, a.Identifier.HeaderValue)
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
	hour, err := strconv.Atoi(parts[0])
	if err != nil {
		return false
	}
	minute, err := strconv.Atoi(parts[1])
	if err != nil {
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
