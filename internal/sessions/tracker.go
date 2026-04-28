package sessions

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/ntt0601zcoder/open-streamer/config"
	"github.com/ntt0601zcoder/open-streamer/internal/domain"
	"github.com/ntt0601zcoder/open-streamer/internal/events"
)

// Tracker is the public contract that publisher / api layers use to record
// playback sessions. The implementation is in-memory; restart drops state.
//
// All methods are safe for concurrent use.
type Tracker interface {
	// TrackHTTP records activity for an HTTP-based pull protocol (HLS / DASH).
	// Called once per segment / manifest GET. Idempotent on the (stream, fp)
	// pair within the idle window — repeated calls extend UpdatedAt and
	// accumulate Bytes onto an existing session instead of opening a new one.
	//
	// The returned session is a snapshot copy safe to retain by the caller;
	// further mutations happen on the live record held inside the tracker.
	TrackHTTP(ctx context.Context, h HTTPHit) *domain.PlaySession

	// OpenConn opens a session for a connection-bound protocol (RTMP / SRT /
	// RTSP). Returns the session record and a Closer the caller must invoke
	// when the underlying transport ends. The Closer is idempotent.
	OpenConn(ctx context.Context, h ConnHit) (*domain.PlaySession, Closer)

	// List returns a snapshot of every session matching the filter. Active
	// (ClosedAt == nil) and idle entries are included; filter by Status.
	List(filter Filter) []*domain.PlaySession

	// Get returns the session with the given ID, or false when missing.
	Get(id string) (*domain.PlaySession, bool)

	// Kick force-closes an active session and emits a closed event with
	// reason="kicked". Returns false when the id is unknown or already closed.
	Kick(id string) bool

	// Stats returns aggregate counters useful for /metrics or readiness
	// probes. Does not allocate per-session storage.
	Stats() Stats
}

// HTTPHit is the input to TrackHTTP. Build with NewHTTPHit from an
// http.Request so the IP / UA / Referer extraction is consistent.
type HTTPHit struct {
	StreamCode domain.StreamCode
	Protocol   domain.SessionProto // SessionProtoHLS or SessionProtoDASH
	IP         string              // already extracted; no port
	UserAgent  string
	Referer    string
	Query      string // raw r.URL.RawQuery — stored only on session open
	Token      string // ?token=… value if present
	Secure     bool
	BytesDelta int64 // bytes the most recent response wrote to the client
}

// ConnHit is the input to OpenConn for connection-bound protocols.
type ConnHit struct {
	StreamCode domain.StreamCode
	Protocol   domain.SessionProto // SessionProtoRTMP / SessionProtoSRT / SessionProtoRTSP
	RemoteAddr string              // ip:port from net.Conn.RemoteAddr().String()
	UserAgent  string              // RTMP flashVer / RTSP User-Agent / "" for SRT
	Token      string              // optional, e.g. SRT streamid query
	Secure     bool                // true for RTMPS / SRTS / RTSPS
}

// Closer ends a connection-bound session. addBytes is the cumulative bytes
// sent on the underlying transport; pass 0 if the protocol can't measure.
// Both forms are idempotent.
type Closer interface {
	Close(reason domain.SessionCloseReason, addBytes int64)
}

// Filter is the union of selectors accepted by List.
// Zero value matches every session.
type Filter struct {
	StreamCode domain.StreamCode // exact match; "" = any
	Protocol   domain.SessionProto
	Status     FilterStatus
	Limit      int // <=0 = unlimited
}

// FilterStatus narrows List by lifecycle state.
type FilterStatus string

// FilterStatus values.
const (
	FilterStatusAny    FilterStatus = ""       // active + recently closed
	FilterStatusActive FilterStatus = "active" // ClosedAt == nil
	FilterStatusClosed FilterStatus = "closed" // ClosedAt != nil
)

// Stats summarises the live tracker state.
type Stats struct {
	Active          int   `json:"active"`
	OpenedTotal     int64 `json:"opened_total"`
	ClosedTotal     int64 `json:"closed_total"`
	IdleClosedTotal int64 `json:"idle_closed_total"`
	KickedTotal     int64 `json:"kicked_total"`
}

