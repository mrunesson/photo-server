// Package main wires and runs the photo slideshow HTTP service.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"photo-server/internal/eventlog"
	"photo-server/internal/filesystem"
	"photo-server/internal/httpapi"
	"photo-server/internal/photo"
)

func main() {
	if err := run(); err != nil {
		slog.Error("photo server stopped", "error", err)
		os.Exit(1)
	}
}

func run() (runErr error) {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	photoRoot := envOrDefault("PHOTO_DIR", "./photos")
	eventLogPath := envOrDefault("EVENT_LOG", "./events.jsonl")
	address := envOrDefault("HTTP_ADDR", "127.0.0.1:8080")

	events, err := eventlog.Open(eventLogPath)
	if err != nil {
		return fmt.Errorf("initialize event log: %w", err)
	}
	defer func() {
		runErr = errors.Join(runErr, events.Close())
	}()

	catalog, err := filesystem.NewCatalog(photoRoot)
	if err != nil {
		return fmt.Errorf("initialize photo catalog: %w", err)
	}
	defer func() {
		runErr = errors.Join(runErr, catalog.Close())
	}()

	service := photo.NewService(catalog, events)
	handler := httpapi.NewHandler(service, logger)
	server := &http.Server{
		Addr:              address,
		Handler:           requestLogger(logger, handler),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      2 * time.Minute,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", address, err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	serveResult := make(chan error, 1)
	go func() {
		serveResult <- server.Serve(listener)
	}()
	logger.Info("photo server listening", "address", address, "photo_root", photoRoot, "event_log", eventLogPath)

	select {
	case err := <-serveResult:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve HTTP: %w", err)
		}
	case <-ctx.Done():
		shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownContext); err != nil {
			closeErr := server.Close()
			return errors.Join(fmt.Errorf("shut down HTTP server: %w", err), closeErr)
		}
		if err := <-serveResult; err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve HTTP during shutdown: %w", err)
		}
	}
	return nil
}

func envOrDefault(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (w *statusRecorder) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusRecorder) Write(contents []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(contents)
}

func (w *statusRecorder) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func requestLogger(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		recorder := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(recorder, r)
		status := recorder.status
		if status == 0 {
			status = http.StatusOK
		}
		logger.Info("http request", "method", r.Method, "status", status, "duration", time.Since(started))
	})
}
