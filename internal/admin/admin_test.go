package admin

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/levee-ai/levee/internal/budget"
	"github.com/levee-ai/levee/internal/config"
)

// okPersister satisfies StatePersister and counts calls.
type okPersister struct{ calls int }

func (persister *okPersister) WriteOnce() error {
	persister.calls++
	return nil
}

// failingPersister always fails, for the persisted false path.
type failingPersister struct{}

func (persister *failingPersister) WriteOnce() error {
	return errors.New("disk full")
}

// testAgents lists scraper before researcher, deliberately out of
// alphabetical order, so the list endpoint's sorted-output assertion is
// load-bearing.
func testAgents() []config.AgentConfig {
	return []config.AgentConfig{
		{
			Name: "scraper",
			Mode: "passthrough",
			Identifier: config.IdentifierConfig{
				Type: "header", HeaderName: "X-Levee-Agent", HeaderValue: "secret-scraper-value",
			},
		},
		{
			Name: "researcher",
			Mode: "enforce",
			Identifier: config.IdentifierConfig{
				Type: "header", HeaderName: "X-Levee-Agent", HeaderValue: "secret-researcher-value",
			},
			Budgets: []config.BudgetConfig{
				{Type: "tokens", Limit: 100000, Window: "1h", WindowType: "rolling"},
				{Type: "dollars", Limit: 50, Window: "24h", WindowType: "fixed", ResetAt: "00:00Z"},
			},
		},
	}
}

// newTestMux builds a store from testAgents and registers admin routes on a
// fresh mux bound to bindHost, empty meaning the loopback default.
func newTestMux(t *testing.T, bindHost string) (*http.ServeMux, *budget.Store, *okPersister) {
	t.Helper()
	agents := testAgents()
	store, err := budget.NewStore(agents, budget.DefaultStreamLimit, nil)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	mux := http.NewServeMux()
	persister := &okPersister{}
	Register(mux, AgentInfosFromConfig(agents), store, persister, bindHost,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	return mux, store, persister
}

func adminGet(mux *http.ServeMux, path string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, path, nil)
	request.Host = "127.0.0.1:9091"
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)
	return recorder
}

