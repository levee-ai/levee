package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func validConfig() *Config {
	return &Config{
		Listen: ListenConfig{
			ProxyPort: 8080,
			AdminPort: 9090,
			AdminBind: "127.0.0.1",
		},
		State: StateConfig{
			SnapshotPath:     filepath.Join(os.TempDir(), "levee-test-state.json"),
			SnapshotInterval: "30s",
		},
		Providers: []ProviderConfig{
			{
				Name:     "openai",
				Upstream: "https://api.openai.com",
				Timeout:  "120s",
			},
		},
		Agents: []AgentConfig{
			{
				Name: "researcher",
				Identifier: IdentifierConfig{
					Type:        "header",
					HeaderName:  "X-Agent-Id",
					HeaderValue: "researcher",
				},
				Mode: "enforce",
				Budgets: []BudgetConfig{
					{
						Type:       "tokens",
						Limit:      1000000,
						Window:     "1h",
						WindowType: "rolling",
					},
				},
			},
		},
		Defaults: DefaultsConfig{
			UnknownAgent:          "block",
			UnknownModelTokenizer: "cl100k_base",
		},
	}
}

func TestValidate_ValidConfig(t *testing.T) {
	cfg := validConfig()
	errs := Validate(cfg)
	if len(errs) > 0 {
		t.Errorf("expected no errors for valid config, got:\n%s", strings.Join(errs, "\n"))
	}
}

func TestValidate_NormalizesEmptyMode(t *testing.T) {
	cfg := validConfig()
	cfg.Agents[0].Mode = ""

	errs := Validate(cfg)
	if len(errs) > 0 {
		t.Fatalf("expected no errors, got:\n%s", strings.Join(errs, "\n"))
	}

	// Validate must mutate the struct to set the default.
	if cfg.Agents[0].Mode != "enforce" {
		t.Errorf("expected Validate() to normalize empty mode to 'enforce', got %q", cfg.Agents[0].Mode)
	}
}

func TestValidate_TrimsWhitespaceFromMode(t *testing.T) {
	cfg := validConfig()
	cfg.Agents[0].Mode = "  observe  "

	errs := Validate(cfg)
	if len(errs) > 0 {
		t.Fatalf("expected no errors, got:\n%s", strings.Join(errs, "\n"))
	}

	// Validate must trim whitespace from mode.
	if cfg.Agents[0].Mode != "observe" {
		t.Errorf("expected Validate() to trim mode to 'observe', got %q", cfg.Agents[0].Mode)
	}
}

func TestValidate_WhitespaceOnlyModeDefaultsToEnforce(t *testing.T) {
	cfg := validConfig()
	cfg.Agents[0].Mode = "   "

	errs := Validate(cfg)
	if len(errs) > 0 {
		t.Fatalf("expected no errors, got:\n%s", strings.Join(errs, "\n"))
	}

	// Whitespace-only is trimmed to empty, then defaulted to "enforce".
	if cfg.Agents[0].Mode != "enforce" {
		t.Errorf("expected whitespace-only mode to default to 'enforce', got %q", cfg.Agents[0].Mode)
	}
}

func TestValidate_TabAndNewlineInMode(t *testing.T) {
	cfg := validConfig()
	cfg.Agents[0].Mode = "\tenforce\n"

	errs := Validate(cfg)
	if len(errs) > 0 {
		t.Fatalf("expected no errors, got:\n%s", strings.Join(errs, "\n"))
	}

	if cfg.Agents[0].Mode != "enforce" {
		t.Errorf("expected tab/newline to be trimmed, got %q", cfg.Agents[0].Mode)
	}
}

func TestValidate_IsIdempotent(t *testing.T) {
	cfg := validConfig()
	cfg.Agents[0].Mode = "  observe  "

	errs1 := Validate(cfg)
	if len(errs1) > 0 {
		t.Fatalf("first Validate() returned errors: %s", strings.Join(errs1, "\n"))
	}
	mode1 := cfg.Agents[0].Mode

	errs2 := Validate(cfg)
	if len(errs2) > 0 {
		t.Fatalf("second Validate() returned errors: %s", strings.Join(errs2, "\n"))
	}

	if cfg.Agents[0].Mode != mode1 {
		t.Errorf("Validate() is not idempotent: first=%q, second=%q", mode1, cfg.Agents[0].Mode)
	}
}

