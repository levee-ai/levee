// Package agent handles agent identification and resolution from HTTP requests.
package agent

import (
	"errors"
	"net/http"

	"github.com/levee-ai/levee/internal/config"
)

// ErrUnknownAgent is returned when a request carries no recognized agent
// header. The proxy layer maps this to block or passthrough based on
// defaults.unknown_agent. The resolver does not encode that policy.
var ErrUnknownAgent = errors.New("unknown agent")

// Resolver maps an incoming request to a configured agent name using
// header-based identification only (ADR-003). It is built once at startup and
// is read-only afterward, so Resolve needs no synchronization.
type Resolver struct {
	// index maps canonical header name -> header value -> agent name.
	index map[string]map[string]string
}

// NewResolver builds a Resolver from the agent list. Header names are
// canonicalized (RFC 9110 case-insensitivity), and header values are matched
// exactly. Config validation guarantees identifier uniqueness, so later
// duplicates do not occur here.
func NewResolver(agents []config.AgentConfig) *Resolver {
	index := make(map[string]map[string]string)
	for _, agent := range agents {
		canonicalName := http.CanonicalHeaderKey(agent.Identifier.HeaderName)
		valueMap, ok := index[canonicalName]
		if !ok {
			valueMap = make(map[string]string)
			index[canonicalName] = valueMap
		}
		valueMap[agent.Identifier.HeaderValue] = agent.Name
	}
	return &Resolver{index: index}
}

// Resolve returns the agent name identified by the request headers, or
// ErrUnknownAgent if no header matches. Header lookup via http.Header.Get is
// already canonical, so no per-call canonicalization is needed.
func (resolver *Resolver) Resolve(request *http.Request) (string, error) {
	for canonicalName, valueMap := range resolver.index {
		value := request.Header.Get(canonicalName)
		if value == "" {
			continue
		}
		if agentName, ok := valueMap[value]; ok {
			return agentName, nil
		}
	}
	return "", ErrUnknownAgent
}
