// Package admin provides the admin API for budget management: agent status,
// budget reset, and the pause and unpause kill switch. It registers on the
// admin listener (loopback by default) and holds no authentication (Phase
// 2), so it applies two zero-dependency browser confused-deputy checks: a
// Host check against DNS rebinding and an Origin check against cross-site
// simple POSTs.
package admin

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"time"

	"github.com/levee-ai/levee/internal/budget"
	"github.com/levee-ai/levee/internal/config"
	"github.com/levee-ai/levee/pkg/types"
)

// AgentInfo is the admin package's view of one configured agent. It is
// deliberately narrow: the config struct carries identifier header values,
// the closest thing Levee has to an agent credential, and an
// unauthenticated HTTP surface must not hold a reference that a future
// diff could leak.
type AgentInfo struct {
	Name string
	Mode string
}

// AgentInfosFromConfig narrows a config agent slice to the admin view.
func AgentInfosFromConfig(agents []config.AgentConfig) []AgentInfo {
	infos := make([]AgentInfo, 0, len(agents))
	for _, configuredAgent := range agents {
		infos = append(infos, AgentInfo{Name: configuredAgent.Name, Mode: configuredAgent.Mode})
	}
	return infos
}

// StatePersister forces a durable snapshot write. Mutating endpoints call
// it synchronously before responding so a crash cannot silently disarm a
// pause.
type StatePersister interface {
	WriteOnce() error
}

// handlers holds the shared dependencies for every admin endpoint.
type handlers struct {
	agents    []AgentInfo // sorted by name at Register time
	store     *budget.Store
	persister StatePersister
	logger    *slog.Logger
	// enforceLoopbackHost is true when the admin listener is bound to a
	// loopback address (the default). The Host check only makes sense
	// there: a deliberately widened bind receives legitimate non-loopback
	// Hosts and gets a startup WARN instead.
	enforceLoopbackHost bool
}

// Register registers the five admin routes on mux. bindHost is the
// effective admin bind address, empty means the 127.0.0.1 default.
func Register(mux *http.ServeMux, agents []AgentInfo, store *budget.Store,
	persister StatePersister, bindHost string, logger *slog.Logger) {
	sorted := make([]AgentInfo, len(agents))
	copy(sorted, agents)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	shared := &handlers{
		agents:              sorted,
		store:               store,
		persister:           persister,
		logger:              logger,
		enforceLoopbackHost: bindHost == "" || isLoopbackHost(bindHost),
	}
	mux.HandleFunc("GET /agents", shared.guarded(shared.listAgents))
	mux.HandleFunc("GET /agents/{name}", shared.guarded(shared.getAgent))
	mux.HandleFunc("POST /agents/{name}/reset", shared.guarded(shared.resetAgent))
	mux.HandleFunc("POST /agents/{name}/pause", shared.guarded(shared.setPaused(true, "pause")))
	mux.HandleFunc("POST /agents/{name}/unpause", shared.guarded(shared.setPaused(false, "unpause")))
}

// guarded wraps a handler with the two browser confused-deputy checks. The
// loopback bind stops network callers but not the operator's own browser,
// which is a same-host process executing remote instructions.
func (shared *handlers) guarded(next http.HandlerFunc) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		// Host check (DNS rebinding): a rebound hostname resolves to
		// 127.0.0.1 but carries the attacker's Host header. Enforced only
		// under a loopback bind, a widened bind legitimately receives
		// other Hosts.
		if shared.enforceLoopbackHost && !isLoopbackHost(request.Host) {
			writeAdminError(writer, http.StatusForbidden, "host_not_allowed",
				"admin API only accepts loopback Host headers, got "+strconv.Quote(request.Host))
			return
		}
		// Origin check (cross-site simple POST): bodyless cross-origin
		// POSTs skip CORS preflight, so a malicious page can fire
		// mutations blind. curl and same-host tools send no Origin.
		if request.Method == http.MethodPost {
			if origin := request.Header.Get("Origin"); origin != "" && !isLoopbackOrigin(origin) {
				writeAdminError(writer, http.StatusForbidden, "origin_not_allowed",
					"admin mutations do not accept cross-origin requests")
				return
			}
		}
		next(writer, request)
	}
}

