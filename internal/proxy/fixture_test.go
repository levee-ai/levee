package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
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
// not a test-authored guess. Only the per-provider model metadata line is
// consulted (it must carry "<provider> model:"), never prose, and the
// streaming marker keeps its leading comma so it can never match inside
// "non-streaming Content-Type: ".
func fixtureContentType(tb testing.TB, provider string, streaming bool) string {
	tb.Helper()
	readme := string(loadFixture(tb, "README.md"))
	marker := "non-streaming Content-Type: "
	if streaming {
		marker = ", streaming: "
	}
	for _, line := range strings.Split(readme, "\n") {
		if !strings.Contains(strings.ToLower(line), provider+" model:") {
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
	tb.Fatalf("fixtures README carries no %q entry for %s, re-run capture.sh", marker, provider)
	return ""
}

func serveFixture(tb testing.TB, body []byte, contentType string) *httptest.Server {
	tb.Helper()
	return httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
		responseWriter.Header().Set("Content-Type", contentType)
		if _, err := responseWriter.Write(body); err != nil {
			tb.Errorf("replay write: %v", err)
		}
	}))
}

// serveSSEFixture replays a captured stream event by event (blank-line
// boundaries) with a flush per event, per docs/architecture/002-streaming-design.md
// framing, so the proxy scanner sees realistic incremental reads.
func serveSSEFixture(tb testing.TB, body []byte, contentType string) (*httptest.Server, int) {
	tb.Helper()
	events := strings.Split(strings.TrimRight(string(body), "\n"), "\n\n")
	if len(events) < 2 {
		tb.Fatalf("fixture split produced %d events, expected several: blank-line framing is broken (CRLF fixture?)", len(events))
	}
	server := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
		responseWriter.Header().Set("Content-Type", contentType)
		flusher, isFlusher := responseWriter.(http.Flusher)
		if !isFlusher {
			tb.Error("replay server writer is not a flusher")
			return
		}
		for _, event := range events {
			if _, err := io.WriteString(responseWriter, event+"\n\n"); err != nil {
				tb.Errorf("replay event write: %v", err)
				return
			}
			flusher.Flush()
		}
	}))
	return server, len(events)
}

// captureMaxTokens mirrors the max_tokens value testdata/fixtures/capture.sh
// sends on every capture request. The oracle guard rejects any fixture whose
// output_tokens exceeds it, and replay requests carry the same cap.
const captureMaxTokens = 16

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
	if usage.output > captureMaxTokens {
		tb.Fatalf("%s output_tokens %d exceeds the captured max_tokens %d, oracle or fixture is wrong", source, usage.output, captureMaxTokens)
	}
	return usage
}

func openAIJSONOracle(tb testing.TB, body []byte, source string) fixtureUsage {
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
	return fixtureUsage{model: parsed.Model, input: parsed.Usage.PromptTokens, output: parsed.Usage.CompletionTokens}.guard(tb, source)
}

func anthropicJSONOracle(tb testing.TB, body []byte, source string) fixtureUsage {
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
	return fixtureUsage{model: parsed.Model, input: parsed.Usage.InputTokens, output: parsed.Usage.OutputTokens}.guard(tb, source)
}