func TestValidate_Listen(t *testing.T) {
	tests := []struct {
		name      string
		modify    func(*Config)
		wantError string
	}{
		{
			name: "proxy_port too low",
			modify: func(c *Config) {
				c.Listen.ProxyPort = 0
			},
			wantError: "listen.proxy_port: must be 1-65535",
		},
		{
			name: "proxy_port too high",
			modify: func(c *Config) {
				c.Listen.ProxyPort = 70000
			},
			wantError: "listen.proxy_port: must be 1-65535",
		},
		{
			name: "admin_port too low",
			modify: func(c *Config) {
				c.Listen.AdminPort = -1
			},
			wantError: "listen.admin_port: must be 1-65535",
		},
		{
			name: "ports are the same",
			modify: func(c *Config) {
				c.Listen.ProxyPort = 8080
				c.Listen.AdminPort = 8080
			},
			wantError: "must differ from admin_port",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			tt.modify(cfg)
			errs := Validate(cfg)
			assertContainsError(t, errs, tt.wantError)
		})
	}
}

func TestValidate_State(t *testing.T) {
	tests := []struct {
		name      string
		modify    func(*Config)
		wantError string
	}{
		{
			name: "empty snapshot_path",
			modify: func(c *Config) {
				c.State.SnapshotPath = ""
			},
			wantError: "state.snapshot_path: required",
		},
		{
			name: "nonexistent parent directory",
			modify: func(c *Config) {
				c.State.SnapshotPath = "/nonexistent/dir/state.json"
			},
			wantError: "parent directory",
		},
		{
			name: "invalid snapshot_interval",
			modify: func(c *Config) {
				c.State.SnapshotInterval = "abc"
			},
			wantError: "invalid duration",
		},
		{
			name: "snapshot_interval too short",
			modify: func(c *Config) {
				c.State.SnapshotInterval = "500ms"
			},
			wantError: "must be >= 1s",
		},
		{
			name: "snapshot_interval too long",
			modify: func(c *Config) {
				c.State.SnapshotInterval = "10m"
			},
			wantError: "must be <= 5m",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			tt.modify(cfg)
			errs := Validate(cfg)
			assertContainsError(t, errs, tt.wantError)
		})
	}
}

func TestValidate_Providers(t *testing.T) {
	tests := []struct {
		name      string
		modify    func(*Config)
		wantError string
	}{
		{
			name: "no providers",
			modify: func(c *Config) {
				c.Providers = nil
			},
			wantError: "at least one provider must be defined",
		},
		{
			name: "empty provider name",
			modify: func(c *Config) {
				c.Providers[0].Name = ""
			},
			wantError: ".name: required",
		},
		{
			name: "duplicate provider name",
			modify: func(c *Config) {
				c.Providers = append(c.Providers, ProviderConfig{
					Name:     "openai",
					Upstream: "https://api.openai.com",
					Timeout:  "30s",
				})
			},
			wantError: "duplicate provider name",
		},
		{
			name: "http upstream (not https)",
			modify: func(c *Config) {
				c.Providers[0].Upstream = "http://api.openai.com"
			},
			wantError: "must be a valid URL with https:// scheme",
		},
		{
			name: "empty upstream",
			modify: func(c *Config) {
				c.Providers[0].Upstream = ""
			},
			wantError: ".upstream: required",
		},
		{
			name: "timeout too short",
			modify: func(c *Config) {
				c.Providers[0].Timeout = "2s"
			},
			wantError: "must be >= 5s",
		},
		{
			name: "timeout too long",
			modify: func(c *Config) {
				c.Providers[0].Timeout = "700s"
			},
			wantError: "must be <= 600s",
		},
		{
			name: "empty timeout",
			modify: func(c *Config) {
				c.Providers[0].Timeout = ""
			},
			wantError: ".timeout: required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			tt.modify(cfg)
			errs := Validate(cfg)
			assertContainsError(t, errs, tt.wantError)
		})
	}
}

