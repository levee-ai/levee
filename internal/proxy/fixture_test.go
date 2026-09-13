package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/levee-ai/levee/internal/agent"
	"github.com/levee-ai/levee/internal/budget"
	"github.com/levee-ai/levee/internal/config"
	"github.com/levee-ai/levee/internal/tokens"
)

// Fixtures live at the REPO ROOT testdata/fixtures/ (see
// testdata/fixtures/README.md). Go tests run with the package directory as
// the working directory, hence the explicit two-level ascent.
func loadFixture(tb testing.TB, name string) []byte {
	tb.Helper()
	path := filepath.Join("..", "..", "testdata", "fixtures", name)
	content, err := os.ReadFile(path)
	if err != nil {
		tb.Fatalf("fixture %s is missing (%v): fixtures are committed, never skipped, re-run testdata/fixtures/capture.sh if the set is incomplete", name, err)
	}
	return content
}

// fixtureContentType returns the Content-Type recorded at capture time in the
// fixtures README metadata block, so the replay serves the OBSERVED header,
// not a test-authored guess.
func fixtureContentType(tb testing.TB, provider string, streaming bool) string {
	tb.Helper()
	readme := string(loadFixture(tb, "README.md"))
	marker := "non-streaming Content-Type: "
	if streaming {
		marker = "streaming: "
	}
	for _, line := range strings.Split(readme, "\n") {
		if !strings.Contains(strings.ToLower(line), provider) {
			continue
		}
		index := strings.Index(line, marker)
		if index < 0 {
			continue
		}
		rest := line[index+len(marker):]
		if end := strings.IndexAny(rest, ",)"); end >= 0 {
			rest = rest[:end]
		}
		return strings.TrimSpace(rest)
	}
	tb.Fatalf("fixtures README carries no %s Content-Type for %s, re-run capture.sh", marker, provider)
	return ""
}

func serveJSONFixture(tb testing.TB, body []byte, contentType string) *httptest.Server {
	tb.Helper()
	return httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
		responseWriter.Header().Set("Content-Type", contentType)
		if _, err := responseWriter.Write(body); err != nil {
			tb.Errorf("replay write: %v", err)
		}
	}))
}

// fixtureUsage is the oracle's independent read of a fixture. The parser is
// encoding/json plus a plain line scan, deliberately not the production gjson
// paths. Production extraction helpers (inspectUsage, shouldParseUsage) are
// in-package and MUST NOT be called here, that would collapse the oracle into
// the code under test.
type fixtureUsage struct {
	model  string
	input  int64
	output int64
}

func (usage fixtureUsage) guard(tb testing.TB, source string) fixtureUsage {
	tb.Helper()
	if usage.model == "" || usage.input <= 0 || usage.output <= 0 {
		tb.Fatalf("%s carries no authoritative usage at the expected paths (model=%q input=%d output=%d): provider shape changed, update extraction and this oracle together, then re-run capture.sh", source, usage.model, usage.input, usage.output)
	}
	if usage.output > 16 {
		tb.Fatalf("%s output_tokens %d exceeds the captured max_tokens 16, oracle or fixture is wrong", source, usage.output)
	}
	return usage
}

func openAIJSONOracle(tb testing.TB, body []byte) fixtureUsage {
	tb.Helper()
	var parsed struct {
		Model string `json:"model"`
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		tb.Fatalf("openai json oracle: %v", err)
	}
	return fixtureUsage{model: parsed.Model, input: parsed.Usage.PromptTokens, output: parsed.Usage.CompletionTokens}.guard(tb, "openai/chat-completion.json")
}

func anthropicJSONOracle(tb testing.TB, body []byte) fixtureUsage {
	tb.Helper()
	var parsed struct {
		Model string `json:"model"`
		Usage struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		tb.Fatalf("anthropic json oracle: %v", err)
	}
	return fixtureUsage{model: parsed.Model, input: parsed.Usage.InputTokens, output: parsed.Usage.OutputTokens}.guard(tb, "anthropic/messages.json")
}

// fixtureAgent is a token PLUS dollar budget agent so both settlement paths
// are asserted. Limits are far above one request so admission never interferes.
func fixtureAgent(name string) config.AgentConfig {
	return config.AgentConfig{
		Name: name,
		Identifier: config.IdentifierConfig{
			Type:        "header",
			HeaderName:  "X-Levee-Agent",
			HeaderValue: name,
		},
		Mode: "enforce",
		Budgets: []config.BudgetConfig{
			{Type: "tokens", Limit: 1_000_000, Window: "1h", WindowType: "rolling"},
			{Type: "dollars", Limit: 100_000_000, Window: "1h", WindowType: "rolling"},
		},
	}
}

