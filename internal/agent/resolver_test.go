package agent

import (
	"errors"
	"net/http"
	"testing"

	"github.com/levee-ai/levee/internal/config"
)

func twoAgents() []config.AgentConfig {
	return []config.AgentConfig{
		{
			Name: "researcher",
			Identifier: config.IdentifierConfig{
				Type: "header", HeaderName: "X-Levee-Agent", HeaderValue: "researcher",
			},
		},
		{
			Name: "customer-bot",
			Identifier: config.IdentifierConfig{
				Type: "header", HeaderName: "X-Levee-Agent", HeaderValue: "customer-bot",
			},
		},
	}
}

func TestResolve(t *testing.T) {
	tests := []struct {
		name        string
		headerName  string
		headerValue string
		setHeader   bool
		wantAgent   string
		wantErr     error
	}{
		{"matches by value", "X-Levee-Agent", "researcher", true, "researcher", nil},
		{"second agent", "X-Levee-Agent", "customer-bot", true, "customer-bot", nil},
		{"case-insensitive header name", "x-levee-agent", "researcher", true, "researcher", nil},
		{"missing header", "X-Levee-Agent", "", false, "", ErrUnknownAgent},
		{"unknown value", "X-Levee-Agent", "ghost", true, "", ErrUnknownAgent},
		{"value is case-sensitive", "X-Levee-Agent", "Researcher", true, "", ErrUnknownAgent},
	}
	resolver := NewResolver(twoAgents())
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request, _ := http.NewRequest(http.MethodPost, "/openai/v1/chat/completions", nil)
			if test.setHeader {
				request.Header.Set(test.headerName, test.headerValue)
			}
			agentName, err := resolver.Resolve(request)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("error: got %v, want %v", err, test.wantErr)
			}
			if agentName != test.wantAgent {
				t.Fatalf("agent: got %q, want %q", agentName, test.wantAgent)
			}
		})
	}
}

func TestResolveDifferentHeaderNames(t *testing.T) {
	// Config permits per-agent header names. The resolver must check each.
	agents := []config.AgentConfig{
		{Name: "a", Identifier: config.IdentifierConfig{Type: "header", HeaderName: "X-Levee-Agent", HeaderValue: "a"}},
		{Name: "b", Identifier: config.IdentifierConfig{Type: "header", HeaderName: "X-Team", HeaderValue: "b"}},
	}
	resolver := NewResolver(agents)
	request, _ := http.NewRequest(http.MethodPost, "/openai", nil)
	request.Header.Set("X-Team", "b")
	agentName, err := resolver.Resolve(request)
	if err != nil || agentName != "b" {
		t.Fatalf("got (%q, %v), want (b, nil)", agentName, err)
	}
}
