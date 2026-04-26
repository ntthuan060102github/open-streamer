// Package transcoder manages a bounded pool of FFmpeg worker processes.
// Each stream may run one FFmpeg process per ladder rung (passthrough copy or ABR encode);
// all read the same raw ingest.
// GPU acceleration (NVENC) is used when configured on profiles / global HW.
package transcoder

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ntt0601zcoder/open-streamer/config"
	"github.com/ntt0601zcoder/open-streamer/internal/buffer"
	"github.com/ntt0601zcoder/open-streamer/internal/domain"
	"github.com/ntt0601zcoder/open-streamer/internal/events"
	"github.com/ntt0601zcoder/open-streamer/internal/metrics"
	"github.com/samber/do/v2"
)

// Profile defines a single transcoding output rendition.
// Rendition label in logs and URLs is track_<n> from ladder order (see buffer.VideoTrackSlug).
type Profile struct {
	Width            int
	Height           int
	Bitrate          string // e.g. "4000k"
	Codec            string // e.g. "h264_nvenc", "libx264"
	Preset           string // e.g. "p5" (NVENC), "fast" (libx264)
	CodecProfile     string // H.264/HEVC profile (baseline, main, high)
	CodecLevel       string // e.g. 4.1
	MaxBitrate       int    // kbps peak (0 = omit -maxrate)
	Framerate        float64
	KeyframeInterval int    // GOP target in seconds (0 = encoder default)
	Bframes          *int   // nil = encoder default; 0 = explicit none
	Refs             *int   // nil = encoder default
	SAR              string // "" = inherit; "N:M" sets output sample aspect ratio
	ResizeMode       string // "" = pad; "pad"|"crop"|"stretch"|"fit"
}

// profileWorker tracks a single FFmpeg encoder process (one ABR ladder rung).
// restartCount and errors are mutated under streamWorker.mu and read by RuntimeStatus.
// Both reset on Stop() (the whole streamWorker is dropped).
type profileWorker struct {
	cancel       context.CancelFunc
	done         chan struct{} // closed when the goroutine exits
	restartCount int
	errors       []domain.ErrorEntry // newest at index 0; capped at maxProfileErrorHistory
}

const maxProfileErrorHistory = 5

// recordProfileErrorEntry prepends an entry, capped at maxProfileErrorHistory.
// Caller must hold the parent streamWorker.mu.
func recordProfileErrorEntry(pw *profileWorker, msg string, at time.Time) {
	e := domain.ErrorEntry{Message: msg, At: at}
	if len(pw.errors) >= maxProfileErrorHistory {
		copy(pw.errors[1:], pw.errors[:maxProfileErrorHistory-1])
		pw.errors[0] = e
		return
	}
	pw.errors = append([]domain.ErrorEntry{e}, pw.errors...)
}

// ProfileSnapshot is a serialisable copy of one profile encoder's runtime state.
// Errors is a bounded rolling history (newest first) of FFmpeg crash messages
// captured for this profile since the stream started.
type ProfileSnapshot struct {
	Index        int                 `json:"index"` // 0-based ladder index; track label = track_<index+1>
	Track        string              `json:"track"`
	RestartCount int                 `json:"restart_count"`
	Errors       []domain.ErrorEntry `json:"errors,omitempty"`
}

// RuntimeStatus is a JSON-safe snapshot of transcoder state for one stream.
type RuntimeStatus struct {
	Profiles []ProfileSnapshot `json:"profiles"`
}

// RuntimeStatus returns a snapshot of per-profile encoder state.
// Returns ok=false if the stream has no transcoder pipeline running.
func (s *Service) RuntimeStatus(streamID domain.StreamCode) (RuntimeStatus, bool) {
	s.mu.Lock()
	sw, ok := s.workers[streamID]
	s.mu.Unlock()
	if !ok {
		return RuntimeStatus{}, false
	}

	sw.mu.Lock()
	defer sw.mu.Unlock()

	out := RuntimeStatus{Profiles: make([]ProfileSnapshot, 0, len(sw.profiles))}
	for idx, pw := range sw.profiles {
		snap := ProfileSnapshot{
			Index:        idx,
			Track:        buffer.VideoTrackSlug(idx),
			RestartCount: pw.restartCount,
		}
		if len(pw.errors) > 0 {
			snap.Errors = make([]domain.ErrorEntry, len(pw.errors))
			copy(snap.Errors, pw.errors)
		}
		out.Profiles = append(out.Profiles, snap)
	}
	sort.Slice(out.Profiles, func(i, j int) bool { return out.Profiles[i].Index < out.Profiles[j].Index })
	return out, true
}