// NewHTTPHit extracts the per-request fields TrackHTTP needs from an
// http.Request. Centralising this avoids per-protocol drift in how IP / UA /
// token are sniffed.
func NewHTTPHit(r *http.Request, code domain.StreamCode, proto domain.SessionProto, bytesDelta int64) HTTPHit {
	q := r.URL.Query()
	token := strings.TrimSpace(q.Get("token"))
	return HTTPHit{
		StreamCode: code,
		Protocol:   proto,
		IP:         clientIP(r),
		UserAgent:  r.Header.Get("User-Agent"),
		Referer:    r.Header.Get("Referer"),
		Query:      r.URL.RawQuery,
		Token:      token,
		Secure:     r.TLS != nil,
		BytesDelta: bytesDelta,
	}
}

// clientIP returns the most likely originating IP for an HTTP request,
// respecting standard proxy headers when present. The chi/middleware.RealIP
// in the API server already rewrites RemoteAddr; this is a defensive double
// extraction that keeps us correct when a different router is used (tests).
func clientIP(r *http.Request) string {
	if xff := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); xff != "" {
		// First entry is the original client; commas separate proxies.
		if i := strings.IndexByte(xff, ','); i > 0 {
			xff = xff[:i]
		}
		xff = strings.TrimSpace(xff)
		if ip := stripPort(xff); ip != "" {
			return ip
		}
	}
	if xri := strings.TrimSpace(r.Header.Get("X-Real-IP")); xri != "" {
		if ip := stripPort(xri); ip != "" {
			return ip
		}
	}
	return stripPort(r.RemoteAddr)
}

func stripPort(addr string) string {
	if addr == "" {
		return ""
	}
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return h
	}
	return addr
}

// connRemoteIP normalises a "ip:port" string from net.Conn.RemoteAddr() to
// just the IP part. Returns the input unchanged if it doesn't parse.
func connRemoteIP(remote string) string { return stripPort(remote) }

// TokenFromQuery sniffs an SRT-style "live/foo?token=…" or RTSP query for a
// `token` field. Returns "" when the query is empty / unparseable / has no
// token. Helper exported for OpenConn callers (publisher's SRT subscribe
// path) to avoid each protocol re-implementing the parse.
func TokenFromQuery(rawQuery string) string {
	if rawQuery == "" {
		return ""
	}
	v, err := url.ParseQuery(rawQuery)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(v.Get("token"))
}

// ─── implementation ──────────────────────────────────────────────────────────

// service is the in-memory Tracker. State is a single map guarded by a
// RWMutex; that's adequate up to ~50 k concurrent sessions, after which the
// hot path becomes the lock. We can shard later without breaking Tracker.
//
// runtimeCfg is held behind an atomic pointer so the runtime config-diff
// path can hot-swap timeouts / enabled state without restarting the
// reaper goroutine or losing the in-memory session map.
type service struct {
	runtimeCfg atomic.Pointer[runtimeConfig]
	bus        events.Bus
	geo        GeoIPResolver
	now        func() time.Time // hookable for tests
	m          *metricsHooks    // pluggable metrics sink; nil-safe for tests

	mu       sync.RWMutex
	sessions map[string]*domain.PlaySession // id → live record

	openedTotal     atomic.Int64
	closedTotal     atomic.Int64
	idleClosedTotal atomic.Int64
	kickedTotal     atomic.Int64
}

// metricsHooks abstracts the Prometheus surface so the tracker file
// doesn't import the metrics package directly (avoiding a heavier coupling
// for what is conceptually an observation hook). The DI wiring in
// service.go fills this in from the injected *metrics.Metrics.
type metricsHooks struct {
	active *prometheus.GaugeVec
	opened *prometheus.CounterVec
	closed *prometheus.CounterVec
}

// runtimeConfig is the resolved subset of config.SessionsConfig the hot
// paths actually read. Stored behind an atomic pointer so updates publish
// atomically without a mutex.
type runtimeConfig struct {
	enabled  bool
	idleDur  time.Duration
	maxAlive time.Duration
}

