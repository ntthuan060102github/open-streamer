// Package sessions tracks live playback sessions across every protocol
// Open-Streamer serves (HLS, DASH, RTMP, SRT, RTSP) so operators can answer
// "who is watching <stream> right now?".
//
// Design rules:
//   - State is in-memory only. Restart = lost; viewers reconnect → new sessions.
//   - HTTP-based sessions (HLS/DASH) are keyed by a deterministic fingerprint
//     so consecutive segment GETs from one viewer collapse onto a single record.
//   - Connection-bound sessions (RTMP/SRT/RTSP) are keyed by a fresh UUID; the
//     transport layer signals close via the Closer returned from OpenConn.
//   - All open / close events are published on the event bus so analytics or
//     persistence can be added without changing this package.
package sessions

import (
	"context"

	"github.com/samber/do/v2"

	"github.com/ntt0601zcoder/open-streamer/config"
	"github.com/ntt0601zcoder/open-streamer/internal/events"
	"github.com/ntt0601zcoder/open-streamer/internal/metrics"
)

// Service is the public DI handle. It implements Tracker.
type Service struct {
	*service
}

// New constructs the Service for samber/do. GeoIPResolver is optional in DI —
// if no provider is registered, NullGeoIP is used. Metrics is also optional
// so unit tests that wire only sessions can construct the service.
func New(i do.Injector) (*Service, error) {
	cfg := do.MustInvoke[config.SessionsConfig](i)
	bus := do.MustInvoke[events.Bus](i)

	geo, err := do.Invoke[GeoIPResolver](i)
	if err != nil {
		geo = NullGeoIP{}
	}

	svc := newService(cfg, bus, geo)
	if m, err := do.Invoke[*metrics.Metrics](i); err == nil {
		svc.m = &metricsHooks{
			active: m.SessionsActive,
			opened: m.SessionsOpenedTotal,
			closed: m.SessionsClosedTotal,
		}
	}
	return &Service{service: svc}, nil
}

// Run starts the idle reaper. Always runs — the reaper itself checks the
// hot-reloadable enabled flag each tick and skips reaping when disabled.
// This way an operator toggling Enabled at runtime via /config takes effect
// without restarting the goroutine. Blocks until ctx is cancelled, then
// closes every still-active session with reason=shutdown so subscribers
// get a final event.
func (s *Service) Run(ctx context.Context) {
	s.runReaper(ctx)
}

// UpdateConfig hot-swaps the runtime config. Called by runtime.Manager.diff
// when the persisted SessionsConfig section changes — no restart needed.
// In-flight sessions keep their state; new idle/max-lifetime windows take
// effect on the next reaper tick.
func (s *Service) UpdateConfig(cfg config.SessionsConfig) {
	s.applyConfig(cfg)
}

// NewServiceForTesting builds a Service without going through DI — for unit
// tests that need a real Tracker but don't want a do.Injector. Bus and
// resolver may be nil.
func NewServiceForTesting(cfg config.SessionsConfig, bus events.Bus, geo GeoIPResolver) *Service {
	if geo == nil {
		geo = NullGeoIP{}
	}
	return &Service{service: newService(cfg, bus, geo)}
}