func TestGetAgentsListsAllSortedWithModeAndBudgets(t *testing.T) {
	mux, store, _ := newTestMux(t, "127.0.0.1")
	if err := store.Track("researcher", 4321); err != nil {
		t.Fatalf("Track: %v", err)
	}
	recorder := adminGet(mux, "/agents")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", recorder.Code, recorder.Body.String())
	}
	if contentType := recorder.Header().Get("Content-Type"); !strings.Contains(contentType, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", contentType)
	}
	var response struct {
		Agents []struct {
			Name     string `json:"name"`
			Mode     string `json:"mode"`
			Paused   bool   `json:"paused"`
			InFlight int    `json:"in_flight"`
			Budgets  []struct {
				Type      string      `json:"type"`
				Limit     json.Number `json:"limit"`
				Used      json.Number `json:"used"`
				Reserved  json.Number `json:"reserved"`
				Remaining json.Number `json:"remaining"`
				ResetAt   string      `json:"reset_at"`
			} `json:"budgets"`
		} `json:"agents"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("unmarshal: %v, body: %s", err, recorder.Body.String())
	}
	if len(response.Agents) != 2 {
		t.Fatalf("agents = %d, want 2", len(response.Agents))
	}
	if response.Agents[0].Name != "researcher" || response.Agents[1].Name != "scraper" {
		t.Fatalf("order = %s, %s, want researcher, scraper", response.Agents[0].Name, response.Agents[1].Name)
	}
	researcher := response.Agents[0]
	if researcher.Mode != "enforce" || researcher.Paused {
		t.Fatalf("researcher mode=%s paused=%v", researcher.Mode, researcher.Paused)
	}
	if len(researcher.Budgets) != 2 {
		t.Fatalf("researcher budgets = %d, want 2", len(researcher.Budgets))
	}
	if researcher.Budgets[0].Used.String() != "4321" {
		t.Errorf("tokens used = %s, want 4321", researcher.Budgets[0].Used)
	}
	if researcher.Budgets[1].Limit.String() != "50.00" {
		t.Errorf("dollar limit = %s, want 50.00", researcher.Budgets[1].Limit)
	}
	if researcher.Budgets[0].ResetAt != "" {
		t.Errorf("rolling budget has reset_at %q, want omitted", researcher.Budgets[0].ResetAt)
	}
	if researcher.Budgets[1].ResetAt == "" {
		t.Error("fixed budget missing reset_at")
	}
	scraper := response.Agents[1]
	if scraper.Mode != "passthrough" || scraper.Budgets == nil || len(scraper.Budgets) != 0 {
		t.Fatalf("scraper mode=%s budgets=%v, want passthrough with empty array", scraper.Mode, scraper.Budgets)
	}
	if !strings.Contains(recorder.Body.String(), `"budgets":[]`) {
		t.Errorf("passthrough budgets not an empty array: %s", recorder.Body.String())
	}
}

func TestGetAgentsNeverLeaksIdentifierValues(t *testing.T) {
	mux, _, _ := newTestMux(t, "127.0.0.1")
	for _, path := range []string{"/agents", "/agents/researcher"} {
		body := adminGet(mux, path).Body.String()
		if strings.Contains(body, "secret-researcher-value") || strings.Contains(body, "secret-scraper-value") ||
			strings.Contains(body, "X-Levee-Agent") {
			t.Fatalf("%s leaked identifier material: %s", path, body)
		}
	}
}

func TestGetAgentDetailAndNotFound(t *testing.T) {
	mux, store, _ := newTestMux(t, "127.0.0.1")
	if err := store.SetPaused("researcher", true); err != nil {
		t.Fatalf("SetPaused: %v", err)
	}
	recorder := adminGet(mux, "/agents/researcher")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), `"paused":true`) {
		t.Errorf("detail missing paused true: %s", recorder.Body.String())
	}

	recorder = adminGet(mux, "/agents/nobody")
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), `"type":"agent_not_found"`) {
		t.Errorf("404 body missing agent_not_found: %s", recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "nobody") {
		t.Errorf("404 message missing the requested name: %s", recorder.Body.String())
	}
}

func TestNegativeRemainingRendersRaw(t *testing.T) {
	mux, store, _ := newTestMux(t, "127.0.0.1")
	if err := store.TrackMulti("researcher", []int64{150000, 0}); err != nil {
		t.Fatalf("TrackMulti: %v", err)
	}
	body := adminGet(mux, "/agents/researcher").Body.String()
	if !strings.Contains(body, `"remaining":-50000`) {
		t.Errorf("negative remaining not raw: %s", body)
	}
}

func TestHostCheckRejectsNonLoopback(t *testing.T) {
	mux, _, _ := newTestMux(t, "127.0.0.1")
	request := httptest.NewRequest(http.MethodGet, "/agents", nil)
	request.Host = "evil.example.com"
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), `"type":"host_not_allowed"`) {
		t.Errorf("body: %s", recorder.Body.String())
	}
}

func TestHostCheckAllowsLoopbackForms(t *testing.T) {
	mux, _, _ := newTestMux(t, "127.0.0.1")
	for _, host := range []string{"127.0.0.1:9091", "localhost:9091", "[::1]:9091", "127.0.0.1"} {
		request := httptest.NewRequest(http.MethodGet, "/agents", nil)
		request.Host = host
		recorder := httptest.NewRecorder()
		mux.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Errorf("host %q: status = %d, want 200", host, recorder.Code)
		}
	}
}

func TestIsLoopbackHostAcceptanceMatrix(t *testing.T) {
	accepted := []string{
		"127.0.0.1", "127.0.0.1:9091", "127.5.4.3", "localhost",
		"localhost:9091", "::1", "[::1]", "[::1]:9091",
	}
	for _, host := range accepted {
		if !isLoopbackHost(host) {
			t.Errorf("isLoopbackHost(%q) = false, want true", host)
		}
	}
	// Localhost stays rejected: the comparison is case-sensitive, which
	// fails closed. 127.1 and 0177.0.0.1 are shorthand and octal-looking
	// forms net.ParseIP rejects.
	rejected := []string{
		"127.evil.com", "localhost.evil.com", "", "10.0.0.5:9091",
		"0177.0.0.1", "Localhost", "127.1",
	}
	for _, host := range rejected {
		if isLoopbackHost(host) {
			t.Errorf("isLoopbackHost(%q) = true, want false", host)
		}
	}
}

func TestWidenedBindDisablesHostCheck(t *testing.T) {
	mux, _, _ := newTestMux(t, "0.0.0.0")
	request := httptest.NewRequest(http.MethodGet, "/agents", nil)
	request.Host = "192.168.1.5:9091"
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 under a widened bind", recorder.Code)
	}
}

func TestEmptyBindDefaultsToLoopbackHostCheck(t *testing.T) {
	mux, _, _ := newTestMux(t, "")
	request := httptest.NewRequest(http.MethodGet, "/agents", nil)
	request.Host = "evil.example.com"
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 under the empty loopback default", recorder.Code)
	}
}

func TestWrongMethodGets405WithAllow(t *testing.T) {
	mux, _, _ := newTestMux(t, "127.0.0.1")
	request := httptest.NewRequest(http.MethodDelete, "/agents/researcher", nil)
	request.Host = "127.0.0.1:9091"
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", recorder.Code)
	}
	if recorder.Header().Get("Allow") == "" {
		t.Error("405 missing Allow header")
	}
}

func adminPost(mux *http.ServeMux, path string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, path, nil)
	request.Host = "127.0.0.1:9091"
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)
	return recorder
}

func TestPauseUnpauseLifecycle(t *testing.T) {
	mux, store, persister := newTestMux(t, "127.0.0.1")

	recorder := adminPost(mux, "/agents/researcher/pause")
	if recorder.Code != http.StatusOK {
		t.Fatalf("pause status = %d, body %s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	for _, want := range []string{`"status":"ok"`, `"agent":"researcher"`, `"action":"pause"`, `"paused":true`, `"persisted":true`} {
		if !strings.Contains(body, want) {
			t.Errorf("pause body missing %s: %s", want, body)
		}
	}
	if !store.IsPaused("researcher") {
		t.Fatal("store not paused after POST pause")
	}
	if persister.calls != 1 {
		t.Fatalf("persister calls = %d, want 1 (durable write before the response)", persister.calls)
	}

	recorder = adminPost(mux, "/agents/researcher/pause")
	if recorder.Code != http.StatusOK {
		t.Fatalf("second pause status = %d (idempotent re-run must be safe)", recorder.Code)
	}

	recorder = adminPost(mux, "/agents/researcher/unpause")
	if recorder.Code != http.StatusOK {
		t.Fatalf("unpause status = %d", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), `"paused":false`) {
		t.Errorf("unpause body: %s", recorder.Body.String())
	}
	if store.IsPaused("researcher") {
		t.Fatal("store still paused after POST unpause")
	}

	recorder = adminPost(mux, "/agents/scraper/pause")
	if recorder.Code != http.StatusOK {
		t.Fatalf("passthrough pause status = %d (kill switch covers passthrough)", recorder.Code)
	}
	if !store.IsPaused("scraper") {
		t.Fatal("passthrough agent not paused")
	}

	recorder = adminPost(mux, "/agents/typo/pause")
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("unknown pause status = %d, want 404", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), `"type":"agent_not_found"`) {
		t.Errorf("404 body: %s", recorder.Body.String())
	}
}

func TestPauseWriteFailureKeepsPauseAndReportsNotPersisted(t *testing.T) {
	agents := testAgents()
	store, err := budget.NewStore(agents, budget.DefaultStreamLimit, nil)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	mux := http.NewServeMux()
	Register(mux, AgentInfosFromConfig(agents), store, &failingPersister{}, "127.0.0.1",
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	recorder := adminPost(mux, "/agents/researcher/pause")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), `"persisted":false`) {
		t.Errorf("body missing persisted false: %s", recorder.Body.String())
	}
	if !store.IsPaused("researcher") {
		t.Fatal("pause rolled back on write failure, it must stand")
	}
}

func TestResetEndpoint(t *testing.T) {
	mux, store, persister := newTestMux(t, "127.0.0.1")
	if err := store.TrackMulti("researcher", []int64{4321, 1_250_000}); err != nil {
		t.Fatalf("TrackMulti: %v", err)
	}

	recorder := adminPost(mux, "/agents/researcher/reset")
	if recorder.Code != http.StatusOK {
		t.Fatalf("reset status = %d, body %s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	for _, want := range []string{`"action":"reset"`, `"cleared":[4321,1.25]`, `"persisted":true`} {
		if !strings.Contains(body, want) {
			t.Errorf("reset body missing %s: %s", want, body)
		}
	}
	if persister.calls != 1 {
		t.Fatalf("persister calls = %d, want 1", persister.calls)
	}
	statuses, err := store.StatusAll("researcher")
	if err != nil {
		t.Fatalf("StatusAll: %v", err)
	}
	if statuses[0].Used != 0 || statuses[1].Used != 0 {
		t.Fatalf("usage not zeroed: %d, %d", statuses[0].Used, statuses[1].Used)
	}

	recorder = adminPost(mux, "/agents/researcher/reset")
	if recorder.Code != http.StatusOK {
		t.Fatalf("second reset status = %d (idempotent re-run must be safe)", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), `"cleared":[0,0.00]`) {
		t.Errorf("second reset body: %s", recorder.Body.String())
	}

	recorder = adminPost(mux, "/agents/scraper/reset")
	if recorder.Code != http.StatusConflict {
		t.Fatalf("passthrough reset status = %d, want 409", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), `"type":"no_budgets"`) {
		t.Errorf("409 body: %s", recorder.Body.String())
	}

	recorder = adminPost(mux, "/agents/typo/reset")
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("unknown reset status = %d, want 404", recorder.Code)
	}
}

func TestPostRejectsCrossOrigin(t *testing.T) {
	mux, store, _ := newTestMux(t, "127.0.0.1")
	request := httptest.NewRequest(http.MethodPost, "/agents/researcher/pause", nil)
	request.Host = "127.0.0.1:9091"
	request.Header.Set("Origin", "https://evil.example.com")
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), `"type":"origin_not_allowed"`) {
		t.Errorf("403 body: %s", recorder.Body.String())
	}
	if store.IsPaused("researcher") {
		t.Fatal("cross-origin POST mutated state")
	}
	request = httptest.NewRequest(http.MethodPost, "/agents/researcher/pause", nil)
	request.Host = "127.0.0.1:9091"
	request.Header.Set("Origin", "http://127.0.0.1:3000")
	recorder = httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("loopback-origin status = %d, want 200", recorder.Code)
	}
}