// recordProfileError appends a crash entry to the profile's history and bumps
// restartCount. Safe to call from runProfileEncoder's retry loop. No-op if the
// stream worker has been stopped (sw removed from s.workers) or the profile
// has been stopped individually (pw removed from sw.profiles).
func (s *Service) recordProfileError(streamID domain.StreamCode, profileIndex int, msg string) {
	s.mu.Lock()
	sw, ok := s.workers[streamID]
	s.mu.Unlock()
	if !ok {
		return
	}
	sw.mu.Lock()
	defer sw.mu.Unlock()
	pw, ok := sw.profiles[profileIndex]
	if !ok {
		return
	}
	pw.restartCount++
	recordProfileErrorEntry(pw, msg, time.Now())
}

// streamWorker holds all profile encoders for one stream.
type streamWorker struct {
	baseCtx    context.Context
	baseCancel context.CancelFunc // cancels all profiles at once (Stop)
	rawIngest  domain.StreamCode
	tc         *domain.TranscoderConfig
	mu         sync.Mutex
	profiles   map[int]*profileWorker // key = profile index (0-based)
}

// Service manages the FFmpeg worker pool. There is no upper bound on the
// number of concurrent encoders — every rendition gets its own FFmpeg
// process and the OS (rlimit / GPU NVENC slots / RAM) is the natural
// limit. The previous app-level semaphore was removed because it caused
// silent profile starvation when set too low (e.g. 4-slot cap with 20
// streams × 2 profile = 40 needed → 36 stuck waiting forever).
type Service struct {
	cfg     config.TranscoderConfig
	buf     *buffer.Service
	bus     events.Bus
	m       *metrics.Metrics
	mu      sync.Mutex
	workers map[domain.StreamCode]*streamWorker

	// Health callbacks fired when ANY profile of a stream transitions
	// between "crashing in a loop" and "running stably". Coordinator
	// uses these to flip stream status between Active and Degraded.
	// Fired strictly on transition (not on every crash) so the
	// coordinator only sees state changes.
	//
	// Both callbacks may be nil — Service operates fine without them.
	onUnhealthy func(streamID domain.StreamCode, reason string)
	onHealthy   func(streamID domain.StreamCode)

	// unhealthyProfiles tracks which (stream, profile) pairs are
	// currently in a crash loop. A stream is "unhealthy" iff its set
	// is non-empty. Per-profile granularity is needed because legacy
	// mode runs N independent FFmpeg processes — one profile failing
	// doesn't mean all are failing, and we shouldn't fire onHealthy
	// until EVERY failing profile has recovered.
	healthMu          sync.Mutex
	unhealthyProfiles map[domain.StreamCode]map[int]struct{}
}

// New creates a Service and registers it with the DI injector.
func New(i do.Injector) (*Service, error) {
	cfg := do.MustInvoke[config.TranscoderConfig](i)
	buf := do.MustInvoke[*buffer.Service](i)
	bus := do.MustInvoke[events.Bus](i)
	m := do.MustInvoke[*metrics.Metrics](i)

	return &Service{
		cfg:               cfg,
		buf:               buf,
		bus:               bus,
		m:                 m,
		workers:           make(map[domain.StreamCode]*streamWorker),
		unhealthyProfiles: make(map[domain.StreamCode]map[int]struct{}),
	}, nil
}

// SetUnhealthyCallback registers a function the Service calls the
// FIRST time a stream transitions to "transcoder unhealthy" (any
// profile has crashed enough consecutive times that it is in a hot
// retry loop). reason is the latest crash error string.
//
// Subsequent crashes on already-unhealthy streams do NOT re-fire — the
// coordinator only needs to see edges. Pair with SetHealthyCallback to
// be notified when the stream recovers.
func (s *Service) SetUnhealthyCallback(fn func(streamID domain.StreamCode, reason string)) {
	s.mu.Lock()
	s.onUnhealthy = fn
	s.mu.Unlock()
}