func TestValidate_Agents(t *testing.T) {
	tests := []struct {
		name      string
		modify    func(*Config)
		wantError string
	}{
		{
			name: "no agents",
			modify: func(c *Config) {
				c.Agents = nil
			},
			wantError: "at least one agent must be defined",
		},
		{
			name: "empty agent name",
			modify: func(c *Config) {
				c.Agents[0].Name = ""
			},
			wantError: ".name: required",
		},
		{
			name: "duplicate agent name",
			modify: func(c *Config) {
				c.Agents = append(c.Agents, AgentConfig{
					Name: "researcher",
					Identifier: IdentifierConfig{
						Type:        "header",
						HeaderName:  "X-Agent-Id",
						HeaderValue: "other",
					},
					Budgets: []BudgetConfig{
						{Type: "tokens", Limit: 100, Window: "1h", WindowType: "rolling"},
					},
					Mode: "enforce",
				})
			},
			wantError: "duplicate agent name",
		},
		{
			name: "invalid identifier type",
			modify: func(c *Config) {
				c.Agents[0].Identifier.Type = "magic"
			},
			wantError: "must be \"header\"",
		},
		{
			name: "header type missing header_name",
			modify: func(c *Config) {
				c.Agents[0].Identifier.HeaderName = ""
			},
			wantError: "header_name: required",
		},
		{
			name: "header type missing header_value",
			modify: func(c *Config) {
				c.Agents[0].Identifier.HeaderValue = ""
			},
			wantError: "header_value: required",
		},
		{
			name: "duplicate identifiers",
			modify: func(c *Config) {
				c.Agents = append(c.Agents, AgentConfig{
					Name: "duplicate-agent",
					Identifier: IdentifierConfig{
						Type:        "header",
						HeaderName:  "X-Agent-Id",
						HeaderValue: "researcher",
					},
					Budgets: []BudgetConfig{
						{Type: "tokens", Limit: 100, Window: "1h", WindowType: "rolling"},
					},
					Mode: "enforce",
				})
			},
			wantError: "duplicate identifier",
		},
		{
			name: "duplicate identifiers case-insensitive header",
			modify: func(c *Config) {
				c.Agents = append(c.Agents, AgentConfig{
					Name: "case-dup-agent",
					Identifier: IdentifierConfig{
						Type:        "header",
						HeaderName:  "x-agent-id",
						HeaderValue: "researcher",
					},
					Budgets: []BudgetConfig{
						{Type: "tokens", Limit: 100, Window: "1h", WindowType: "rolling"},
					},
					Mode: "enforce",
				})
			},
			wantError: "duplicate identifier",
		},
		{
			name: "no budgets in enforce mode",
			modify: func(c *Config) {
				c.Agents[0].Mode = "enforce"
				c.Agents[0].Budgets = nil
			},
			wantError: "at least one budget must be defined when mode is \"enforce\"",
		},
		{
			name: "no budgets in observe mode",
			modify: func(c *Config) {
				c.Agents[0].Mode = "observe"
				c.Agents[0].Budgets = nil
			},
			wantError: "at least one budget must be defined when mode is \"observe\"",
		},
		{
			name: "no budgets in passthrough mode is valid",
			modify: func(c *Config) {
				c.Agents[0].Mode = "passthrough"
				c.Agents[0].Budgets = nil
			},
			wantError: "", // no error expected
		},
		{
			name: "invalid mode",
			modify: func(c *Config) {
				c.Agents[0].Mode = "unlimited"
			},
			wantError: "must be one of enforce, observe, passthrough",
		},
		{
			name: "mode is case-sensitive",
			modify: func(c *Config) {
				c.Agents[0].Mode = "Enforce"
			},
			wantError: "must be one of enforce, observe, passthrough",
		},
		{
			name: "mode with trailing whitespace is trimmed",
			modify: func(c *Config) {
				c.Agents[0].Mode = "enforce "
			},
			wantError: "", // trimmed to "enforce", valid
		},
		{
			name: "empty mode defaults to enforce",
			modify: func(c *Config) {
				c.Agents[0].Mode = ""
			},
			wantError: "", // no error, defaults to enforce and budgets exist
		},
		{
			name: "passthrough with invalid budget still validates budgets",
			modify: func(c *Config) {
				c.Agents[0].Mode = "passthrough"
				c.Agents[0].Budgets = []BudgetConfig{
					{Type: "invalid", Limit: -1, Window: "abc", WindowType: "sliding"},
				}
			},
			wantError: "must be one of tokens, dollars",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			tt.modify(cfg)
			errs := Validate(cfg)
			if tt.wantError == "" {
				if len(errs) > 0 {
					t.Errorf("expected no errors, got:\n%s", strings.Join(errs, "\n"))
				}
			} else {
				assertContainsError(t, errs, tt.wantError)
			}
		})
	}
}

