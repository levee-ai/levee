// Package types defines shared domain types for the Levee proxy.
package types

import "time"

// IdentifierType defines how an agent is identified in incoming requests.
type IdentifierType string

const (
	IdentifierHeader IdentifierType = "header"
)

// BudgetType defines the unit of measurement for a budget.
type BudgetType string

const (
	BudgetTokens  BudgetType = "tokens"
	BudgetDollars BudgetType = "dollars"
)

// WindowType defines how the budget window resets.
type WindowType string

const (
	WindowRolling WindowType = "rolling"
	WindowFixed   WindowType = "fixed"
)

// EnforcementMode defines how an agent's budget is enforced.
type EnforcementMode string

const (
	ModeEnforce     EnforcementMode = "enforce"
	ModeObserve     EnforcementMode = "observe"
	ModePassthrough EnforcementMode = "passthrough"
)

// UnknownAgentAction defines behavior for unidentified agents.
type UnknownAgentAction string

const (
	UnknownAgentBlock       UnknownAgentAction = "block"
	UnknownAgentPassthrough UnknownAgentAction = "passthrough"
)

// Provider represents an upstream LLM API provider.
type Provider struct {
	Name     string
	Upstream string
	Timeout  time.Duration
}

// Agent represents a configured agent with its identification and budgets.
type Agent struct {
	Name       string
	Identifier Identifier
	Mode       EnforcementMode
	Budgets    []Budget
}

// Identifier defines how to match an incoming request to an agent.
type Identifier struct {
	Type        IdentifierType
	HeaderName  string
	HeaderValue string
}

// Budget defines a single budget constraint for an agent.
type Budget struct {
	Type       BudgetType
	Limit      float64
	Window     time.Duration
	WindowType WindowType
	ResetAt    string
}