// SetHealthyCallback registers a function the Service calls the FIRST
// time every previously-failing profile in a stream has run stably for
// the sustain threshold. Pair with SetUnhealthyCallback for the
// degraded → active edge.
func (s *Service) SetHealthyCallback(fn func(streamID domain.StreamCode)) {
	s.mu.Lock()
	s.onHealthy = fn
	s.mu.Unlock()
}

// markProfileUnhealthy adds (streamID, profileIndex) to the unhealthy
// set. Returns true when the stream JUST transitioned from healthy
// (set was empty) — caller fires onUnhealthy in that case so the
// coordinator only sees the edge, not every crash.
func (s *Service) markProfileUnhealthy(streamID domain.StreamCode, profileIndex int) bool {
	s.healthMu.Lock()
	defer s.healthMu.Unlock()
	set, ok := s.unhealthyProfiles[streamID]
	if !ok {
		set = make(map[int]struct{})
		s.unhealthyProfiles[streamID] = set
	}
	wasEmpty := len(set) == 0
	if _, already := set[profileIndex]; already {
		return false
	}
	set[profileIndex] = struct{}{}
	return wasEmpty
}

// markProfileHealthy removes (streamID, profileIndex) from the
// unhealthy set. Returns true when the stream JUST transitioned to
// fully healthy (set became empty) — caller fires onHealthy on that
// edge.
func (s *Service) markProfileHealthy(streamID domain.StreamCode, profileIndex int) bool {
	s.healthMu.Lock()
	defer s.healthMu.Unlock()
	set, ok := s.unhealthyProfiles[streamID]
	if !ok {
		return false
	}
	if _, present := set[profileIndex]; !present {
		return false
	}
	delete(set, profileIndex)
	if len(set) == 0 {
		delete(s.unhealthyProfiles, streamID)
		return true
	}
	return false
}

// fireUnhealthyIfTransitioned consults the callback under the service
// lock and invokes it outside the lock to avoid holding state.mu /
// service.mu across third-party code (coordinator handler may call
// back into us via setStatus).
func (s *Service) fireUnhealthyIfTransitioned(streamID domain.StreamCode, profileIndex int, reason string) {
	if !s.markProfileUnhealthy(streamID, profileIndex) {
		return
	}
	s.mu.Lock()
	cb := s.onUnhealthy
	s.mu.Unlock()
	if cb != nil {
		cb(streamID, reason)
	}
}

// fireHealthyIfTransitioned mirrors fireUnhealthyIfTransitioned for
// the recovery edge.
func (s *Service) fireHealthyIfTransitioned(streamID domain.StreamCode, profileIndex int) {
	if !s.markProfileHealthy(streamID, profileIndex) {
		return
	}
	s.mu.Lock()
	cb := s.onHealthy
	s.mu.Unlock()
	if cb != nil {
		cb(streamID)
	}
}

// dropHealthState clears every profile entry for a stream — used by
// Stop so a fresh Start with the same code starts from a healthy
// baseline (the previous run's unhealthy markers must not leak across
// pipeline restarts).
//
// Fires onHealthy if the stream had unhealthy entries at drop time so
// the coordinator's mirrored degradation flag also clears. Without
// this, hot-restart paths (Update → Stop → Start to swap config) leave
// the coordinator stuck reporting StatusDegraded — the new transcoder
// process starts clean and never has anything to "recover" from, so
// it would never fire onHealthy on its own.
//
// Safe to fire on full teardown too: caller checks IsRunning before
// returning StreamStatus, so degradation flags become irrelevant once
// the pipeline is gone (StatusStopped wins).
func (s *Service) dropHealthState(streamID domain.StreamCode) {
	s.healthMu.Lock()
	_, hadEntries := s.unhealthyProfiles[streamID]
	delete(s.unhealthyProfiles, streamID)
	s.healthMu.Unlock()
	if !hadEntries {
		return
	}
	s.mu.Lock()
	cb := s.onHealthy
	s.mu.Unlock()
	if cb != nil {
		cb(streamID)
	}
}

// SetConfig hot-swaps the cached transcoder config. Used by runtime.Manager
// when the operator updates GlobalConfig.Transcoder via POST /config so the
// next Start uses the new value.
//
// Already-running streams are NOT restarted from here — caller must
// stop+start them separately to materialize behaviour-changing fields like
// MultiOutput. Holding s.mu prevents a Start in flight from observing a
// torn (half-old, half-new) config.
func (s *Service) SetConfig(cfg config.TranscoderConfig) {
	s.mu.Lock()
	s.cfg = cfg
	s.mu.Unlock()
}