func TestValidate_Budgets(t *testing.T) {
	tests := []struct {
		name      string
		modify    func(*Config)
		wantError string
	}{
		{
			name: "invalid budget type",
			modify: func(c *Config) {
				c.Agents[0].Budgets[0].Type = "requests"
			},
			wantError: "must be one of tokens, dollars",
		},
		{
			name: "zero limit",
			modify: func(c *Config) {
				c.Agents[0].Budgets[0].Limit = 0
			},
			wantError: "must be > 0",
		},
		{
			name: "negative limit",
			modify: func(c *Config) {
				c.Agents[0].Budgets[0].Limit = -100
			},
			wantError: "must be > 0",
		},
		{
			name: "fractional token limit",
			modify: func(c *Config) {
				c.Agents[0].Budgets[0].Limit = 100.5
			},
			wantError: "must be an integer for token budgets",
		},
		{
			name: "dollar limit too many decimals",
			modify: func(c *Config) {
				c.Agents[0].Budgets[0].Type = "dollars"
				c.Agents[0].Budgets[0].Limit = 50.123
			},
			wantError: "must have at most 2 decimal places",
		},
		{
			name: "valid dollar limit with 2 decimals",
			modify: func(c *Config) {
				c.Agents[0].Budgets[0].Type = "dollars"
				c.Agents[0].Budgets[0].Limit = 50.99
			},
			wantError: "", // no error expected
		},
		{
			name: "empty window",
			modify: func(c *Config) {
				c.Agents[0].Budgets[0].Window = ""
			},
			wantError: "window: required",
		},
		{
			name: "window too short",
			modify: func(c *Config) {
				c.Agents[0].Budgets[0].Window = "500ms"
			},
			wantError: "must be >= 1s",
		},
		{
			name: "invalid window_type",
			modify: func(c *Config) {
				c.Agents[0].Budgets[0].WindowType = "sliding"
			},
			wantError: "must be one of rolling, fixed",
		},
		{
			name: "fixed window without reset_at",
			modify: func(c *Config) {
				c.Agents[0].Budgets[0].WindowType = "fixed"
				c.Agents[0].Budgets[0].ResetAt = ""
			},
			wantError: "reset_at: required for fixed window_type",
		},
		{
			name: "fixed window with invalid reset_at",
			modify: func(c *Config) {
				c.Agents[0].Budgets[0].WindowType = "fixed"
				c.Agents[0].Budgets[0].ResetAt = "25:00Z"
			},
			wantError: "must be in HH:MMZ format",
		},
		{
			name: "fixed window with valid reset_at",
			modify: func(c *Config) {
				c.Agents[0].Budgets[0].WindowType = "fixed"
				c.Agents[0].Budgets[0].ResetAt = "00:00Z"
			},
			wantError: "", // no error expected
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			tt.modify(cfg)
			errs := Validate(cfg)
			if tt.wantError == "" {
				if len(errs) > 0 {
					t.Errorf("expected no errors, got:\n%s", strings.Join(errs, "\n"))
				}
			} else {
				assertContainsError(t, errs, tt.wantError)
			}
		})
	}
}

