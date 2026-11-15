package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to YAML configuration")
	flag.Parse()
	if err := run(*configPath); err != nil {
		fmt.Fprintf(os.Stderr, "naive-cacher: %v\n", err)
		os.Exit(1)
	}
}

func run(configPath string) error {
	cfg, err := LoadConfig(configPath)
	if err != nil {
		return err
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(cfg.LogLevel)); err != nil {
		return err
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	proxy, err := NewProxy(cfg, logger)
	if err != nil {
		return err
	}
	server := &http.Server{
		Addr:              cfg.Listen,
		Handler:           proxy,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout.Duration,
		IdleTimeout:       cfg.IdleTimeout.Duration,
	}
	serverErrors := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", cfg.Listen)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErrors <- err
		}
	}()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(signals)
	select {
	case err := <-serverErrors:
		return fmt.Errorf("server failed: %w", err)
	case receivedSignal := <-signals:
		logger.Info("shutting down", "signal", receivedSignal.String())
	}
	shutdownContext, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout.Duration)
	defer cancel()
	if err := server.Shutdown(shutdownContext); err != nil {
		server.Close()
		return fmt.Errorf("shutdown failed: %w", err)
	}
	proxy.CloseIdleConnections()
	logger.Info("stopped")
	return nil
}