// New constructs the Tracker. Reads config.SessionsConfig and the GeoIPResolver
// from the injector. Caller is expected to call Run(ctx) to start the reaper.
func newService(cfg config.SessionsConfig, bus events.Bus, geo GeoIPResolver) *service {
	if geo == nil {
		geo = NullGeoIP{}
	}
	s := &service{
		bus:      bus,
		geo:      geo,
		now:      time.Now,
		sessions: make(map[string]*domain.PlaySession, 256),
	}
	s.applyConfig(cfg)
	return s
}

// applyConfig resolves the user-facing SessionsConfig into the runtime form
// and atomically swaps it into place. Safe to call concurrently with
// TrackHTTP / OpenConn / reapOnce — they read the pointer fresh each time.
func (s *service) applyConfig(cfg config.SessionsConfig) {
	idle := time.Duration(cfg.IdleTimeoutSec) * time.Second
	if idle <= 0 {
		idle = 30 * time.Second
	}
	maxAlive := time.Duration(cfg.MaxLifetimeSec) * time.Second
	if maxAlive < 0 {
		maxAlive = 0
	}
	s.runtimeCfg.Store(&runtimeConfig{
		enabled:  cfg.Enabled,
		idleDur:  idle,
		maxAlive: maxAlive,
	})
}

// loadCfg returns the current runtime config. Always non-nil after construction.
func (s *service) loadCfg() *runtimeConfig { return s.runtimeCfg.Load() }

// TrackHTTP implements Tracker.
func (s *service) TrackHTTP(ctx context.Context, h HTTPHit) *domain.PlaySession {
	if !s.loadCfg().enabled {
		return nil
	}
	id := fingerprintID(h.StreamCode, h.IP, h.UserAgent, h.Token)

	s.mu.Lock()
	sess, exists := s.sessions[id]
	now := s.now()
	if !exists {
		sess = s.openHTTP(id, h, now)
		s.sessions[id] = sess
		s.mu.Unlock()
		s.observeOpened(sess)
		s.publishOpened(ctx, sess)
		snap := *sess
		return &snap
	}
	if h.BytesDelta > 0 {
		sess.Bytes += h.BytesDelta
		if sess.StartedAt.IsZero() {
			sess.StartedAt = now
		}
	}
	sess.UpdatedAt = now
	snap := *sess
	s.mu.Unlock()
	return &snap
}

// openHTTP fills in a fresh PlaySession for a first-seen HTTP fingerprint.
// Must be called with s.mu held in write mode.
func (s *service) openHTTP(id string, h HTTPHit, now time.Time) *domain.PlaySession {
	named := domain.SessionNamedByFingerprint
	user := shortFingerprintLabel(id)
	if h.Token != "" {
		named = domain.SessionNamedByToken
		user = h.Token
	}
	started := time.Time{}
	bytes := h.BytesDelta
	if bytes > 0 {
		started = now
	}
	sess := &domain.PlaySession{
		ID:          id,
		StreamCode:  h.StreamCode,
		Protocol:    h.Protocol,
		IP:          h.IP,
		UserAgent:   h.UserAgent,
		Referer:     h.Referer,
		QueryString: h.Query,
		Token:       h.Token,
		UserName:    user,
		NamedBy:     named,
		Country:     s.geo.Country(net.ParseIP(h.IP)),
		Bytes:       bytes,
		Secure:      h.Secure,
		OpenedAt:    now,
		StartedAt:   started,
		UpdatedAt:   now,
	}
	s.openedTotal.Add(1)
	return sess
}

