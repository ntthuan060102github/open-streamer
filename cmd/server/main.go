// Package main is the entrypoint for the Open Streamer server.
// It loads StorageConfig from file/env, loads GlobalConfig from the store,
// wires all dependencies via samber/do, and starts services via RuntimeManager.
// Graceful shutdown is triggered on SIGINT or SIGTERM.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/ntt0601zcoder/open-streamer/config"
	"github.com/ntt0601zcoder/open-streamer/internal/api"
	"github.com/ntt0601zcoder/open-streamer/internal/api/handler"
	"github.com/ntt0601zcoder/open-streamer/internal/buffer"
	"github.com/ntt0601zcoder/open-streamer/internal/coordinator"
	"github.com/ntt0601zcoder/open-streamer/internal/domain"
	"github.com/ntt0601zcoder/open-streamer/internal/dvr"
	"github.com/ntt0601zcoder/open-streamer/internal/events"
	"github.com/ntt0601zcoder/open-streamer/internal/hooks"
	"github.com/ntt0601zcoder/open-streamer/internal/hwdetect"
	"github.com/ntt0601zcoder/open-streamer/internal/ingestor"
	"github.com/ntt0601zcoder/open-streamer/internal/manager"
	"github.com/ntt0601zcoder/open-streamer/internal/metrics"
	"github.com/ntt0601zcoder/open-streamer/internal/publisher"
	"github.com/ntt0601zcoder/open-streamer/internal/runtime"
	"github.com/ntt0601zcoder/open-streamer/internal/store"
	jsonstore "github.com/ntt0601zcoder/open-streamer/internal/store/json"
	yamlstore "github.com/ntt0601zcoder/open-streamer/internal/store/yaml"
	"github.com/ntt0601zcoder/open-streamer/internal/transcoder"
	"github.com/ntt0601zcoder/open-streamer/internal/vod"
	"github.com/ntt0601zcoder/open-streamer/pkg/logger"
	"github.com/q191201771/naza/pkg/nazalog"
	"github.com/samber/do/v2"
)

func main() {
	if err := run(); err != nil {
		slog.Error("server: fatal error", "err", err)
		os.Exit(1)
	}
}

