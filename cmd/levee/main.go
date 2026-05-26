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

	"github.com/levee-ai/levee/internal/config"
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

	// Proxy server: handles agent traffic
	proxyMux := http.NewServeMux()
	proxyMux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Placeholder: will be replaced by actual proxy logic
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = fmt.Fprintf(w, `{"error":"proxy not yet implemented"}`)
	})

	proxyAddr := fmt.Sprintf("0.0.0.0:%d", cfg.Listen.ProxyPort)
	proxyServer := &http.Server{
		Addr:        proxyAddr,
		Handler:     proxyMux,
		ReadTimeout: 30 * time.Second,
		// WriteTimeout intentionally unset (0 = no deadline).
		// Streaming SSE responses from LLM providers can exceed any fixed timeout.
		// Connection lifetimes are bounded by provider timeouts and budget enforcement.
		IdleTimeout: 60 * time.Second,
	}

	// Admin server: health, metrics, budget management
	adminMux := http.NewServeMux()
	adminMux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"status":  "ok",
			"version": version,
		})
	})

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

	select {
	case sig := <-quit:
		logger.Info("shutting down", "signal", sig.String())
	case err := <-errCh:
		logger.Error("server failed, shutting down", "error", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := proxyServer.Shutdown(ctx); err != nil {
		logger.Error("proxy server shutdown error", "error", err)
	}
	if err := adminServer.Shutdown(ctx); err != nil {
		logger.Error("admin server shutdown error", "error", err)
	}

	logger.Info("levee stopped")
}
