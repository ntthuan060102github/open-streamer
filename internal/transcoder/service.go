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
}

// New creates a Service and registers it with the DI injector.
func New(i do.Injector) (*Service, error) {
	cfg := do.MustInvoke[config.TranscoderConfig](i)
	buf := do.MustInvoke[*buffer.Service](i)
	bus := do.MustInvoke[events.Bus](i)
	m := do.MustInvoke[*metrics.Metrics](i)

	return &Service{
		cfg:     cfg,
		buf:     buf,
		bus:     bus,
		m:       m,
		workers: make(map[domain.StreamCode]*streamWorker),
	}, nil
}

// Start launches one FFmpeg encoder per RenditionTarget (same raw ingest, different output buffers).
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

	slog.Info("transcoder: stream job started",
		"stream_code", logStreamCode,
		"profiles", len(targets),
		"read_from", rawIngestID,
	)

	s.m.TranscoderWorkersActive.WithLabelValues(string(logStreamCode)).Set(float64(len(targets)))
	s.m.TranscoderQualitiesActive.WithLabelValues(string(logStreamCode)).Set(float64(len(targets)))
	s.bus.Publish(ctx, domain.Event{
		Type:       domain.EventTranscoderStarted,
		StreamCode: logStreamCode,
		Payload:    map[string]any{"profiles": len(targets), "raw_ingest_id": string(rawIngestID)},
	})

	for i, t := range targets {
		//nolint:contextcheck // spawnProfile derives its context from sw.baseCtx; by design
		s.spawnProfile(logStreamCode, sw, i, t)
	}
	return nil
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
