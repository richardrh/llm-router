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
	"sync"
	"syscall"
	"time"

	"llm-router/internal/config"
	routerpkg "llm-router/internal/router"
	"llm-router/internal/usage"
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

	cfg, err := config.LoadConfig(*configPath)
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
		for _, name := range cfg.UpstreamsInOrder() {
			u := cfg.Upstreams[name]
			if u.KindValue() == config.UpstreamCLI || u.KindValue() == config.UpstreamCodex {
				// No credential to report: the command owns it.
				log.Info("upstream ok", "name", name, "kind", u.KindValue(), "command", strings.Join(u.Command, " "))
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
				targets = append(targets, fmt.Sprintf("%s/%s(w=%d)", t.Upstream, t.Model, t.WeightValue()))
			}
			log.Info("model ok", "alias", name, "api", string(a.API), "targets", targets)
		}
		log.Info("config is valid", "models", len(cfg.Models), "upstreams", len(cfg.Upstreams))
		return nil
	}

	router, err := routerpkg.NewRouter(cfg, log)
	if err != nil {
		return err
	}
	defer router.Close()
	store, err := usage.OpenStoreForConfig(cfg, log)
	if err != nil {
		return err
	}
	// Flushes what is queued and releases the file. Close is safe on a nil
	// store, so this needs no guard for the feature being off.
	defer store.Close()

	srv := routerpkg.NewServer(cfg, router, log, store)
	defer srv.Close()

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen %s: %w", cfg.Listen, err)
	}
	defer ln.Close()

	// Track connection lifetimes, not just Shutdown's graceful-drain result.
	// Close interrupts I/O but does not wait for handlers to finish accounting.
	var connections sync.WaitGroup
	httpSrv := &http.Server{
		Handler: srv.Handler(),
		// No WriteTimeout: a coding agent can hold one stream for many
		// minutes, and a write deadline would sever it mid-turn.
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
		ConnState: func(_ net.Conn, state http.ConnState) {
			switch state {
			case http.StateNew:
				connections.Add(1)
			case http.StateClosed, http.StateHijacked:
				connections.Done()
			}
		},
	}

	usageStore := "off"
	if cfg.Store.Path != "" {
		usageStore = cfg.Store.Path
	}
	log.Info("omp-router listening",
		"addr", ln.Addr().String(),
		"aliases", len(cfg.Models),
		"upstreams", len(cfg.Upstreams),
		"auth", cfg.APIKey != "",
		"usage_store", usageStore)

	errCh := make(chan error, 1)
	served := make(chan struct{})
	go func() {
		defer close(served)
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()
	defer func() {
		// Also runs on Serve errors and graceful-drain timeout. Waiting for
		// Serve to return ensures no new connection can register before Wait.
		_ = httpSrv.Close()
		<-served
		connections.Wait()
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	select {
	case err := <-errCh:
		return err
	case sig := <-stop:
		log.Info("shutting down", "signal", sig.String())
	}

	// Cancel console work alongside the HTTP drain. The deferred Close joins
	// every console worker before the usage store can close.
	go srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return httpSrv.Shutdown(ctx)
}

func sortedModelNames(cfg *config.Config) []string {
	names := make([]string, 0, len(cfg.Models))
	for n := range cfg.Models {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}