func TestValidate_Defaults(t *testing.T) {
	tests := []struct {
		name      string
		modify    func(*Config)
		wantError string
	}{
		{
			name: "empty unknown_agent",
			modify: func(c *Config) {
				c.Defaults.UnknownAgent = ""
			},
			wantError: "unknown_agent: required",
		},
		{
			name: "invalid unknown_agent",
			modify: func(c *Config) {
				c.Defaults.UnknownAgent = "allow"
			},
			wantError: "must be one of block, passthrough",
		},
		{
			name: "empty unknown_model_tokenizer",
			modify: func(c *Config) {
				c.Defaults.UnknownModelTokenizer = ""
			},
			wantError: "unknown_model_tokenizer: required",
		},
		{
			name: "invalid tokenizer",
			modify: func(c *Config) {
				c.Defaults.UnknownModelTokenizer = "gpt5_turbo"
			},
			wantError: "must be a recognized tiktoken encoding",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			tt.modify(cfg)
			errs := Validate(cfg)
			assertContainsError(t, errs, tt.wantError)
		})
	}
}

func TestLoad_ValidFile(t *testing.T) {
	content := `
listen:
  proxy_port: 8080
  admin_port: 9090
  admin_bind: "127.0.0.1"

state:
  snapshot_path: "` + filepath.Join(os.TempDir(), "levee-test.json") + `"
  snapshot_interval: "30s"

providers:
  - name: openai
    upstream: "https://api.openai.com"
    timeout: "120s"

agents:
  - name: "tester"
    identifier:
      type: header
      header_name: "X-Agent-Id"
      header_value: "tester"
    mode: enforce
    budgets:
      - type: tokens
        limit: 500000
        window: "1h"
        window_type: rolling

defaults:
  unknown_agent: block
  unknown_model_tokenizer: "cl100k_base"
`

	tmpFile := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(tmpFile, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}

	cfg, err := Load(tmpFile)
	if err != nil {
		t.Fatalf("Load() returned error: %v", err)
	}
	if cfg.Listen.ProxyPort != 8080 {
		t.Errorf("expected proxy_port 8080, got %d", cfg.Listen.ProxyPort)
	}
	if cfg.Providers[0].Name != "openai" {
		t.Errorf("expected provider name 'openai', got %q", cfg.Providers[0].Name)
	}
	if cfg.Agents[0].Mode != "enforce" {
		t.Errorf("expected mode 'enforce', got %q", cfg.Agents[0].Mode)
	}
}

func TestLoad_UnknownFieldsRejected(t *testing.T) {
	content := `
listen:
  proxy_port: 8080
  admin_port: 9090
  admin_bind: "127.0.0.1"
  typo_field: true

state:
  snapshot_path: "` + filepath.Join(os.TempDir(), "levee-test.json") + `"
  snapshot_interval: "30s"

providers:
  - name: openai
    upstream: "https://api.openai.com"
    timeout: "120s"

agents:
  - name: "tester"
    identifier:
      type: header
      header_name: "X-Agent-Id"
      header_value: "tester"
    mode: enforce
    budgets:
      - type: tokens
        limit: 500000
        window: "1h"
        window_type: rolling

defaults:
  unknown_agent: block
  unknown_model_tokenizer: "cl100k_base"
`

	tmpFile := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(tmpFile, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}

	_, err := Load(tmpFile)
	if err == nil {
		t.Fatal("expected error for unknown field, got nil")
	}
	if !strings.Contains(err.Error(), "cannot parse YAML") {
		t.Errorf("expected YAML parse error for unknown field, got: %v", err)
	}
}

