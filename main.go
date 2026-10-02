// Command volc-stt-proxy serves the OpenAI speech-to-text API
// (POST /v1/audio/transcriptions) and forwards audio to Volcengine's
// Doubao streaming ASR 2.0 in "一句话识别" mode (WebSocket bigmodel_nostream).
package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	fatal := func(msg string, err error) {
		logger.Error(msg, "err", err)
		os.Exit(1)
	}

	if err := loadDotEnv(".env"); err != nil {
		fatal("loading .env", err)
	}
	cfg, err := loadConfig(os.Getenv)
	if err != nil {
		fatal("invalid configuration", err)
	}
	if cfg.FFmpegPath, err = resolveFFmpeg(cfg.FFmpegPath); err != nil {
		fatal("invalid configuration", err)
	}

	// Request contexts derive from baseCtx, so cancelling it aborts in-flight
	// work (ffmpeg is killed, temp files removed) if shutdown takes too long.
	baseCtx, cancelRequests := context.WithCancel(context.Background())
	defer cancelRequests()
	app := newServer(cfg, logger)
	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           app.routes(),
		BaseContext:       func(net.Listener) context.Context { return baseCtx },
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       cfg.Timeout,              // bounds slow uploads
		WriteTimeout:      cfg.maxRequestDuration(), // never cuts off a max-length dictation
		IdleTimeout:       2 * time.Minute,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		logger.Info("shutting down")
		// launchd sends SIGKILL 20s after SIGTERM by default; stay well below.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logger.Warn("graceful shutdown timed out; aborting in-flight requests", "err", err)
			cancelRequests()
			srv.Close() // close active connections so stalled body reads return
			waited := make(chan struct{})
			go func() { app.wg.Wait(); close(waited) }()
			select {
			case <-waited:
			case <-time.After(3 * time.Second):
			}
		}
	}()

	logger.Info("volc-stt-proxy listening",
		"addr", cfg.ListenAddr, "endpoint", cfg.Endpoint, "resource_id", cfg.ResourceID,
		"model_name", cfg.ModelName, "language", cfg.Language, "enable_ddc", cfg.EnableDDC, "auth", cfg.authMode(),
		"proxy_auth", cfg.ProxyAPIKey != "", "ffmpeg", cfg.FFmpegPath, "timeout", cfg.Timeout,
		"max_audio_sec", cfg.MaxAudioSec, "max_upload_mb", cfg.MaxUpload>>20,
		"timeout_per_audio_sec", cfg.PerAudioSec, "max_request", cfg.maxRequestDuration())
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		fatal("server error", err)
	}
	<-shutdownDone
	logger.Info("shut down")
}