// OpenConn implements Tracker.
func (s *service) OpenConn(ctx context.Context, h ConnHit) (*domain.PlaySession, Closer) {
	if !s.loadCfg().enabled {
		return nil, noopCloser{}
	}
	id := uuid.NewString()
	now := s.now()
	ip := connRemoteIP(h.RemoteAddr)
	named := domain.SessionNamedByFingerprint
	user := shortFingerprintLabel(strings.ReplaceAll(id, "-", ""))
	if h.Token != "" {
		named = domain.SessionNamedByToken
		user = h.Token
	}
	sess := &domain.PlaySession{
		ID:         id,
		StreamCode: h.StreamCode,
		Protocol:   h.Protocol,
		IP:         ip,
		UserAgent:  h.UserAgent,
		Token:      h.Token,
		UserName:   user,
		NamedBy:    named,
		Country:    s.geo.Country(net.ParseIP(ip)),
		Secure:     h.Secure,
		OpenedAt:   now,
		StartedAt:  now,
		UpdatedAt:  now,
	}
	s.mu.Lock()
	s.sessions[id] = sess
	s.mu.Unlock()
	s.openedTotal.Add(1)
	s.observeOpened(sess)
	s.publishOpened(ctx, sess)
	snap := *sess
	return &snap, &connCloser{svc: s, id: id}
}

// closeByID is the standard close path used by Tracker.Kick / Closer.Close /
// shutdown sweeps. Publishes EventSessionClosed with a Background context
// so callers that hold a request-scoped ctx don't lose the analytics event
// when their ctx is cancelled mid-close.
func (s *service) closeByID(id string, reason domain.SessionCloseReason, addBytes int64) {
	s.closeByIDCtx(context.Background(), id, reason, addBytes)
}

// closeByIDCtx is the ctx-aware close path used by the reaper, which has a
// live ctx it wants to forward to event subscribers. Idempotent: a no-op
// when the id is missing or already closed.
func (s *service) closeByIDCtx(ctx context.Context, id string, reason domain.SessionCloseReason, addBytes int64) {
	now := s.now()
	s.mu.Lock()
	sess, ok := s.sessions[id]
	if !ok {
		s.mu.Unlock()
		return
	}
	if sess.ClosedAt != nil {
		s.mu.Unlock()
		return
	}
	if addBytes > 0 {
		sess.Bytes += addBytes
	}
	t := now
	sess.ClosedAt = &t
	sess.CloseReason = reason
	sess.UpdatedAt = now
	delete(s.sessions, id)
	snap := *sess
	s.mu.Unlock()

	s.closedTotal.Add(1)
	switch reason {
	case domain.SessionCloseIdle:
		s.idleClosedTotal.Add(1)
	case domain.SessionCloseKicked:
		s.kickedTotal.Add(1)
	case domain.SessionCloseClient, domain.SessionCloseShutdown:
		// Counted only via closedTotal; no per-reason counter today.
	}
	s.observeClosed(&snap, reason)
	s.publishClosed(ctx, &snap)
}

// List implements Tracker.
func (s *service) List(filter Filter) []*domain.PlaySession {
	s.mu.RLock()
	out := make([]*domain.PlaySession, 0, len(s.sessions))
	for _, sess := range s.sessions {
		if !filter.matches(sess) {
			continue
		}
		c := *sess
		out = append(out, &c)
		if filter.Limit > 0 && len(out) >= filter.Limit {
			break
		}
	}
	s.mu.RUnlock()
	return out
}

// Get implements Tracker.
func (s *service) Get(id string) (*domain.PlaySession, bool) {
	s.mu.RLock()
	sess, ok := s.sessions[id]
	if !ok {
		s.mu.RUnlock()
		return nil, false
	}
	c := *sess
	s.mu.RUnlock()
	return &c, true
}

// Kick implements Tracker.
func (s *service) Kick(id string) bool {
	s.mu.RLock()
	_, ok := s.sessions[id]
	s.mu.RUnlock()
	if !ok {
		return false
	}
	s.closeByID(id, domain.SessionCloseKicked, 0)
	return true
}

// Stats implements Tracker.
func (s *service) Stats() Stats {
	s.mu.RLock()
	active := len(s.sessions)
	s.mu.RUnlock()
	return Stats{
		Active:          active,
		OpenedTotal:     s.openedTotal.Load(),
		ClosedTotal:     s.closedTotal.Load(),
		IdleClosedTotal: s.idleClosedTotal.Load(),
		KickedTotal:     s.kickedTotal.Load(),
	}
}

