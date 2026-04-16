// Package main is the entrypoint for the Open Streamer server.
// It loads StorageConfig from file/env, loads GlobalConfig from the store,
// wires all dependencies via samber/do, and starts services via RuntimeManager.
// Graceful shutdown is triggered on SIGINT or SIGTERM.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
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
	"github.com/ntt0601zcoder/open-streamer/internal/ingestor"
	"github.com/ntt0601zcoder/open-streamer/internal/manager"
	"github.com/ntt0601zcoder/open-streamer/internal/metrics"
	"github.com/ntt0601zcoder/open-streamer/internal/publisher"
	"github.com/ntt0601zcoder/open-streamer/internal/runtime"
	"github.com/ntt0601zcoder/open-streamer/internal/store"
	jsonstore "github.com/ntt0601zcoder/open-streamer/internal/store/json"
	yamlstore "github.com/ntt0601zcoder/open-streamer/internal/store/yaml"
	"github.com/ntt0601zcoder/open-streamer/internal/transcoder"
	"github.com/ntt0601zcoder/open-streamer/pkg/logger"
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
	settingsRepo := do.MustInvoke[store.SettingsRepository](injector)
	gcfg, err := loadOrSeedGlobalConfig(settingsRepo)
	if err != nil {
		return fmt.Errorf("load global config: %w", err)
	}

	// 4. Provide individual sub-configs to DI so services can read them at construction time.
	provideSubConfigs(injector, gcfg)

	// Init logger from store config.
	if gcfg.Log != nil {
		slog.SetDefault(logger.New(*gcfg.Log))
	}

	// 5. Wire all services.
	wireServices(injector)

	// 6. Assemble RuntimeManager deps from DI.
	rtm := runtime.New(ctx, runtime.Deps{
		Ingestor:     do.MustInvoke[*ingestor.Service](injector),
		Publisher:    do.MustInvoke[*publisher.Service](injector),
		Coordinator:  do.MustInvoke[*coordinator.Coordinator](injector),
		HooksSvc:     do.MustInvoke[*hooks.Service](injector),
		APISrv:       do.MustInvoke[*api.Server](injector),
		Bus:          do.MustInvoke[events.Bus](injector),
		StreamRepo:   do.MustInvoke[store.StreamRepository](injector),
		SettingsRepo: settingsRepo,
	})

	// 7. Inject RuntimeManager into ConfigHandler (breaks circular DI dependency).
	configH := do.MustInvoke[*handler.ConfigHandler](injector)
	configH.SetRuntimeManager(rtm)

	// 8. Start all configured services.
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
		do.ProvideValue(i, s.Settings())

	default: // "yaml" or empty
		s, err := yamlstore.New(cfg.YAMLDir)
		if err != nil {
			return fmt.Errorf("yaml store: %w", err)
		}
		do.ProvideValue(i, s.Streams())
		do.ProvideValue(i, s.Recordings())
		do.ProvideValue(i, s.Hooks())
		do.ProvideValue(i, s.Settings())
	}

	slog.Info("server: storage backend ready", "driver", cfg.Driver)
	return nil
}

// provideSubConfigs extracts individual sub-configs from the GlobalConfig and
// registers them in the DI injector so each service only sees its own config type.
func provideSubConfigs(i *do.RootScope, gcfg *domain.GlobalConfig) {
	if gcfg.Server != nil {
		do.ProvideValue(i, *gcfg.Server)
	}
	if gcfg.Ingestor != nil {
		do.ProvideValue(i, *gcfg.Ingestor)
	}
	if gcfg.Buffer != nil {
		do.ProvideValue(i, *gcfg.Buffer)
	}
	if gcfg.Transcoder != nil {
		do.ProvideValue(i, *gcfg.Transcoder)
	}
	if gcfg.Publisher != nil {
		do.ProvideValue(i, *gcfg.Publisher)
	}
	if gcfg.Manager != nil {
		do.ProvideValue(i, *gcfg.Manager)
	}
	if gcfg.Hooks != nil {
		do.ProvideValue(i, *gcfg.Hooks)
	}
	if gcfg.Metrics != nil {
		do.ProvideValue(i, *gcfg.Metrics)
	}
	if gcfg.Log != nil {
		do.ProvideValue(i, *gcfg.Log)
	}
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
	do.Provide(i, api.New)
}

// loadOrSeedGlobalConfig reads the GlobalConfig from the settings store.
// On first boot (key not found), it seeds the store with DefaultGlobalConfig.
func loadOrSeedGlobalConfig(repo store.SettingsRepository) (*domain.GlobalConfig, error) {
	raw, err := repo.Get(context.Background(), "global")
	if err == nil {
		var gcfg domain.GlobalConfig
		if err := json.Unmarshal(raw, &gcfg); err != nil {
			return nil, fmt.Errorf("unmarshal stored config: %w", err)
		}
		slog.Info("server: loaded global config from store")
		return &gcfg, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("read stored config: %w", err)
	}

	// First boot — seed defaults.
	gcfg := domain.DefaultGlobalConfig()
	raw, _ = json.Marshal(gcfg)
	if err := repo.Set(context.Background(), "global", raw); err != nil {
		slog.Warn("server: failed to seed default config", "err", err)
	} else {
		slog.Info("server: seeded default global config to store")
	}
	return gcfg, nil
}