// isLoopbackHost reports whether a Host header value, optionally with a
// port, names loopback: localhost, 127.0.0.0/8, or ::1.
func isLoopbackHost(hostPort string) bool {
	host := hostPort
	if splitHost, _, err := net.SplitHostPort(hostPort); err == nil {
		host = splitHost
	}
	if host == "localhost" {
		return true
	}
	// A bare bracketed IPv6 literal like "[::1]" carries no port, so
	// SplitHostPort rejects it and the brackets survive, which ParseIP
	// does not accept. Strip one balanced surrounding pair and let
	// ParseIP stay the sole judge of what the inside names. A successful
	// split never leaves surrounding brackets, so this fires only on the
	// split-failure path.
	if len(host) >= 2 && host[0] == '[' && host[len(host)-1] == ']' {
		host = host[1 : len(host)-1]
	}
	parsed := net.ParseIP(host)
	return parsed != nil && parsed.IsLoopback()
}

// isLoopbackOrigin reports whether an Origin header names a loopback host.
func isLoopbackOrigin(origin string) bool {
	parsed, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return isLoopbackHost(parsed.Host)
}

// budgetView is one budget row of an agent view. Amounts are strings via
// budget.FormatAmount, emitted as json.Number like the proxy 429 body.
// remaining is RAW, including negative: this surface is diagnostic and
// shows the truth (the 429 body clamps to 0 for client consumption, a
// deliberate divergence). reset_at renders ONLY for fixed windows: for a
// healthy rolling window StatusAll's ResetAt is the current instant, which
// ticks with every poll and carries no information.
type budgetView struct {
	Type      string      `json:"type"`
	Limit     json.Number `json:"limit"`
	Used      json.Number `json:"used"`
	Reserved  json.Number `json:"reserved"`
	Remaining json.Number `json:"remaining"`
	ResetAt   string      `json:"reset_at,omitempty"`
}

// agentView is the JSON shape of one agent on the admin surface.
type agentView struct {
	Name     string       `json:"name"`
	Mode     string       `json:"mode"`
	Paused   bool         `json:"paused"`
	InFlight int          `json:"in_flight"`
	Budgets  []budgetView `json:"budgets"`
}

// buildAgentView assembles one agent's view from the store. A passthrough
// agent has no budget state: empty budgets array (never null) and zero
// in-flight. Each store call is individually consistent under its own
// lock, but the composite is not atomic. in_flight and budgets can
// disagree transiently while a reservation settles, so one poll must not
// be treated as a consistent snapshot.
func (shared *handlers) buildAgentView(info AgentInfo) agentView {
	view := agentView{
		Name:    info.Name,
		Mode:    info.Mode,
		Paused:  shared.store.IsPaused(info.Name),
		Budgets: []budgetView{},
	}
	if inFlight, err := shared.store.InFlightReservations(info.Name); err == nil {
		view.InFlight = inFlight
	}
	statuses, err := shared.store.StatusAll(info.Name)
	if err != nil {
		return view
	}
	for _, status := range statuses {
		row := budgetView{
			Type:      status.Type,
			Limit:     json.Number(budget.FormatAmount(status.Type, status.Limit)),
			Used:      json.Number(budget.FormatAmount(status.Type, status.Used)),
			Reserved:  json.Number(budget.FormatAmount(status.Type, status.Reserved)),
			Remaining: json.Number(budget.FormatAmount(status.Type, status.Remaining)),
		}
		if status.WindowType == types.WindowFixed {
			row.ResetAt = status.ResetAt.UTC().Format(time.RFC3339)
		}
		view.Budgets = append(view.Budgets, row)
	}
	return view
}

// findAgent returns the AgentInfo for name, or false.
func (shared *handlers) findAgent(name string) (AgentInfo, bool) {
	for _, info := range shared.agents {
		if info.Name == name {
			return info, true
		}
	}
	return AgentInfo{}, false
}

