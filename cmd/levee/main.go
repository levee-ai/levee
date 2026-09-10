// Package main is the entrypoint for the Levee budget-enforcement proxy.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

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
		fmt.Fprintf(os.Stderr, "error: %s\n", err.Error())
		os.Exit(1)
	}

	loadResult, err := state.Load(cfg.State.SnapshotPath, time.Now)
	if err != nil {
		// Fail-safe: an unreadable or unknown-version state file refuses
		// startup rather than zeroing the ledger.
		fmt.Fprintf(os.Stderr, "error: %s\n", err.Error())
		os.Exit(1)
	}
	switch {
	case loadResult.CorruptAside != "":
		logger.Error("State snapshot was corrupt, starting fresh",
			"path", cfg.State.SnapshotPath, "moved_to", loadResult.CorruptAside)
	case loadResult.Fresh:
		logger.Info("No prior state snapshot, starting fresh", "path", cfg.State.SnapshotPath)
	default:
		if loadResult.WrittenAt.After(time.Now()) {
			logger.Warn("State snapshot written_at is in the future, clock may have stepped backward",
				"written_at", loadResult.WrittenAt)
		}
		report, restoreErr := store.Restore(loadResult.Agents)
		if restoreErr != nil {
			fmt.Fprintf(os.Stderr, "error: %s\n", restoreErr.Error())
			os.Exit(1)
		}
		for _, discard := range report.Discards {
			logger.Warn("Saved budget state discarded, identity mismatch",
				"agent", discard.Agent, "budget_index", discard.BudgetIndex, "field", discard.Field)
		}
		logger.Info("State snapshot restored",
			"path", cfg.State.SnapshotPath,
			"age_seconds", int64(time.Since(loadResult.WrittenAt).Seconds()),
			"restored_budgets", report.RestoredBudgets,
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
		fmt.Fprintf(os.Stderr, "error: %s\n", err.Error())
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
	adminAddr := fmt.Sprintf("%s:%d", adminBind, cfg.Listen.AdminPort)
	adminServer := &http.Server{
		Addr:         adminAddr,
		Handler:      adminMux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  30 * time.Second,
	}

	if err := snapshotter.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %s\n", err.Error())
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
	// land after it. Skip the final write on a server failure: in a rolling
	// restart, an instance that failed to bind would otherwise write its stale
	// restored state over the exiting instance's final snapshot.
	snapshotter.Stop()
	if !serveFailed {
		droppedReservations := store.OutstandingReservations()
		if err := snapshotter.WriteOnce(); err != nil {
			logger.Error("Final state snapshot failed", "error", err.Error())
		} else {
			logger.Info("Final state snapshot written",
				"path", cfg.State.SnapshotPath,
				"dropped_reservations", droppedReservations)
		}
	}

	logger.Info("levee stopped")
}