// matches reports whether sess passes the filter.
func (f Filter) matches(sess *domain.PlaySession) bool {
	if f.StreamCode != "" && sess.StreamCode != f.StreamCode {
		return false
	}
	if f.Protocol != "" && sess.Protocol != f.Protocol {
		return false
	}
	switch f.Status {
	case FilterStatusActive:
		if sess.ClosedAt != nil {
			return false
		}
	case FilterStatusClosed:
		if sess.ClosedAt == nil {
			return false
		}
	case FilterStatusAny:
		// no narrowing — fall through and match
	}
	return true
}

// ─── metrics helpers ─────────────────────────────────────────────────────────

// observeOpened bumps SessionsActive and SessionsOpenedTotal for the labels
// derived from sess. Nil-safe so tests that don't wire metrics keep working.
func (s *service) observeOpened(sess *domain.PlaySession) {
	if s.m == nil {
		return
	}
	labels := prometheus.Labels{
		"stream_code": string(sess.StreamCode),
		"proto":       string(sess.Protocol),
	}
	s.m.active.With(labels).Inc()
	s.m.opened.With(labels).Inc()
}

// observeClosed mirrors observeOpened: decrements the active gauge and
// increments the closed counter with the reason label.
func (s *service) observeClosed(sess *domain.PlaySession, reason domain.SessionCloseReason) {
	if s.m == nil {
		return
	}
	streamLabels := prometheus.Labels{
		"stream_code": string(sess.StreamCode),
		"proto":       string(sess.Protocol),
	}
	s.m.active.With(streamLabels).Dec()
	s.m.closed.With(prometheus.Labels{
		"stream_code": string(sess.StreamCode),
		"proto":       string(sess.Protocol),
		"reason":      string(reason),
	}).Inc()
}

// ─── publishing helpers ──────────────────────────────────────────────────────

func (s *service) publishOpened(ctx context.Context, sess *domain.PlaySession) {
	if s.bus == nil {
		return
	}
	s.bus.Publish(ctx, domain.Event{
		ID:         uuid.NewString(),
		Type:       domain.EventSessionOpened,
		StreamCode: sess.StreamCode,
		OccurredAt: sess.OpenedAt,
		Payload: map[string]any{
			"session_id": sess.ID,
			"proto":      string(sess.Protocol),
			"ip":         sess.IP,
			"user_agent": sess.UserAgent,
			"country":    sess.Country,
			"user_name":  sess.UserName,
		},
	})
}

func (s *service) publishClosed(ctx context.Context, sess *domain.PlaySession) {
	if s.bus == nil {
		return
	}
	closedAt := time.Time{}
	if sess.ClosedAt != nil {
		closedAt = *sess.ClosedAt
	}
	s.bus.Publish(ctx, domain.Event{
		ID:         uuid.NewString(),
		Type:       domain.EventSessionClosed,
		StreamCode: sess.StreamCode,
		OccurredAt: closedAt,
		Payload: map[string]any{
			"session_id":   sess.ID,
			"proto":        string(sess.Protocol),
			"ip":           sess.IP,
			"bytes":        sess.Bytes,
			"duration_sec": int64(sess.Duration() / time.Second),
			"reason":       string(sess.CloseReason),
		},
	})
}

// ─── Closer impls ────────────────────────────────────────────────────────────

type connCloser struct {
	svc  *service
	id   string
	once sync.Once
}

// Close runs the underlying service close path exactly once, no matter how
// many times the protocol layer invokes it (e.g. both deferred cleanup and
// the explicit watchdog path).
func (c *connCloser) Close(reason domain.SessionCloseReason, addBytes int64) {
	c.once.Do(func() { c.svc.closeByID(c.id, reason, addBytes) })
}

// noopCloser is the Closer returned when sessions tracking is disabled. It
// satisfies the protocol-side defer pattern without touching tracker state.
type noopCloser struct{}

// Close is the disabled-tracker no-op — discards both reason and bytes.
func (noopCloser) Close(domain.SessionCloseReason, int64) {}
