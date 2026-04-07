// Package main is the entrypoint for the Open Streamer server.
// It loads configuration, wires all dependencies via samber/do, and starts all services.
// Graceful shutdown is triggered on SIGINT or SIGTERM.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ntthuan060102github/open-streamer/config"
	"github.com/ntthuan060102github/open-streamer/internal/api"
	"github.com/ntthuan060102github/open-streamer/internal/api/handler"
	"github.com/ntthuan060102github/open-streamer/internal/buffer"
	"github.com/ntthuan060102github/open-streamer/internal/coordinator"
	"github.com/ntthuan060102github/open-streamer/internal/dvr"
	"github.com/ntthuan060102github/open-streamer/internal/events"
	"github.com/ntthuan060102github/open-streamer/internal/hooks"
	"github.com/ntthuan060102github/open-streamer/internal/ingestor"
	"github.com/ntthuan060102github/open-streamer/internal/manager"
	"github.com/ntthuan060102github/open-streamer/internal/metrics"
	"github.com/ntthuan060102github/open-streamer/internal/publisher"
	"github.com/ntthuan060102github/open-streamer/internal/store"
	jsonstore "github.com/ntthuan060102github/open-streamer/internal/store/json"
	mongostore "github.com/ntthuan060102github/open-streamer/internal/store/mongo"
	sqlstore "github.com/ntthuan060102github/open-streamer/internal/store/sql"
	"github.com/ntthuan060102github/open-streamer/internal/transcoder"
	"github.com/ntthuan060102github/open-streamer/pkg/logger"
	"github.com/samber/do/v2"
	"golang.org/x/sync/errgroup"
)

func main() {
	if err := run(); err != nil {
		slog.Error("server: fatal error", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	log := logger.New(cfg.Log)
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	injector := do.New()

	if err := wire(injector, cfg); err != nil {
		return fmt.Errorf("wire dependencies: %w", err)
	}

	return startAll(ctx, injector)
}

// wire registers all services into the DI injector.
func wire(i *do.RootScope, cfg *config.Config) error {
	// Config
	do.ProvideValue(i, cfg)

	// Storage — select backend based on cfg.Storage.Driver
	if err := wireStorage(i, cfg); err != nil {
		return err
	}

	// Infrastructure
	do.Provide(i, func(_ do.Injector) (events.Bus, error) {
		bus := events.New(cfg.Hooks.WorkerCount, 512)
		return bus, nil
	})

	// Core services (registration order matters for DI)
	do.Provide(i, buffer.New)
	do.Provide(i, ingestor.New)
	do.Provide(i, manager.New)
	do.Provide(i, transcoder.New)
	do.Provide(i, publisher.New)
	do.Provide(i, dvr.New)
	do.Provide(i, hooks.New)
	do.Provide(i, metrics.New)
	do.Provide(i, coordinator.New)

	// API handlers
	do.Provide(i, handler.NewStreamHandler)
	do.Provide(i, handler.NewRecordingHandler)
	do.Provide(i, handler.NewHookHandler)
	do.Provide(i, api.New)

	return nil
}

// wireStorage connects to the configured storage backend and registers
// the three repository interfaces into the DI injector.
func wireStorage(i *do.RootScope, cfg *config.Config) error {
	switch cfg.Storage.Driver {
	case "sql":
		s, err := sqlstore.New(cfg.Storage.SQLDSN)
		if err != nil {
			return fmt.Errorf("sql store: %w", err)
		}
		migrateCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := s.Migrate(migrateCtx); err != nil {
			return fmt.Errorf("sql store: migrate: %w", err)
		}
		do.ProvideValue(i, s.Streams())
		do.ProvideValue(i, s.Recordings())
		do.ProvideValue(i, s.Hooks())

	case "mongo":
		connCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		s, err := mongostore.New(connCtx, cfg.Storage.MongoURI, cfg.Storage.MongoDatabase)
		if err != nil {
			return fmt.Errorf("mongo store: %w", err)
		}
		idxCtx, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel2()
		if err := s.EnsureIndexes(idxCtx); err != nil {
			return fmt.Errorf("mongo store: ensure indexes: %w", err)
		}
		do.ProvideValue(i, s.Streams())
		do.ProvideValue(i, s.Recordings())
		do.ProvideValue(i, s.Hooks())

	default: // "json" or empty
		s, err := jsonstore.New(cfg.Storage.JSONDir)
		if err != nil {
			return fmt.Errorf("json store: %w", err)
		}
		do.ProvideValue(i, s.Streams())
		do.ProvideValue(i, s.Recordings())
		do.ProvideValue(i, s.Hooks())
	}

	slog.Info("server: storage backend ready", "driver", cfg.Storage.Driver)
	return nil
}

// startAll starts all long-running services concurrently.
// All services share the same context and stop together on shutdown.
func startAll(ctx context.Context, i *do.RootScope) error {
	bus := do.MustInvoke[events.Bus](i)
	events.Start(ctx, bus)

	ing := do.MustInvoke[*ingestor.Service](i)
	pub := do.MustInvoke[*publisher.Service](i)
	coord := do.MustInvoke[*coordinator.Coordinator](i)
	streamRepo := do.MustInvoke[store.StreamRepository](i)

	hookSvc := do.MustInvoke[*hooks.Service](i)
	apiSrv := do.MustInvoke[*api.Server](i)

	ing.SetRTMPPlayHandler(pub.HandleRTMPPlay)

	g, gCtx := errgroup.WithContext(ctx)

	g.Go(func() error { return ing.Run(gCtx) })
	g.Go(func() error { return pub.RunRTSPPlayServer(gCtx) })
	g.Go(func() error { return pub.RunRTMPPlayServer(gCtx) })

	// Best-effort: let RTMP/SRT listeners bind before registering push routes from persisted streams.
	time.Sleep(50 * time.Millisecond)
	coordinator.BootstrapPersistedStreams(ctx, slog.Default(), streamRepo, coord)

	g.Go(func() error { return hookSvc.Start(gCtx) })
	g.Go(func() error { return apiSrv.Start(gCtx) })

	slog.Info("server: all services started")

	if err := g.Wait(); err != nil {
		return err
	}

	slog.Info("server: shutdown complete")

	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if err := i.ShutdownWithContext(shutdownCtx); err != nil {
		slog.Warn("server: injector shutdown", "err", err)
	}

	return nil
}
