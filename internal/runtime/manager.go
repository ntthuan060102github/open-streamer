// Package runtime manages the lifecycle of all long-running services based on
// the persisted GlobalConfig.  Each service is independently startable and
// stoppable; the Manager diffs old vs new config and applies changes surgically.
package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/ntt0601zcoder/open-streamer/internal/api"
	"github.com/ntt0601zcoder/open-streamer/internal/coordinator"
	"github.com/ntt0601zcoder/open-streamer/internal/domain"
	"github.com/ntt0601zcoder/open-streamer/internal/events"
	"github.com/ntt0601zcoder/open-streamer/internal/hooks"
	"github.com/ntt0601zcoder/open-streamer/internal/ingestor"
	"github.com/ntt0601zcoder/open-streamer/internal/publisher"
	"github.com/ntt0601zcoder/open-streamer/internal/store"
)

// serviceEntry tracks one running service goroutine.
type serviceEntry struct {
	cancel context.CancelFunc
	done   chan struct{} // closed when the goroutine exits
}

// Deps holds all service references needed by the Manager.
// Populated by main.go from the DI injector.
type Deps struct {
	Ingestor     *ingestor.Service
	Publisher    *publisher.Service
	Coordinator  *coordinator.Coordinator
	HooksSvc     *hooks.Service
	APISrv       *api.Server
	Bus          events.Bus
	StreamRepo   store.StreamRepository
	SettingsRepo store.SettingsRepository
}

// Manager owns the lifecycle of all long-running services.
// It reads GlobalConfig from the store and starts/stops services accordingly.
type Manager struct {
	deps Deps

	mu       sync.Mutex
	services map[string]*serviceEntry
	current  *domain.GlobalConfig

	// rootCtx is the top-level application context (cancelled on SIGINT/SIGTERM).
	rootCtx context.Context
}

// New creates a Manager.  Call Bootstrap() to load config and start services.
func New(ctx context.Context, deps Deps) *Manager {
	return &Manager{
		deps:     deps,
		services: make(map[string]*serviceEntry),
		rootCtx:  ctx,
	}
}

// Bootstrap loads GlobalConfig from the store (seeding defaults on first boot),
// then starts all configured services.
func (m *Manager) Bootstrap() error {
	gcfg, err := m.loadOrSeed()
	if err != nil {
		return fmt.Errorf("runtime: bootstrap: %w", err)
	}

	m.mu.Lock()
	m.current = gcfg
	m.mu.Unlock()

	m.applyAll(gcfg)
	return nil
}

// BootstrapWith starts all configured services using the provided GlobalConfig.
// Use this when main.go has already loaded/seeded the config from the store.
func (m *Manager) BootstrapWith(gcfg *domain.GlobalConfig) {
	m.mu.Lock()
	m.current = gcfg
	m.mu.Unlock()

	m.applyAll(gcfg)
}

// CurrentConfig returns a copy of the active GlobalConfig.
func (m *Manager) CurrentConfig() *domain.GlobalConfig {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.current
}

// Apply saves a new GlobalConfig to the store and diffs against the current
// config to start/stop services as needed.
func (m *Manager) Apply(ctx context.Context, newCfg *domain.GlobalConfig) error {
	raw, err := json.Marshal(newCfg)
	if err != nil {
		return fmt.Errorf("runtime: marshal config: %w", err)
	}
	if err := m.deps.SettingsRepo.Set(ctx, "global", raw); err != nil {
		return fmt.Errorf("runtime: save config: %w", err)
	}

	m.mu.Lock()
	old := m.current
	m.current = newCfg
	m.mu.Unlock()

	m.diff(old, newCfg) //nolint:contextcheck // services derive context from rootCtx, not the request ctx
	return nil
}

// WaitAll blocks until all running services have exited.
func (m *Manager) WaitAll() {
	m.mu.Lock()
	entries := make([]*serviceEntry, 0, len(m.services))
	for _, e := range m.services {
		entries = append(entries, e)
	}
	m.mu.Unlock()
	for _, e := range entries {
		<-e.done
	}
}

// --- internal ---

const settingsKey = "global"

func (m *Manager) loadOrSeed() (*domain.GlobalConfig, error) {
	raw, err := m.deps.SettingsRepo.Get(context.Background(), settingsKey)
	if err == nil {
		var gcfg domain.GlobalConfig
		if err := json.Unmarshal(raw, &gcfg); err != nil {
			return nil, fmt.Errorf("unmarshal stored config: %w", err)
		}
		slog.Info("runtime: loaded global config from store")
		return &gcfg, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("read stored config: %w", err)
	}

	// First boot — seed defaults.
	gcfg := domain.DefaultGlobalConfig()
	raw, _ = json.Marshal(gcfg)
	if err := m.deps.SettingsRepo.Set(context.Background(), settingsKey, raw); err != nil {
		slog.Warn("runtime: failed to seed default config", "err", err)
	} else {
		slog.Info("runtime: seeded default global config to store")
	}
	return gcfg, nil
}