func fixtureProxy(tb testing.TB, openaiURL string, anthropicURL string, agents []config.AgentConfig) *Proxy {
	tb.Helper()
	store, err := budget.NewStore(agents, budget.DefaultStreamLimit, nil)
	if err != nil {
		tb.Fatalf("NewStore: %v", err)
	}
	runtimes := make(map[string]agentRuntime, len(agents))
	for _, configuredAgent := range agents {
		budgetTypes := make([]string, len(configuredAgent.Budgets))
		for i, configuredBudget := range configuredAgent.Budgets {
			budgetTypes[i] = configuredBudget.Type
		}
		runtimes[configuredAgent.Name] = agentRuntime{mode: configuredAgent.Mode, budgetTypes: budgetTypes}
	}
	providers := map[string]*providerTarget{}
	if openaiURL != "" {
		providers["openai"] = newProviderTarget(openaiURL, testTimeouts())
	}
	if anthropicURL != "" {
		providers["anthropic"] = newProviderTarget(anthropicURL, testTimeouts())
	}
	return &Proxy{
		providers:    providers,
		logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		resolver:     agent.NewResolver(agents),
		store:        store,
		estimator:    tokens.NewEstimator("cl100k_base"),
		agents:       runtimes,
		unknownAgent: "block",
	}
}

func fixtureChatRequest(tb testing.TB, path string, model string, streaming bool) *http.Request {
	tb.Helper()
	body := `{"model":"` + model + `","max_tokens":16,"messages":[{"role":"user","content":"Reply with the single word ok."}]}`
	if streaming {
		body = `{"model":"` + model + `","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"Reply with the single word ok."}]}`
	}
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Levee-Agent", "fixture-agent")
	return request
}

// assertSettlement reads both budgets after the response and asserts exact
// accounting: tokens Used is input plus output, dollars Used is the production
// pricing of that split. The fixture test proves PLUMBING (model threading,
// extraction, settlement commit). Rate arithmetic stays pricing_test territory,
// so CostMicrodollars is the deliberate expected-value source here.
func assertSettlement(tb testing.TB, proxyUnderTest *Proxy, agentName string, usage fixtureUsage) {
	tb.Helper()
	statuses, err := proxyUnderTest.store.StatusAll(agentName)
	if err != nil {
		tb.Fatalf("StatusAll: %v", err)
	}
	expectedTokens := usage.input + usage.output
	expectedMicrodollars, known := budget.CostMicrodollars(usage.model, usage.input, usage.output)
	if !known {
		tb.Fatalf("model %q is not in the pricing table, capture with a priced model", usage.model)
	}
	for _, status := range statuses {
		switch status.Type {
		case "tokens":
			if status.Used != expectedTokens {
				tb.Errorf("token budget Used = %d, want %d (input %d output %d)", status.Used, expectedTokens, usage.input, usage.output)
			}
		case "dollars":
			if status.Used != expectedMicrodollars {
				tb.Errorf("dollar budget Used = %d microdollars, want %d", status.Used, expectedMicrodollars)
			}
		}
		if status.Reserved != 0 {
			tb.Errorf("%s budget still holds a reservation of %d after settlement", status.Type, status.Reserved)
		}
	}
}

func TestFixtureOpenAINonStreamingLifecycle(t *testing.T) {
	fixture := loadFixture(t, filepath.Join("openai", "chat-completion.json"))
	usage := openAIJSONOracle(t, fixture)
	upstream := serveJSONFixture(t, fixture, fixtureContentType(t, "openai", false))
	defer upstream.Close()

	proxyUnderTest := fixtureProxy(t, upstream.URL, "", []config.AgentConfig{fixtureAgent("fixture-agent")})
	recorder := httptest.NewRecorder()
	proxyUnderTest.ServeHTTP(recorder, fixtureChatRequest(t, "/openai/v1/chat/completions", usage.model, false))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", recorder.Code, recorder.Body.String())
	}
	if !bytes.Equal(recorder.Body.Bytes(), fixture) {
		t.Error("response body does not match the fixture bytes")
	}
	assertSettlement(t, proxyUnderTest, "fixture-agent", usage)
}

func TestFixtureAnthropicNonStreamingLifecycle(t *testing.T) {
	fixture := loadFixture(t, filepath.Join("anthropic", "messages.json"))
	usage := anthropicJSONOracle(t, fixture)
	upstream := serveJSONFixture(t, fixture, fixtureContentType(t, "anthropic", false))
	defer upstream.Close()

	proxyUnderTest := fixtureProxy(t, "", upstream.URL, []config.AgentConfig{fixtureAgent("fixture-agent")})
	recorder := httptest.NewRecorder()
	proxyUnderTest.ServeHTTP(recorder, fixtureChatRequest(t, "/anthropic/v1/messages", usage.model, false))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", recorder.Code, recorder.Body.String())
	}
	if !bytes.Equal(recorder.Body.Bytes(), fixture) {
		t.Error("response body does not match the fixture bytes")
	}
	assertSettlement(t, proxyUnderTest, "fixture-agent", usage)
}