func openAIStreamOracle(tb testing.TB, body []byte, source string) fixtureUsage {
	tb.Helper()
	var usage fixtureUsage
	for _, line := range strings.Split(string(body), "\n") {
		payload, found := strings.CutPrefix(line, "data: ")
		if !found || payload == "[DONE]" {
			continue
		}
		var chunk struct {
			Model string `json:"model"`
			Usage *struct {
				PromptTokens     int64 `json:"prompt_tokens"`
				CompletionTokens int64 `json:"completion_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		if chunk.Model != "" {
			usage.model = chunk.Model
		}
		if chunk.Usage != nil {
			usage.input = chunk.Usage.PromptTokens
			usage.output = chunk.Usage.CompletionTokens
		}
	}
	return usage.guard(tb, source)
}

func anthropicStreamOracle(tb testing.TB, body []byte, source string) fixtureUsage {
	tb.Helper()
	var usage fixtureUsage
	for _, line := range strings.Split(string(body), "\n") {
		payload, found := strings.CutPrefix(line, "data: ")
		if !found {
			continue
		}
		var event struct {
			Type    string `json:"type"`
			Message *struct {
				Model string `json:"model"`
				Usage struct {
					InputTokens int64 `json:"input_tokens"`
				} `json:"usage"`
			} `json:"message"`
			Usage *struct {
				OutputTokens int64 `json:"output_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			continue
		}
		if event.Type == "message_start" && event.Message != nil {
			usage.model = event.Message.Model
			usage.input = event.Message.Usage.InputTokens
		}
		if event.Type == "message_delta" && event.Usage != nil {
			usage.output = event.Usage.OutputTokens
		}
	}
	return usage.guard(tb, source)
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
	body := fmt.Sprintf(`{"model":%q,"max_tokens":%d,"messages":[{"role":"user","content":"Reply with the single word ok."}]}`, model, captureMaxTokens)
	if streaming {
		body = fmt.Sprintf(`{"model":%q,"max_tokens":%d,"stream":true,"messages":[{"role":"user","content":"Reply with the single word ok."}]}`, model, captureMaxTokens)
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
	seenTypes := make(map[string]bool, 2)
	for _, status := range statuses {
		switch status.Type {
		case "tokens":
			seenTypes["tokens"] = true
			if status.Used != expectedTokens {
				tb.Errorf("token budget Used = %d, want %d (input %d output %d)", status.Used, expectedTokens, usage.input, usage.output)
			}
		case "dollars":
			seenTypes["dollars"] = true
			if status.Used != expectedMicrodollars {
				tb.Errorf("dollar budget Used = %d microdollars, want %d", status.Used, expectedMicrodollars)
			}
		}
		if status.Reserved != 0 {
			tb.Errorf("%s budget still holds a reservation of %d after settlement", status.Type, status.Reserved)
		}
	}
	for _, budgetType := range []string{"tokens", "dollars"} {
		if !seenTypes[budgetType] {
			tb.Fatalf("StatusAll returned no %s budget, its settlement was never asserted, check the fixtureAgent budget wiring", budgetType)
		}
	}
}

func TestFixtureOpenAINonStreamingLifecycle(t *testing.T) {
	fixtureName := filepath.Join("openai", "chat-completion.json")
	fixture := loadFixture(t, fixtureName)
	usage := openAIJSONOracle(t, fixture, fixtureName)
	upstream := serveFixture(t, fixture, fixtureContentType(t, "openai", false))
	defer upstream.Close()

	proxyUnderTest := fixtureProxy(t, upstream.URL, "", []config.AgentConfig{fixtureAgent("fixture-agent")})
	recorder := httptest.NewRecorder()
	proxyUnderTest.ServeHTTP(recorder, fixtureChatRequest(t, "/openai/v1/chat/completions", usage.model, false))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", recorder.Code, recorder.Body.String())
	}
	if !bytes.Equal(recorder.Body.Bytes(), fixture) {
		t.Errorf("response body does not match the fixture bytes: got %d bytes %q, want %d bytes %q", recorder.Body.Len(), recorder.Body.Bytes(), len(fixture), fixture)
	}
	assertSettlement(t, proxyUnderTest, "fixture-agent", usage)
}

func TestFixtureAnthropicNonStreamingLifecycle(t *testing.T) {
	fixtureName := filepath.Join("anthropic", "messages.json")
	fixture := loadFixture(t, fixtureName)
	usage := anthropicJSONOracle(t, fixture, fixtureName)
	upstream := serveFixture(t, fixture, fixtureContentType(t, "anthropic", false))
	defer upstream.Close()

	proxyUnderTest := fixtureProxy(t, "", upstream.URL, []config.AgentConfig{fixtureAgent("fixture-agent")})
	recorder := httptest.NewRecorder()
	proxyUnderTest.ServeHTTP(recorder, fixtureChatRequest(t, "/anthropic/v1/messages", usage.model, false))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", recorder.Code, recorder.Body.String())
	}
	if !bytes.Equal(recorder.Body.Bytes(), fixture) {
		t.Errorf("response body does not match the fixture bytes: got %d bytes %q, want %d bytes %q", recorder.Body.Len(), recorder.Body.Bytes(), len(fixture), fixture)
	}
	assertSettlement(t, proxyUnderTest, "fixture-agent", usage)
}