func TestLoad_InvalidYAML(t *testing.T) {
	tmpFile := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(tmpFile, []byte(":::invalid"), 0644); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}

	_, err := Load(tmpFile)
	if err == nil {
		t.Fatal("expected error for invalid YAML, got nil")
	}
	if !strings.Contains(err.Error(), "cannot parse YAML") {
		t.Errorf("expected YAML parse error, got: %v", err)
	}
}

func TestLoad_FileNotFound(t *testing.T) {
	_, err := Load("/nonexistent/path/config.yaml")
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
	if !strings.Contains(err.Error(), "cannot read config file") {
		t.Errorf("expected read error, got: %v", err)
	}
}

func TestIsValidResetAt(t *testing.T) {
	tests := []struct {
		input string
		valid bool
	}{
		{"00:00Z", true},
		{"23:59Z", true},
		{"12:30Z", true},
		{"24:00Z", false},
		{"00:60Z", false},
		{"0:00Z", false},
		{"00:0Z", false},
		{"12:30", false},
		{"abc", false},
		{"", false},
		{"0a:00Z", false},
		{"12:3xZ", false},
		{"1x:30Z", false},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := isValidResetAt(tt.input)
			if got != tt.valid {
				t.Errorf("isValidResetAt(%q) = %v, want %v", tt.input, got, tt.valid)
			}
		})
	}
}

func TestLoad_ModeDefaultsToEnforce(t *testing.T) {
	content := `
listen:
  proxy_port: 8080
  admin_port: 9090
  admin_bind: "127.0.0.1"

state:
  snapshot_path: "` + filepath.Join(os.TempDir(), "levee-test.json") + `"
  snapshot_interval: "30s"

providers:
  - name: openai
    upstream: "https://api.openai.com"
    timeout: "120s"

agents:
  - name: "no-mode-agent"
    identifier:
      type: header
      header_name: "X-Levee-Agent"
      header_value: "no-mode"
    budgets:
      - type: tokens
        limit: 100000
        window: "1h"
        window_type: rolling

defaults:
  unknown_agent: block
  unknown_model_tokenizer: "cl100k_base"
`

	tmpFile := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(tmpFile, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}

	cfg, err := Load(tmpFile)
	if err != nil {
		t.Fatalf("Load() returned error: %v", err)
	}
	if cfg.Agents[0].Mode != "enforce" {
		t.Errorf("expected mode to default to 'enforce', got %q", cfg.Agents[0].Mode)
	}
}

func TestLoad_PassthroughWithoutBudgets(t *testing.T) {
	content := `
listen:
  proxy_port: 8080
  admin_port: 9090
  admin_bind: "127.0.0.1"

state:
  snapshot_path: "` + filepath.Join(os.TempDir(), "levee-test.json") + `"
  snapshot_interval: "30s"

providers:
  - name: openai
    upstream: "https://api.openai.com"
    timeout: "120s"

agents:
  - name: "passthrough-agent"
    identifier:
      type: header
      header_name: "X-Levee-Agent"
      header_value: "passthrough-agent"
    mode: passthrough

defaults:
  unknown_agent: block
  unknown_model_tokenizer: "cl100k_base"
`

	tmpFile := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(tmpFile, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}

	cfg, err := Load(tmpFile)
	if err != nil {
		t.Fatalf("Load() returned error: %v", err)
	}
	if cfg.Agents[0].Mode != "passthrough" {
		t.Errorf("expected mode 'passthrough', got %q", cfg.Agents[0].Mode)
	}
	if len(cfg.Agents[0].Budgets) != 0 {
		t.Errorf("expected no budgets for passthrough agent, got %d", len(cfg.Agents[0].Budgets))
	}
}

func assertContainsError(t *testing.T, errs []string, substr string) {
	t.Helper()
	if substr == "" {
		return
	}
	for _, e := range errs {
		if strings.Contains(e, substr) {
			return
		}
	}
	t.Errorf("expected error containing %q, got errors:\n%s", substr, strings.Join(errs, "\n"))
}
