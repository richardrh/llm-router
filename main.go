package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		configPath = flag.String("config", "router.yaml", "path to the YAML config")
		listenFlag = flag.String("listen", "", "override the listen address from config")
		checkOnly  = flag.Bool("check", false, "validate the config and exit")
	)
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		return err
	}
	// Findings that are not fatal but that the operator should see before the
	// router starts serving: metadata that disagrees with the capability table,
	// for instance.
	for _, w := range cfg.Warnings {
		log.Warn("config warning", "detail", w)
	}
	if *listenFlag != "" {
		cfg.Listen = *listenFlag
	}

	if *checkOnly {
		for _, name := range cfg.upstreamsInOrder() {
			u := cfg.Upstreams[name]
			if u.kind() == UpstreamCLI {
				// No credential to report: the command owns it.
				log.Info("upstream ok", "name", name, "kind", "cli", "command", strings.Join(u.Command, " "))
				continue
			}
			state := "env:" + u.APIKeyEnv
			if u.APIKey != "" {
				state = "literal"
			}
			if u.APIKeyEnv != "" && os.Getenv(u.APIKeyEnv) == "" {
				state += " (UNSET)"
			}
			log.Info("upstream ok", "name", name, "kind", "http", "baseUrl", u.BaseURL, "credential", state)
		}
		for _, name := range sortedModelNames(cfg) {
			a := cfg.Models[name]
			targets := make([]string, 0, len(a.Targets))
			for _, t := range a.Targets {
				targets = append(targets, fmt.Sprintf("%s/%s(w=%d)", t.Upstream, t.Model, t.weight()))
			}
			log.Info("model ok", "alias", name, "api", string(a.API), "targets", targets)
		}
		log.Info("config is valid", "models", len(cfg.Models), "upstreams", len(cfg.Upstreams))
		return nil
	}

	router, err := NewRouter(cfg, log)
	if err != nil {
		return err
	}
	srv := NewServer(cfg, router, log)

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen %s: %w", cfg.Listen, err)
	}

	httpSrv := &http.Server{
		Handler: srv.Handler(),
		// No WriteTimeout: a coding agent can hold one stream for many
		// minutes, and a write deadline would sever it mid-turn.
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}

	log.Info("omp-router listening",
		"addr", ln.Addr().String(),
		"aliases", len(cfg.Models),
		"upstreams", len(cfg.Upstreams),
		"auth", cfg.APIKey != "")

	errCh := make(chan error, 1)
	go func() {
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	select {
	case err := <-errCh:
		return err
	case sig := <-stop:
		log.Info("shutting down", "signal", sig.String())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return httpSrv.Shutdown(ctx)
}

func sortedModelNames(cfg *Config) []string {
	names := make([]string, 0, len(cfg.Models))
	for n := range cfg.Models {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}