func TestFixtureOpenAIStreamingLifecycle(t *testing.T) {
	fixtureName := filepath.Join("openai", "chat-completion-stream.sse")
	fixture := loadFixture(t, fixtureName)
	usage := openAIStreamOracle(t, fixture, fixtureName)
	nonStreamingName := filepath.Join("openai", "chat-completion.json")
	nonStreamingUsage := openAIJSONOracle(t, loadFixture(t, nonStreamingName), nonStreamingName)
	if usage.input != nonStreamingUsage.input {
		t.Errorf("streaming input_tokens %d differs from same-prompt non-streaming %d, provider tokenization drifted or capture prompts diverged", usage.input, nonStreamingUsage.input)
	}

	upstream, eventCount := serveSSEFixture(t, fixture, fixtureContentType(t, "openai", true))
	defer upstream.Close()
	if eventCount < 2 {
		t.Fatalf("event count %d", eventCount)
	}

	proxyUnderTest := fixtureProxy(t, upstream.URL, "", []config.AgentConfig{fixtureAgent("fixture-agent")})
	recorder := httptest.NewRecorder()
	proxyUnderTest.ServeHTTP(recorder, fixtureChatRequest(t, "/openai/v1/chat/completions", usage.model, true))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	forwarded := recorder.Body.String()
	if !strings.Contains(forwarded, "data: [DONE]") {
		t.Error("forwarded stream lacks the DONE terminal marker")
	}
	for eventIndex, event := range strings.Split(strings.TrimRight(string(fixture), "\n"), "\n\n") {
		firstLine := strings.SplitN(event, "\n", 2)[0]
		if !strings.Contains(forwarded, firstLine) {
			t.Errorf("event %d first line was not forwarded: %q", eventIndex, firstLine)
		}
	}
	assertSettlement(t, proxyUnderTest, "fixture-agent", usage)
}

func TestFixtureAnthropicStreamingLifecycle(t *testing.T) {
	fixtureName := filepath.Join("anthropic", "messages-stream.sse")
	fixture := loadFixture(t, fixtureName)
	usage := anthropicStreamOracle(t, fixture, fixtureName)
	nonStreamingName := filepath.Join("anthropic", "messages.json")
	nonStreamingUsage := anthropicJSONOracle(t, loadFixture(t, nonStreamingName), nonStreamingName)
	if usage.input != nonStreamingUsage.input {
		t.Errorf("streaming input_tokens %d differs from same-prompt non-streaming %d", usage.input, nonStreamingUsage.input)
	}

	upstream, eventCount := serveSSEFixture(t, fixture, fixtureContentType(t, "anthropic", true))
	defer upstream.Close()
	if eventCount < 3 {
		t.Fatalf("anthropic stream replay produced only %d events", eventCount)
	}

	proxyUnderTest := fixtureProxy(t, "", upstream.URL, []config.AgentConfig{fixtureAgent("fixture-agent")})
	recorder := httptest.NewRecorder()
	proxyUnderTest.ServeHTTP(recorder, fixtureChatRequest(t, "/anthropic/v1/messages", usage.model, true))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	forwarded := recorder.Body.String()
	if !strings.Contains(forwarded, "event: message_stop") {
		t.Error("forwarded stream lacks message_stop")
	}
	assertSettlement(t, proxyUnderTest, "fixture-agent", usage)
}

// TestFixtureBudgetExhaustion derives the limit from the fixture usage so a
// re-capture cannot break it, and bounds the loop so an enforcement regression
// fails instead of hanging. Only the budget_exhausted type field is asserted
// here, the full 429 body shape is already pinned by the rejection tests.
func TestFixtureBudgetExhaustion(t *testing.T) {
	fixtureName := filepath.Join("openai", "chat-completion.json")
	fixture := loadFixture(t, fixtureName)
	usage := openAIJSONOracle(t, fixture, fixtureName)
	upstream := serveFixture(t, fixture, fixtureContentType(t, "openai", false))
	defer upstream.Close()

	perRequestTokens := usage.input + usage.output
	exhaustionAgent := fixtureAgent("fixture-agent")
	exhaustionAgent.Budgets = []config.BudgetConfig{
		{Type: "tokens", Limit: float64(perRequestTokens * 2), Window: "1h", WindowType: "rolling"},
	}
	proxyUnderTest := fixtureProxy(t, upstream.URL, "", []config.AgentConfig{exhaustionAgent})

	const boundedAttempts = 6
	sawSuccess := false
	for attempt := 1; attempt <= boundedAttempts; attempt++ {
		recorder := httptest.NewRecorder()
		proxyUnderTest.ServeHTTP(recorder, fixtureChatRequest(t, "/openai/v1/chat/completions", usage.model, false))
		if recorder.Code == http.StatusOK {
			sawSuccess = true
			continue
		}
		if recorder.Code == http.StatusTooManyRequests {
			if !sawSuccess {
				t.Fatal("429 arrived before any request succeeded, the limit or estimate math is off")
			}
			if !strings.Contains(recorder.Body.String(), `"type":"budget_exhausted"`) {
				t.Errorf("429 body lacks budget_exhausted type: %s", recorder.Body.String())
			}
			return
		}
		t.Fatalf("attempt %d: unexpected status %d", attempt, recorder.Code)
	}
	t.Fatalf("no 429 within %d fixture-backed requests at a two-request budget", boundedAttempts)
}
