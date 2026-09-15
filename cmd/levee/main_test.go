package main

import (
	"reflect"
	"strings"
	"testing"

	"github.com/levee-ai/levee/internal/config"
)

func TestPlaintextUpstreams(t *testing.T) {
	tests := []struct {
		name      string
		providers []config.ProviderConfig
		want      []plaintextUpstream
	}{
		{
			name: "lowercase http on loopback is reported",
			providers: []config.ProviderConfig{
				{Name: "mock", Upstream: "http://127.0.0.1:9999"},
			},
			want: []plaintextUpstream{{Name: "mock", Upstream: "http://127.0.0.1:9999"}},
		},
		{
			// url.Parse lowercases the scheme, so config validation accepts a
			// mixed-case scheme and the warning MUST fire for it too. A
			// strings.HasPrefix check on "http://" would skip this silently and
			// leave the operator with no warning about a plaintext hop.
			// Redacted() also normalizes the scheme, which is why the expected
			// upstream is lowercase here.
			name: "mixed-case HTTP on loopback is reported",
			providers: []config.ProviderConfig{
				{Name: "mock", Upstream: "HTTP://127.0.0.1:9999"},
			},
			want: []plaintextUpstream{{Name: "mock", Upstream: "http://127.0.0.1:9999"}},
		},
		{
			name: "https upstream is silent",
			providers: []config.ProviderConfig{
				{Name: "openai", Upstream: "https://api.openai.com"},
			},
			want: nil,
		},
		{
			name: "mixed-case HTTPS upstream is silent",
			providers: []config.ProviderConfig{
				{Name: "openai", Upstream: "Https://api.openai.com"},
			},
			want: nil,
		},
		{
			// Every provider is inspected, not only the first, and the result
			// keeps config order. A loop that stopped at the first match or
			// only looked at index 0 fails here.
			name: "every plaintext provider in a mixed list is reported",
			providers: []config.ProviderConfig{
				{Name: "openai", Upstream: "https://api.openai.com"},
				{Name: "mock-one", Upstream: "http://127.0.0.1:9001"},
				{Name: "anthropic", Upstream: "https://api.anthropic.com"},
				{Name: "mock-two", Upstream: "HTTP://[::1]:9002"},
			},
			want: []plaintextUpstream{
				{Name: "mock-one", Upstream: "http://127.0.0.1:9001"},
				{Name: "mock-two", Upstream: "http://[::1]:9002"},
			},
		},
		{
			name:      "no providers reports nothing",
			providers: nil,
			want:      nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := plaintextUpstreams(tt.providers)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("plaintextUpstreams() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestPlaintextUpstreamsRedactsCredentials guards the startup warning against
// leaking a credential. url.Hostname() strips userinfo before the loopback
// check, so an upstream carrying an embedded password passes config validation.
// Logging the raw config string would then put that password in the WARN line
// in cleartext, which go-standards forbids at any level. Do not replace
// Redacted() with the raw string for scheme fidelity.
func TestPlaintextUpstreamsRedactsCredentials(t *testing.T) {
	const password = "supersecretpassword"
	providers := []config.ProviderConfig{
		{Name: "mock", Upstream: "http://benchuser:" + password + "@127.0.0.2:19996"},
	}

	plaintext := plaintextUpstreams(providers)
	if len(plaintext) != 1 {
		t.Fatalf("plaintextUpstreams() returned %d entries, want 1: %+v", len(plaintext), plaintext)
	}
	if strings.Contains(plaintext[0].Upstream, password) {
		t.Errorf("upstream leaks the password %q in cleartext: %s", password, plaintext[0].Upstream)
	}
	if want := "http://benchuser:xxxxx@127.0.0.2:19996"; plaintext[0].Upstream != want {
		t.Errorf("redacted upstream = %q, want %q", plaintext[0].Upstream, want)
	}
}