// applyAll starts all services that are configured (initial boot).
func (m *Manager) applyAll(cfg *domain.GlobalConfig) {
	if cfg.Server != nil {
		m.startService("http", func(ctx context.Context) error {
			return m.deps.APISrv.StartWithConfig(ctx, cfg.Server)
		})
	}

	if cfg.Ingestor != nil {
		m.startService("ingestor", func(ctx context.Context) error {
			return m.deps.Ingestor.Run(ctx)
		})
	}

	if cfg.Publisher != nil && cfg.Publisher.RTSP.PortMin > 0 {
		m.startService("pub_rtsp", func(ctx context.Context) error {
			return m.deps.Publisher.RunRTSPPlayServer(ctx)
		})
	}
	if cfg.Publisher != nil && cfg.Publisher.RTMP.Port > 0 {
		m.startService("pub_rtmp", func(ctx context.Context) error {
			return m.deps.Publisher.RunRTMPPlayServer(ctx)
		})
	}
	if cfg.Publisher != nil && cfg.Publisher.SRT.Port > 0 {
		m.startService("pub_srt", func(ctx context.Context) error {
			return m.deps.Publisher.RunSRTPlayServer(ctx)
		})
	}

	if cfg.Hooks != nil {
		m.startService("hooks", func(ctx context.Context) error {
			return m.deps.HooksSvc.Start(ctx)
		})
	}

	// Event bus is always running (lightweight).
	events.Start(m.rootCtx, m.deps.Bus)

	// Wire ingestor → publisher RTMP play handler.
	m.deps.Ingestor.SetRTMPPlayHandler(m.deps.Publisher.HandleRTMPPlay)

	// Bootstrap persisted streams.
	coordinator.BootstrapPersistedStreams(
		m.rootCtx, slog.Default(), m.deps.StreamRepo, m.deps.Coordinator,
	)
}

// diff compares old and new configs and starts/stops services as needed.
func (m *Manager) diff(old, new *domain.GlobalConfig) {
	// HTTP server
	m.diffService("http", old.Server != nil, new.Server != nil,
		configChanged(old.Server, new.Server),
		func(ctx context.Context) error {
			return m.deps.APISrv.StartWithConfig(ctx, new.Server)
		})

	// Ingestor (RTMP/SRT push listeners)
	m.diffService("ingestor", old.Ingestor != nil, new.Ingestor != nil,
		configChanged(old.Ingestor, new.Ingestor),
		func(ctx context.Context) error {
			return m.deps.Ingestor.Run(ctx)
		})

	// Publisher play servers
	oldPub := old.Publisher
	newPub := new.Publisher
	oldRTSP := oldPub != nil && oldPub.RTSP.PortMin > 0
	newRTSP := newPub != nil && newPub.RTSP.PortMin > 0
	m.diffService("pub_rtsp", oldRTSP, newRTSP,
		configChanged(ptrIf(oldRTSP, oldPub), ptrIf(newRTSP, newPub)),
		func(ctx context.Context) error {
			return m.deps.Publisher.RunRTSPPlayServer(ctx)
		})

	oldRTMP := oldPub != nil && oldPub.RTMP.Port > 0
	newRTMP := newPub != nil && newPub.RTMP.Port > 0
	m.diffService("pub_rtmp", oldRTMP, newRTMP,
		configChanged(ptrIf(oldRTMP, oldPub), ptrIf(newRTMP, newPub)),
		func(ctx context.Context) error {
			return m.deps.Publisher.RunRTMPPlayServer(ctx)
		})

	oldSRT := oldPub != nil && oldPub.SRT.Port > 0
	newSRT := newPub != nil && newPub.SRT.Port > 0
	m.diffService("pub_srt", oldSRT, newSRT,
		configChanged(ptrIf(oldSRT, oldPub), ptrIf(newSRT, newPub)),
		func(ctx context.Context) error {
			return m.deps.Publisher.RunSRTPlayServer(ctx)
		})

	// Hooks
	m.diffService("hooks", old.Hooks != nil, new.Hooks != nil,
		configChanged(old.Hooks, new.Hooks),
		func(ctx context.Context) error {
			return m.deps.HooksSvc.Start(ctx)
		})
}

// diffService handles the transition for a single service:
//   - was off, now on → start
//   - was on, now off → stop
//   - was on, now on, config changed → restart (stop + start)
//   - unchanged → no-op
func (m *Manager) diffService(
	name string,
	wasOn, nowOn bool,
	changed bool,
	startFn func(context.Context) error,
) {
	switch {
	case !wasOn && nowOn:
		slog.Info("runtime: starting service", "service", name)
		m.startService(name, startFn)
	case wasOn && !nowOn:
		slog.Info("runtime: stopping service", "service", name)
		m.stopService(name)
	case wasOn && nowOn && changed:
		slog.Info("runtime: restarting service (config changed)", "service", name)
		m.stopService(name)
		m.startService(name, startFn)
	}
}

func (m *Manager) startService(name string, fn func(context.Context) error) {
	ctx, cancel := context.WithCancel(m.rootCtx)
	done := make(chan struct{})

	m.mu.Lock()
	// Stop any existing instance first.
	if old, ok := m.services[name]; ok {
		old.cancel()
		m.mu.Unlock()
		<-old.done
		m.mu.Lock()
	}
	m.services[name] = &serviceEntry{cancel: cancel, done: done}
	m.mu.Unlock()

	go func() {
		defer close(done)
		if err := fn(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("runtime: service exited with error", "service", name, "err", err)
		}
	}()
}

func (m *Manager) stopService(name string) {
	m.mu.Lock()
	e, ok := m.services[name]
	if ok {
		delete(m.services, name)
	}
	m.mu.Unlock()
	if ok {
		e.cancel()
		<-e.done
		slog.Info("runtime: service stopped", "service", name)
	}
}

// configChanged compares two config sections by JSON serialization.
// Returns true when the serialized forms differ or one is nil and the other is not.
func configChanged(a, b any) bool {
	if a == nil && b == nil {
		return false
	}
	if a == nil || b == nil {
		return true
	}
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return string(ja) != string(jb)
}

func ptrIf[T any](cond bool, v *T) *T {
	if cond {
		return v
	}
	return nil
}
