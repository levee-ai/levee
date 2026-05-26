// Package types defines shared domain types for the Levee proxy.
package types

import "time"

// IdentifierType defines how an agent is identified in incoming requests.
type IdentifierType string

const (
	IdentifierHeader       IdentifierType = "header"
	IdentifierAPIKeyPrefix IdentifierType = "api_key_prefix"
	IdentifierPathPrefix   IdentifierType = "path_prefix"
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

// BreachAction defines what happens when a budget is exceeded.
type BreachAction string

const (
	BreachBlock BreachAction = "block"
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
	Budgets    []Budget
	OnBreach   BreachAction
}

// Identifier defines how to match an incoming request to an agent.
type Identifier struct {
	Type        IdentifierType
	HeaderName  string
	HeaderValue string
	Prefix      string
}

// Budget defines a single budget constraint for an agent.
type Budget struct {
	Type       BudgetType
	Limit      float64
	Window     time.Duration
	WindowType WindowType
	ResetAt    string
}