// Config returns a snapshot of the currently active transcoder config.
// Used by runtime.diff to compare old vs new without racing SetConfig.
func (s *Service) Config() config.TranscoderConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg
}

// Start launches the transcoder pipeline for a stream. By default it spawns
// one FFmpeg per RenditionTarget (legacy mode). When config.MultiOutput is
// true, spawns ONE FFmpeg per stream that emits all renditions via separate
// output pipes — see multi_output_run.go for rationale and trade-offs.
func (s *Service) Start(
	ctx context.Context,
	logStreamCode domain.StreamCode,
	rawIngestID domain.StreamCode,
	tc *domain.TranscoderConfig,
	targets []RenditionTarget,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.workers[logStreamCode]; ok {
		return fmt.Errorf("transcoder: stream %s already running", logStreamCode)
	}

	if len(targets) == 0 {
		return fmt.Errorf("transcoder: no rendition targets")
	}

	baseCtx, baseCancel := context.WithCancel(ctx)
	sw := &streamWorker{
		baseCtx:    baseCtx,
		baseCancel: baseCancel,
		rawIngest:  rawIngestID,
		tc:         tc,
		profiles:   make(map[int]*profileWorker, len(targets)),
	}
	s.workers[logStreamCode] = sw

	mode := "per_profile"
	if s.cfg.MultiOutput {
		mode = "multi_output"
	}

	slog.Info("transcoder: stream job started",
		"stream_code", logStreamCode,
		"profiles", len(targets),
		"read_from", rawIngestID,
		"mode", mode,
	)

	s.m.TranscoderWorkersActive.WithLabelValues(string(logStreamCode)).Set(float64(len(targets)))
	s.m.TranscoderQualitiesActive.WithLabelValues(string(logStreamCode)).Set(float64(len(targets)))
	s.bus.Publish(ctx, domain.Event{
		Type:       domain.EventTranscoderStarted,
		StreamCode: logStreamCode,
		Payload: map[string]any{
			"profiles":      len(targets),
			"raw_ingest_id": string(rawIngestID),
			"mode":          mode,
		},
	})

	if s.cfg.MultiOutput {
		// One goroutine, one FFmpeg, all profiles. Track as a synthetic
		// "profile 0" entry so Stop / status APIs see a non-empty
		// profiles map and behave consistently.
		//nolint:contextcheck // spawnMultiOutput derives its context from sw.baseCtx; by design
		s.spawnMultiOutput(logStreamCode, sw, targets)
		return nil
	}

	for i, t := range targets {
		//nolint:contextcheck // spawnProfile derives its context from sw.baseCtx; by design
		s.spawnProfile(logStreamCode, sw, i, t)
	}
	return nil
}

// spawnMultiOutput launches the single multi-output encoder goroutine. The
// real worker is tracked under profile index 0; one "shadow" profileWorker
// is registered per remaining ladder rung so the existing Stop / Update /
// RuntimeStatus / recordProfileError code paths (which iterate over
// profiles) see the same N-entry shape as legacy per-profile mode.
//
// The shadows share an already-closed `done` channel and a no-op cancel —
// accidental StopProfile on a shadow returns immediately without tearing
// down the single underlying FFmpeg process. (StartProfile/StopProfile
// granularity is intentionally lost in multi-output mode — caller must
// fall back to a full Stop+Start to add/remove profiles.)
//
// When FFmpeg crashes, runStreamEncoder calls recordProfileError once per
// target index → every rung in the ladder shows the same crash entry +
// restart counter, accurately conveying "all rungs went down together".
func (s *Service) spawnMultiOutput(streamID domain.StreamCode, sw *streamWorker, targets []RenditionTarget) {
	pCtx, pCancel := context.WithCancel(sw.baseCtx)
	real := &profileWorker{
		cancel: pCancel,
		done:   make(chan struct{}),
	}

	sw.mu.Lock()
	sw.profiles[0] = real
	for i := 1; i < len(targets); i++ {
		sw.profiles[i] = newShadowProfileWorker()
	}
	sw.mu.Unlock()

	go func() {
		defer close(real.done)
		s.runStreamEncoder(pCtx, streamID, sw.rawIngest, sw.tc, targets)
	}()
}