func (shared *handlers) listAgents(writer http.ResponseWriter, request *http.Request) {
	views := make([]agentView, 0, len(shared.agents))
	for _, info := range shared.agents {
		views = append(views, shared.buildAgentView(info))
	}
	writeJSON(writer, http.StatusOK, map[string]any{"agents": views})
}

func (shared *handlers) getAgent(writer http.ResponseWriter, request *http.Request) {
	name := request.PathValue("name")
	info, ok := shared.findAgent(name)
	if !ok {
		writeAgentNotFound(writer, name)
		return
	}
	writeJSON(writer, http.StatusOK, shared.buildAgentView(info))
}

func writeAgentNotFound(writer http.ResponseWriter, name string) {
	writeAdminError(writer, http.StatusNotFound, "agent_not_found",
		"no configured agent named "+strconv.Quote(name))
}

// writeAdminError writes the wire error envelope. The shape matches the
// proxy wire format, restated here because importing the proxy package
// from admin is the wrong dependency direction.
func writeAdminError(writer http.ResponseWriter, status int, errorType, message string) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	payload := struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}{}
	payload.Error.Type = errorType
	payload.Error.Message = message
	_ = json.NewEncoder(writer).Encode(payload)
}

func writeJSON(writer http.ResponseWriter, status int, payload any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(payload)
}

// persistMutation forces the snapshot write that makes an admin mutation
// durable BEFORE the response. On failure the in-memory state STANDS (a
// pause that blocks now beats one that does not) and the caller reports
// persisted false, never a bare 200 over a failed write.
func (shared *handlers) persistMutation(action, agentName string) bool {
	if err := shared.persister.WriteOnce(); err != nil {
		shared.logger.Warn("Admin mutation applied but snapshot write failed, state is in-memory only",
			"action", action, "agent", agentName, "error", err.Error())
		return false
	}
	return true
}

func (shared *handlers) setPaused(paused bool, action string) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		name := request.PathValue("name")
		if err := shared.store.SetPaused(name, paused); err != nil {
			writeAgentNotFound(writer, name)
			return
		}
		persisted := shared.persistMutation(action, name)
		shared.logger.Info("Admin agent action",
			"endpoint", request.URL.Path, "agent", name, "action", action,
			"remote_addr", request.RemoteAddr, "persisted", persisted)
		writeJSON(writer, http.StatusOK, map[string]any{
			"status": "ok", "agent": name, "action": action,
			"paused": paused, "persisted": persisted,
		})
	}
}

func (shared *handlers) resetAgent(writer http.ResponseWriter, request *http.Request) {
	name := request.PathValue("name")
	cleared, err := shared.store.ResetUsage(name)
	if errors.Is(err, budget.ErrUnknownAgent) {
		writeAgentNotFound(writer, name)
		return
	}
	if errors.Is(err, budget.ErrNoBudgets) {
		writeAdminError(writer, http.StatusConflict, "no_budgets",
			"agent "+strconv.Quote(name)+" is passthrough and has no budgets to reset")
		return
	}
	if err != nil {
		writeAdminError(writer, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	// Render cleared amounts in each budget's unit as json.Number, which
	// writes the FormatAmount literal verbatim and unquoted, so cleared
	// renders exactly like the budget amounts on the GET surface. StatusAll
	// is index aligned with the cleared slice by construction.
	statuses, statusErr := shared.store.StatusAll(name)
	clearedText := make([]json.Number, len(cleared))
	for i, amount := range cleared {
		unit := "tokens"
		if statusErr == nil && i < len(statuses) {
			unit = statuses[i].Type
		}
		clearedText[i] = json.Number(budget.FormatAmount(unit, amount))
	}
	persisted := shared.persistMutation("reset", name)
	shared.logger.Info("Admin agent action",
		"endpoint", request.URL.Path, "agent", name, "action", "reset",
		"remote_addr", request.RemoteAddr, "cleared", cleared, "persisted", persisted)
	writeJSON(writer, http.StatusOK, map[string]any{
		"status": "ok", "agent": name, "action": "reset",
		"cleared": clearedText, "persisted": persisted,
	})
}