func run() error {
	// 1. Load StorageConfig from file/env — only storage settings come from viper.
	storageCfg, err := config.LoadStorage()
	if err != nil {
		return fmt.Errorf("load storage config: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	injector := do.New()

	// 2. Open storage backend and register repositories (including SettingsRepository).
	if err := wireStorage(injector, storageCfg); err != nil {
		return fmt.Errorf("wire storage: %w", err)
	}

	// 3. Load GlobalConfig from store, seeding defaults on first boot.
	gcRepo := do.MustInvoke[store.GlobalConfigRepository](injector)
	gcfg, err := loadGlobalConfig(gcRepo)
	if err != nil {
		return fmt.Errorf("load global config: %w", err)
	}

	// 4. Provide individual sub-configs to DI so services can read them at construction time.
	provideSubConfigs(injector, gcfg)

	// 4b. Probe FFmpeg before any service starts. A missing required encoder
	// (libx264 / aac / mpegts) means transcoding will crash on every stream
	// — fail fast with a clear error instead of accepting traffic and
	// erroring per-stream later. Path defaults to "ffmpeg" via $PATH when
	// gcfg.Transcoder.FFmpegPath is empty (matches publisher.NewService).
	ffmpegPath := ""
	if gcfg.Transcoder != nil {
		ffmpegPath = gcfg.Transcoder.FFmpegPath
	}
	// Auto-detect host hardware backends — probe warnings only cover
	// what's actually installed (no NVENC noise on a CPU-only host).
	probeRes, probeErr := transcoder.Probe(ctx, ffmpegPath, hwdetect.Available())
	if probeErr != nil {
		return fmt.Errorf("ffmpeg probe: %w", probeErr)
	}
	if !probeRes.OK {
		return fmt.Errorf("ffmpeg %q at %s is incompatible: %s",
			probeRes.Version, probeRes.Path, strings.Join(probeRes.Errors, "; "))
	}
	slog.Info("ffmpeg probe ok",
		"path", probeRes.Path,
		"version", probeRes.Version,
		"warnings", len(probeRes.Warnings),
	)
	for _, w := range probeRes.Warnings {
		slog.Warn("ffmpeg capability missing", "msg", w)
	}

	// Init logger from store config.
	if gcfg.Log != nil {
		slog.SetDefault(logger.New(*gcfg.Log))
	}

	// Silence lal's nazalog INFO-level chatter (every RTMP "Acknowledgement"
	// and lifecycle line) — keep warn/error so real failures still surface.
	// Without this the journalctl tail is dominated by per-second ack lines
	// from each push session, drowning out our own structured slog output.
	_ = nazalog.Init(func(o *nazalog.Option) {
		o.Level = nazalog.LevelWarn
	})

	// 5. Wire all services.
	wireServices(injector)

	// 6. Assemble RuntimeManager deps from DI.
	rtm := runtime.New(ctx, runtime.Deps{
		Ingestor:         do.MustInvoke[*ingestor.Service](injector),
		Publisher:        do.MustInvoke[*publisher.Service](injector),
		Coordinator:      do.MustInvoke[*coordinator.Coordinator](injector),
		Transcoder:       do.MustInvoke[*transcoder.Service](injector),
		HooksSvc:         do.MustInvoke[*hooks.Service](injector),
		APISrv:           do.MustInvoke[*api.Server](injector),
		Bus:              do.MustInvoke[events.Bus](injector),
		StreamRepo:       do.MustInvoke[store.StreamRepository](injector),
		GlobalConfigRepo: gcRepo,
	})

	// 7. Inject RuntimeManager into ConfigHandler (breaks circular DI dependency).
	configH := do.MustInvoke[*handler.ConfigHandler](injector)
	configH.SetRuntimeManager(rtm)

	// 7b. Wire the upstream-stream lookup into the ingestor so `copy://`
	// inputs can find the buffer to subscribe to. Done here (post-DI) to
	// avoid the ingestor package depending on the store layer.
	wireCopyLookup(injector)

	// 8. Hydrate VOD registry from the store so ingestor workers started in
	//    bootstrap can resolve file:// URLs against the user's mount table.
	if err := hydrateVODRegistry(ctx, injector); err != nil {
		return fmt.Errorf("hydrate vod registry: %w", err)
	}

	// 9. Start all configured services.
	rtm.BootstrapWith(gcfg)
	slog.Info("server: all services started")

	// 9. Block until root context is cancelled (SIGINT/SIGTERM).
	<-ctx.Done()
	slog.Info("server: shutting down...")

	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if err := injector.ShutdownWithContext(shutdownCtx); err != nil {
		slog.Warn("server: injector shutdown", "err", err)
	}

	slog.Info("server: shutdown complete")
	return nil
}

// wireStorage connects to the configured storage backend and registers
// all repository interfaces (including SettingsRepository) into the DI injector.
func wireStorage(i *do.RootScope, cfg config.StorageConfig) error {
	switch cfg.Driver {
	case "json":
		s, err := jsonstore.New(cfg.JSONDir)
		if err != nil {
			return fmt.Errorf("json store: %w", err)
		}
		do.ProvideValue(i, s.Streams())
		do.ProvideValue(i, s.Recordings())
		do.ProvideValue(i, s.Hooks())
		do.ProvideValue(i, s.GlobalConfig())
		do.ProvideValue(i, s.VOD())

	default: // "yaml" or empty
		s, err := yamlstore.New(cfg.YAMLDir)
		if err != nil {
			return fmt.Errorf("yaml store: %w", err)
		}
		do.ProvideValue(i, s.Streams())
		do.ProvideValue(i, s.Recordings())
		do.ProvideValue(i, s.Hooks())
		do.ProvideValue(i, s.GlobalConfig())
		do.ProvideValue(i, s.VOD())
	}

	slog.Info("server: storage backend ready", "driver", cfg.Driver)
	return nil
}

// provideSubConfigs extracts individual sub-configs from the GlobalConfig and
// registers them in the DI injector so each service only sees its own config type.
// Zero-value configs are provided for nil sections so DI constructors never panic.
func provideSubConfigs(i *do.RootScope, gcfg *domain.GlobalConfig) {
	do.ProvideValue(i, deref(gcfg.Server))
	do.ProvideValue(i, deref(gcfg.Listeners))
	do.ProvideValue(i, deref(gcfg.Ingestor))
	do.ProvideValue(i, deref(gcfg.Buffer))
	do.ProvideValue(i, deref(gcfg.Transcoder))
	do.ProvideValue(i, deref(gcfg.Publisher))
	do.ProvideValue(i, deref(gcfg.Manager))
	do.ProvideValue(i, deref(gcfg.Hooks))
	do.ProvideValue(i, deref(gcfg.Log))
}

func deref[T any](p *T) T {
	if p != nil {
		return *p
	}
	var zero T
	return zero
}

// wireServices registers all non-storage services into the DI injector.
func wireServices(i *do.RootScope) {
	// Infrastructure
	do.Provide(i, func(inj do.Injector) (events.Bus, error) {
		cfg := do.MustInvoke[config.HooksConfig](inj)
		bus := events.New(cfg.WorkerCount, 512)
		return bus, nil
	})

	// Core services
	do.Provide(i, buffer.New)
	do.Provide(i, func(do.Injector) (*vod.Registry, error) { return vod.NewRegistry(), nil })
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
	do.Provide(i, handler.NewConfigHandler)
	do.Provide(i, handler.NewVODHandler)
	do.Provide(i, api.New)
}

// wireCopyLookup hands the ingestor a stream-by-code resolver backed by the
// repository. Required for `copy://` input URLs — the copy reader needs to
// look up the upstream stream to find its buffer ID and verify shape.
//
// We use a fresh background context for the lookup because resolution
// happens at packet-read time, long after the original request that created
// the worker has been served. The repo's FindByCode is fast (in-memory or
// indexed), so blocking briefly here is acceptable.
func wireCopyLookup(i do.Injector) {
	ing := do.MustInvoke[*ingestor.Service](i)
	repo := do.MustInvoke[store.StreamRepository](i)
	ing.SetStreamLookup(func(code domain.StreamCode) (*domain.Stream, bool) {
		s, err := repo.FindByCode(context.Background(), code)
		if err != nil {
			return nil, false
		}
		return s, true
	})
}

// hydrateVODRegistry reads the persisted VOD mount table and seeds the
// in-memory registry. Without this, file:// URLs would fail to resolve until
// the operator first edited the mount table at runtime.
func hydrateVODRegistry(ctx context.Context, i do.Injector) error {
	repo := do.MustInvoke[store.VODMountRepository](i)
	registry := do.MustInvoke[*vod.Registry](i)
	mounts, err := repo.List(ctx)
	if err != nil {
		return fmt.Errorf("list vod mounts: %w", err)
	}
	registry.Sync(mounts)
	slog.Info("server: vod registry hydrated", "mounts", len(mounts))
	return nil
}

// loadGlobalConfig reads the GlobalConfig from the store.
// Returns an empty GlobalConfig (all sections nil) when none has been saved yet.
func loadGlobalConfig(repo store.GlobalConfigRepository) (*domain.GlobalConfig, error) {
	gcfg, err := repo.Get(context.Background())
	if err == nil {
		slog.Info("server: loaded global config from store")
		return gcfg, nil
	}
	if errors.Is(err, store.ErrNotFound) {
		slog.Info("server: no global config in store, starting unconfigured")
		return &domain.GlobalConfig{}, nil
	}
	return nil, fmt.Errorf("read stored config: %w", err)
}
