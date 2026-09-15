// Package main is the entrypoint for the Levee budget-enforcement proxy.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/levee-ai/levee/internal/admin"
	"github.com/levee-ai/levee/internal/budget"
	"github.com/levee-ai/levee/internal/config"
	"github.com/levee-ai/levee/internal/metrics"
	"github.com/levee-ai/levee/internal/proxy"
	"github.com/levee-ai/levee/internal/state"
)

// version is set at build time via ldflags.
var version = "dev"

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "serve":
		runServe(os.Args[2:])
	case "validate":
		runValidate(os.Args[2:])
	case "version":
		fmt.Printf("levee %s\n", version)
	default:
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Fprintf(os.Stderr, "Usage: levee <command> [options]\n\n")
	fmt.Fprintf(os.Stderr, "Commands:\n")
	fmt.Fprintf(os.Stderr, "  serve      Start the proxy server\n")
	fmt.Fprintf(os.Stderr, "  validate   Validate config and exit\n")
	fmt.Fprintf(os.Stderr, "  version    Print version\n")
	fmt.Fprintf(os.Stderr, "\nOptions:\n")
	fmt.Fprintf(os.Stderr, "  --config <path>   Path to configuration file\n")
}

func parseConfigFlag(args []string) string {
	for i, arg := range args {
		if arg == "--config" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func runValidate(args []string) {
	configPath := parseConfigFlag(args)
	if configPath == "" {
		fmt.Fprintf(os.Stderr, "error: --config flag is required\n")
		os.Exit(1)
	}

	_, err := config.Load(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s\n", err.Error())
		os.Exit(1)
	}

	fmt.Println("config valid")
}

// plaintextUpstream names one provider whose upstream uses a plaintext scheme,
// carrying only what the startup warning logs.
//
// Upstream holds the REDACTED URL, never the raw config string. A URL can carry
// a credential in its userinfo, and url.Hostname() strips userinfo before the
// loopback check, so an upstream such as
// http://benchuser:password@127.0.0.1:9999 passes validation and would put that
// password in the startup warning in cleartext. url.URL.Redacted replaces the
// password with "xxxxx". It also normalizes the scheme, so an upstream written
// HTTP:// prints as http://, which is cosmetic and not worth trading the
// redaction for.
type plaintextUpstream struct {
	Name     string
	Upstream string
}

// plaintextUpstreams returns one entry per provider whose upstream scheme is
// plaintext http, in config order. Config validation already ran, and it
// accepts http only when the host is a literal loopback address, so a match
// here means a plaintext loopback hop and nothing else.
//
// The scheme is read from the parsed URL rather than matched as an "http://"
// string prefix because url.Parse lowercases the scheme. An upstream written
// HTTP://127.0.0.1:9999 passes validation, and a prefix check would leave that
// operator unwarned.
func plaintextUpstreams(providers []config.ProviderConfig) []plaintextUpstream {
	var plaintext []plaintextUpstream
	for _, provider := range providers {
		upstreamURL, parseErr := url.Parse(provider.Upstream)
		if parseErr != nil || upstreamURL.Scheme != "http" {
			continue
		}
		plaintext = append(plaintext, plaintextUpstream{
			Name:     provider.Name,
			Upstream: upstreamURL.Redacted(),
		})
	}
	return plaintext
}

func runServe(args []string) {
	configPath := parseConfigFlag(args)
	if configPath == "" {
		fmt.Fprintf(os.Stderr, "error: --config flag is required\n")
		os.Exit(1)
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	cfg, err := config.Load(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s\n", err.Error())
		os.Exit(1)
	}

	logger.Info("levee starting",
		"version", version,
		"config", configPath,
		"providers", len(cfg.Providers),
		"agents", len(cfg.Agents),
	)

	store, err := budget.NewStore(cfg.Agents, budget.DefaultStreamLimit, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: budget store: %s\n", err.Error())
		os.Exit(1)
	}

	loadResult, err := state.Load(cfg.State.SnapshotPath, time.Now)
	if err != nil {
		// Fail-safe: an unreadable or unknown-version state file refuses
		// startup rather than zeroing the ledger.
		fmt.Fprintf(os.Stderr, "error: state snapshot: %s\n", err.Error())
		os.Exit(1)
	}
	switch {
	case loadResult.CorruptAside != "":
		logger.Error("State snapshot was corrupt, starting fresh, any persisted pause state was lost with it",
			"path", cfg.State.SnapshotPath, "moved_to", loadResult.CorruptAside)
	case loadResult.Fresh:
		logger.Info("No prior state snapshot, starting fresh", "path", cfg.State.SnapshotPath)
	default:
		if loadResult.WrittenAt.After(time.Now()) {
			logger.Warn("State snapshot written_at is in the future, clock may have stepped backward",
				"written_at", loadResult.WrittenAt)
		}
		report, restoreErr := store.Restore(loadResult.Agents, loadResult.PausedAgents)
		if restoreErr != nil {
			fmt.Fprintf(os.Stderr, "error: state snapshot restore: %s\n", restoreErr.Error())
			os.Exit(1)
		}
		for _, discard := range report.Discards {
			logger.Warn("Saved budget state discarded, identity mismatch",
				"agent", discard.Agent, "budget_index", discard.BudgetIndex, "field", discard.Field)
		}
		for _, staleName := range report.DiscardedPaused {
			logger.Warn("Saved pause discarded, agent no longer configured", "agent", staleName)
		}
		logger.Info("State snapshot restored",
			"path", cfg.State.SnapshotPath,
			"age_seconds", int64(time.Since(loadResult.WrittenAt).Seconds()),
			"restored_budgets", report.RestoredBudgets,
			"restored_paused", report.RestoredPaused,
			"absent_agents", report.AbsentAgents)
	}

	agentNames := make([]string, 0, len(cfg.Agents))
	for _, configuredAgent := range cfg.Agents {
		agentNames = append(agentNames, configuredAgent.Name)
	}
	providerNames := make([]string, 0, len(cfg.Providers))
	for _, configuredProvider := range cfg.Providers {
		providerNames = append(providerNames, configuredProvider.Name)
	}
	recorder := metrics.New(agentNames, providerNames)

	// Proxy server: handles agent traffic
	proxyHandler, err := proxy.New(cfg, store, recorder, logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %s\n", err.Error())
		os.Exit(1)
	}

	snapshotInterval, err := time.ParseDuration(cfg.State.SnapshotInterval)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: state.snapshot_interval: %s\n", err.Error())
		os.Exit(1)
	}
	snapshotter := state.NewSnapshotter(store, cfg.State.SnapshotPath, snapshotInterval, logger)
	if err := snapshotter.ProbeWritable(); err != nil {
		fmt.Fprintf(os.Stderr, "error: state snapshot directory: %s\n", err.Error())
		os.Exit(1)
	}

	proxyAddr := fmt.Sprintf("0.0.0.0:%d", cfg.Listen.ProxyPort)
	proxyServer := &http.Server{
		Addr:        proxyAddr,
		Handler:     proxyHandler,
		ReadTimeout: 30 * time.Second,
		// WriteTimeout intentionally unset (0 = no deadline).
		// Streaming SSE responses from LLM providers can exceed any fixed timeout,
		// and a WriteTimeout would sever healthy long streams. Per ADR-005, a
		// streaming connection that goes silent after headers is currently bounded
		// only by the downstream client until the Session 6 idle watchdog lands.
		// Non-streaming connections are bounded by the provider request timeout.
		IdleTimeout: 60 * time.Second,
	}

	// Admin server: health, metrics, budget management
	adminMux := http.NewServeMux()
	adminMux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		health := map[string]any{
			"status":  "ok",
			"version": version,
		}
		// last_snapshot_at appears only after the first successful write.
		// Its absence right after startup means not-yet-ticked, not broken.
		if lastSuccess, ok := snapshotter.LastSuccess(); ok {
			health["last_snapshot_at"] = lastSuccess.Format(time.RFC3339)
			health["snapshot_age_seconds"] = int64(time.Since(lastSuccess).Seconds())
		}
		_ = json.NewEncoder(w).Encode(health)
	})
	adminMux.Handle("/metrics", recorder.Handler())

	adminBind := cfg.Listen.AdminBind
	if adminBind == "" {
		adminBind = "127.0.0.1"
	}

	admin.Register(adminMux, admin.AgentInfosFromConfig(cfg.Agents), store, snapshotter, adminBind, logger)

	if cfg.Defaults.UnknownAgent != "block" {
		logger.Warn("Unknown-agent policy is passthrough, pause and budgets do not cover unidentified traffic",
			"policy", cfg.Defaults.UnknownAgent)
	}
	if bindIP := net.ParseIP(adminBind); adminBind != "localhost" && (bindIP == nil || !bindIP.IsLoopback()) {
		logger.Warn("Admin API bound to a non-loopback address with no authentication", "bind", adminBind)
	}
	for _, plaintext := range plaintextUpstreams(cfg.Providers) {
		logger.Warn("Provider upstream is plaintext, pass-through API keys are visible to local processes",
			"provider", plaintext.Name, "upstream", plaintext.Upstream)
	}

	adminAddr := fmt.Sprintf("%s:%d", adminBind, cfg.Listen.AdminPort)
	adminServer := &http.Server{
		Addr: adminAddr,
		// GuardLoopback covers the WHOLE listener, so health and metrics
		// (registered directly on adminMux above) get the same DNS-rebinding
		// Host check as the agent routes. Metrics expose the agent roster
		// and per-agent telemetry, which a rebound page must not read.
		Handler:      admin.GuardLoopback(adminMux, adminBind),
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  30 * time.Second,
	}

	if err := snapshotter.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "error: state snapshot loop: %s\n", err.Error())
		os.Exit(1)
	}

	// Start both servers, using an error channel for orderly failure handling
	errCh := make(chan error, 2)

	go func() {
		logger.Info("admin server listening", "addr", adminAddr)
		if err := adminServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("admin server: %w", err)
		}
	}()

	go func() {
		logger.Info("proxy server listening", "addr", proxyAddr)
		if err := proxyServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("proxy server: %w", err)
		}
	}()

	logger.Info("levee started",
		"proxy_addr", proxyAddr,
		"admin_addr", adminAddr,
	)

	// Wait for shutdown signal or server failure
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	serveFailed := false
	select {
	case sig := <-quit:
		logger.Info("shutting down", "signal", sig.String())
	case err := <-errCh:
		logger.Error("server failed, shutting down", "error", err)
		serveFailed = true
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := proxyServer.Shutdown(ctx); err != nil {
		logger.Error("proxy server shutdown error", "error", err)
	}
	if err := adminServer.Shutdown(ctx); err != nil {
		logger.Error("admin server shutdown error", "error", err)
	}

	// Join the loop BEFORE the final write so no ticker write can race it or
	// land after it. On any server-failure shutdown, whether a startup bind
	// failure or a runtime listener error after hours of healthy serving, the
	// final write is skipped. That loses at most one snapshot_interval of
	// usage beyond the last periodic tick, accepted because the alternative
	// is worse: a rolling restart's freshly bind-failed instance would
	// otherwise overwrite the exiting instance's final snapshot with its own
	// stale restored state.
	snapshotter.Stop()
	if !serveFailed {
		droppedReservations := store.OutstandingReservations()
		if err := snapshotter.WriteOnce(); err != nil {
			logger.Error("Final state snapshot failed",
				"error", err.Error(),
				"dropped_reservations", droppedReservations)
		} else {
			logger.Info("Final state snapshot written",
				"path", cfg.State.SnapshotPath,
				"dropped_reservations", droppedReservations)
		}
	}

	logger.Info("levee stopped")
	if serveFailed {
		// Supervisors gate restart and alerting on the exit code.
		os.Exit(1)
	}
}