// shadowDoneCh is a single pre-closed channel shared by every shadow
// profileWorker — read returns immediately, so Stop / StopProfile waits
// don't block on shadows. Sharing one closed channel across all shadows
// is safe (channel reads are concurrency-safe).
var shadowDoneCh = func() chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}()

// newShadowProfileWorker returns a profileWorker that satisfies the
// streamWorker.profiles invariants without owning a goroutine.
//   - cancel is a no-op so StopProfile on a shadow does not propagate to
//     the real multi-output FFmpeg process.
//   - done is the pre-closed shadowDoneCh so any wait returns instantly.
//
// Errors and restartCount fields are still per-instance (default zero) so
// recordProfileError accumulates a distinct history per rung — required
// for the UI to show "track_2 had 3 crashes" the same way per-profile
// mode does.
func newShadowProfileWorker() *profileWorker {
	return &profileWorker{
		cancel: func() {},
		done:   shadowDoneCh,
	}
}

// Stop cancels all FFmpeg encoders for a stream and waits for them to exit.
func (s *Service) Stop(streamID domain.StreamCode) {
	s.mu.Lock()
	sw, ok := s.workers[streamID]
	if !ok {
		s.mu.Unlock()
		return
	}
	delete(s.workers, streamID)
	s.mu.Unlock()

	sw.baseCancel()

	// Wait for all profile goroutines to finish.
	sw.mu.Lock()
	profiles := make(map[int]*profileWorker, len(sw.profiles))
	for k, v := range sw.profiles {
		profiles[k] = v
	}
	sw.mu.Unlock()
	for _, pw := range profiles {
		<-pw.done
	}

	s.m.TranscoderWorkersActive.WithLabelValues(string(streamID)).Set(0)
	s.m.TranscoderQualitiesActive.WithLabelValues(string(streamID)).Set(0)
	// Drop unhealthy markers so a fresh Start with the same code
	// observes a healthy baseline. Done after goroutine wait so any
	// in-flight crash records can finish before we wipe state.
	s.dropHealthState(streamID)
	//nolint:contextcheck // baseCtx is cancelled; publish must outlive it
	s.bus.Publish(context.Background(), domain.Event{
		Type:       domain.EventTranscoderStopped,
		StreamCode: streamID,
	})
}

// StopProfile stops a single FFmpeg encoder for one profile index.
func (s *Service) StopProfile(streamID domain.StreamCode, profileIndex int) {
	s.mu.Lock()
	sw, ok := s.workers[streamID]
	s.mu.Unlock()
	if !ok {
		return
	}

	sw.mu.Lock()
	pw, ok := sw.profiles[profileIndex]
	if !ok {
		sw.mu.Unlock()
		return
	}
	delete(sw.profiles, profileIndex)
	sw.mu.Unlock()

	pw.cancel()
	<-pw.done

	slog.Info("transcoder: profile stopped",
		"stream_code", streamID,
		"profile", buffer.VideoTrackSlug(profileIndex),
	)
	s.updateMetrics(streamID, sw)
}

// StartProfile starts a single FFmpeg encoder for one profile index on an existing stream worker.
func (s *Service) StartProfile(streamID domain.StreamCode, profileIndex int, target RenditionTarget) error {
	s.mu.Lock()
	sw, ok := s.workers[streamID]
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("transcoder: stream %s not running", streamID)
	}

	sw.mu.Lock()
	if _, exists := sw.profiles[profileIndex]; exists {
		sw.mu.Unlock()
		return fmt.Errorf("transcoder: profile %d already running for stream %s", profileIndex, streamID)
	}
	sw.mu.Unlock()

	s.spawnProfile(streamID, sw, profileIndex, target)

	slog.Info("transcoder: profile started",
		"stream_code", streamID,
		"profile", buffer.VideoTrackSlug(profileIndex),
		"write_to", target.BufferID,
	)
	s.updateMetrics(streamID, sw)
	return nil
}

// spawnProfile creates a per-profile context and launches the encoder goroutine.
func (s *Service) spawnProfile(streamID domain.StreamCode, sw *streamWorker, profileIndex int, target RenditionTarget) {
	profileCtx, profileCancel := context.WithCancel(sw.baseCtx)
	pw := &profileWorker{
		cancel: profileCancel,
		done:   make(chan struct{}),
	}

	sw.mu.Lock()
	sw.profiles[profileIndex] = pw
	sw.mu.Unlock()

	go func() {
		defer close(pw.done)
		s.runProfileEncoder(profileCtx, streamID, sw.rawIngest, target.BufferID, sw.tc, profileIndex, target.Profile)
	}()
}

