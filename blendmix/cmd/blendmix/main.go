package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sahand363/blendartai/blendmix/internal/config"
	"github.com/sahand363/blendartai/blendmix/internal/gemini"
	"github.com/sahand363/blendartai/blendmix/internal/runner"
	"github.com/sahand363/blendartai/blendmix/internal/store"
	"github.com/sahand363/blendartai/blendmix/internal/web"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	cfg, err := config.Load()
	if err != nil {
		logger.Error("invalid configuration", "error", err)
		os.Exit(1)
	}

	st, err := store.New(cfg.OutDir)
	if err != nil {
		logger.Error("failed to initialize store", "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	gemClient, err := gemini.NewClient(ctx, cfg.GeminiAPIKey, cfg.TextModel, cfg.ImageModel, cfg.ImageSize)
	if err != nil {
		logger.Error("failed to initialize gemini client", "error", err)
		os.Exit(1)
	}

	r := runner.New(cfg, gemClient, st, logger)
	srv := web.NewServer(cfg, r, st, logger)

	httpServer := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("starting blendmix server",
			"addr", cfg.ListenAddr,
			"base_path", cfg.BasePath,
			"source_dir", cfg.SourceDir,
			"out_dir", cfg.OutDir,
			"text_model", cfg.TextModel,
			"image_model", cfg.ImageModel,
		)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
		logger.Info("shutdown signal received")
	case err := <-errCh:
		logger.Error("server error", "error", err)
	}

	r.Abort()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		logger.Error("http shutdown error", "error", err)
	}
	logger.Info("blendmix stopped")
}