// updateMetrics refreshes the active worker/quality gauge for a stream.
func (s *Service) updateMetrics(streamID domain.StreamCode, sw *streamWorker) {
	sw.mu.Lock()
	n := float64(len(sw.profiles))
	sw.mu.Unlock()
	s.m.TranscoderWorkersActive.WithLabelValues(string(streamID)).Set(n)
	s.m.TranscoderQualitiesActive.WithLabelValues(string(streamID)).Set(n)
}

// stderrTail is a bounded ring of the last N "interesting" stderr lines —
// the warn-level fraction that survives logStderr's noise filter. runOnce
// reads the snapshot on crash to enrich the otherwise-opaque exit-status
// error ("exit status 8" → plus the actual filter / encoder error message).
type stderrTail struct {
	mu    sync.Mutex
	lines []string
	cap   int
}

const stderrTailCap = 8

func newStderrTail(cap int) *stderrTail {
	return &stderrTail{cap: cap}
}

func (t *stderrTail) push(line string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.lines) >= t.cap {
		copy(t.lines, t.lines[1:])
		t.lines[t.cap-1] = line
		return
	}
	t.lines = append(t.lines, line)
}

func (t *stderrTail) snapshot() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]string, len(t.lines))
	copy(out, t.lines)
	return out
}

// formatStderrTail joins lines with " | " for compact one-line embedding in
// error messages. Returns "" when no lines were captured.
func formatStderrTail(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, " | ")
}

// logStderr scans FFmpeg stderr, filters out known-benign noise (packet/PPS/
// MMCO debug spam) at debug level, surfaces real errors at warn, and pushes
// every warn-level line into tail (when non-nil) for crash diagnostics.
func (s *Service) logStderr(streamID domain.StreamCode, profile string, r io.Reader, tail *stderrTail) {
	sc := bufio.NewScanner(r)
	const maxLine = 64 * 1024
	buf := make([]byte, maxLine)
	sc.Buffer(buf, maxLine)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		// FFmpeg compensates PTS/DTS jumps (common when ingest fails over between HLS variants).
		if strings.Contains(line, "timestamp discontinuity") && strings.Contains(line, "new offset") {
			slog.Debug("transcoder: ffmpeg timestamp resync", "stream_code", streamID, "profile", profile, "msg", line)
			continue
		}
		trim := strings.TrimSpace(line)
		if strings.Contains(line, "Packet corrupt") ||
			strings.Contains(line, "PES packet size mismatch") ||
			strings.HasPrefix(trim, "Last message repeated") {
			slog.Debug("transcoder: ffmpeg stderr", "stream_code", streamID, "profile", profile, "msg", line)
			continue
		}
		// H.264 decoder / demuxer noise: probe gaps, segment joins, B-frame reorder, MMCO.
		// "Invalid timestamps" is mpegts-muxer noise: input has DTS > PTS or
		// non-monotonic PTS (common with re-muxed Wowza/Flussonic upstreams
		// or weird B-frame structures); muxer self-corrects, output stays
		// valid. Without this filter the warn-level spam is one line per
		// affected packet (30/sec for 30fps).
		if strings.Contains(line, "non-existing PPS") ||
			strings.Contains(line, "no frame!") ||
			strings.Contains(line, "error while decoding MB") ||
			strings.Contains(line, "Could not find codec parameters") ||
			strings.Contains(line, "Consider increasing the value for the 'analyzeduration'") ||
			strings.Contains(line, "unspecified pixel format") ||
			strings.Contains(line, "co located POCs unavailable") ||
			strings.Contains(line, "mmco: unref short failure") ||
			strings.Contains(line, "reference picture missing during reorder") ||
			strings.Contains(line, "Missing reference picture") ||
			strings.Contains(line, "Invalid timestamps") {
			slog.Debug("transcoder: ffmpeg stderr", "stream_code", streamID, "profile", profile, "msg", line)
			continue
		}
		if tail != nil {
			tail.push(line)
		}
		slog.Warn("transcoder: ffmpeg stderr", "stream_code", streamID, "profile", profile, "msg", line)
	}
}
