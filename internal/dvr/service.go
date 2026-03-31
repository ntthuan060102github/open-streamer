// Package dvr implements the DVR (Digital Video Recorder).
// It reads from Buffer Hub, writes MPEG-TS segments to disk,
// and maintains an M3U8 index for timeshift and VOD playback.
// DVR never re-ingests from the network — it consumes from the Buffer Hub only.
package dvr

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/open-streamer/open-streamer/config"
	"github.com/open-streamer/open-streamer/internal/buffer"
	"github.com/open-streamer/open-streamer/internal/domain"
	"github.com/open-streamer/open-streamer/internal/events"
	"github.com/open-streamer/open-streamer/internal/store"
	"github.com/samber/do/v2"
)

// recordingSession tracks an active DVR recording.
type recordingSession struct {
	recording *domain.Recording
	cancel    context.CancelFunc
}

// Service manages DVR recording sessions.
type Service struct {
	cfg       config.DVRConfig
	buf       *buffer.Service
	bus       events.Bus
	recRepo   store.RecordingRepository
	mu        sync.Mutex
	sessions  map[domain.StreamCode]*recordingSession
}

// New creates a Service and registers it with the DI injector.
func New(i do.Injector) (*Service, error) {
	cfg := do.MustInvoke[*config.Config](i)
	if !cfg.DVR.Enabled {
		return &Service{cfg: cfg.DVR}, nil
	}

	buf := do.MustInvoke[*buffer.Service](i)
	bus := do.MustInvoke[events.Bus](i)
	recRepo := do.MustInvoke[store.RecordingRepository](i)

	return &Service{
		cfg:      cfg.DVR,
		buf:      buf,
		bus:      bus,
		recRepo:  recRepo,
		sessions: make(map[domain.StreamCode]*recordingSession),
	}, nil
}

// StartRecording begins writing segments for the given stream.
// segmentDuration controls how long each MPEG-TS segment file will be.
// If 0, defaults to 6 seconds. Pass the stream's DVR config value if available.
func (s *Service) StartRecording(ctx context.Context, streamID domain.StreamCode, segmentDuration time.Duration) (*domain.Recording, error) {
	if !s.cfg.Enabled {
		return nil, fmt.Errorf("dvr: disabled in config")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.sessions[streamID]; ok {
		return nil, fmt.Errorf("dvr: stream %s already recording", streamID)
	}

	rec := &domain.Recording{
		ID:        domain.RecordingID(fmt.Sprintf("%s-%d", streamID, time.Now().UnixNano())),
		StreamCode: streamID,
		StartedAt: time.Now(),
		Status:    domain.RecordingStatusRecording,
	}

	if err := s.recRepo.Save(ctx, rec); err != nil {
		return nil, fmt.Errorf("dvr: save recording: %w", err)
	}

	workerCtx, cancel := context.WithCancel(ctx)
	s.sessions[streamID] = &recordingSession{recording: rec, cancel: cancel}

	if segmentDuration <= 0 {
		segmentDuration = 6 * time.Second
	}
	go s.record(workerCtx, rec, segmentDuration)

	s.bus.Publish(ctx, domain.Event{
		Type:     domain.EventRecordingStarted,
		StreamCode: streamID,
		Payload:  map[string]any{"recording_id": rec.ID},
	})

	slog.Info("dvr: recording started", "stream_code", streamID, "recording_id", rec.ID)
	return rec, nil
}

// StopRecording finalises the recording and flushes the M3U8 index.
func (s *Service) StopRecording(ctx context.Context, streamID domain.StreamCode) error {
	s.mu.Lock()
	session, ok := s.sessions[streamID]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("dvr: no active recording for stream %s", streamID)
	}
	session.cancel()
	delete(s.sessions, streamID)
	s.mu.Unlock()

	now := time.Now()
	session.recording.StoppedAt = &now
	session.recording.Status = domain.RecordingStatusStopped

	if err := s.recRepo.Save(ctx, session.recording); err != nil {
		return fmt.Errorf("dvr: update recording: %w", err)
	}

	s.bus.Publish(ctx, domain.Event{
		Type:     domain.EventRecordingStopped,
		StreamCode: streamID,
		Payload:  map[string]any{"recording_id": session.recording.ID},
	})

	slog.Info("dvr: recording stopped",
		"stream_code", streamID,
		"recording_id", session.recording.ID,
		"segments", len(session.recording.Segments),
	)
	return nil
}

func (s *Service) record(ctx context.Context, rec *domain.Recording, segDuration time.Duration) {
	sub, err := s.buf.Subscribe(rec.StreamCode)
	if err != nil {
		slog.Error("dvr: subscribe failed", "stream_code", rec.StreamCode, "err", err)
		return
	}
	defer s.buf.Unsubscribe(rec.StreamCode, sub)
	var (
		segBuf   []byte
		segStart = time.Now()
		segIdx   int
	)

	for {
		select {
		case <-ctx.Done():
			if len(segBuf) > 0 {
				s.flushSegment(ctx, rec, segIdx, segBuf, time.Since(segStart))
			}
			return
		case pkt, ok := <-sub.Recv():
			if !ok {
				return
			}
			segBuf = append(segBuf, pkt...)

			if time.Since(segStart) >= segDuration {
				s.flushSegment(ctx, rec, segIdx, segBuf, time.Since(segStart))
				segBuf = segBuf[:0]
				segStart = time.Now()
				segIdx++
			}
		}
	}
}

func (s *Service) flushSegment(_ context.Context, rec *domain.Recording, idx int, data []byte, dur time.Duration) {
	// TODO: write segment to s.cfg.RootDir/{streamID}/{recordingID}/{idx}.ts
	// TODO: update M3U8 playlist file
	seg := domain.Segment{
		Index:     idx,
		Path:      fmt.Sprintf("%s/%s/%d.ts", rec.StreamCode, rec.ID, idx),
		Duration:  dur,
		Size:      int64(len(data)),
		CreatedAt: time.Now(),
	}
	rec.Segments = append(rec.Segments, seg)

	slog.Debug("dvr: segment flushed",
		"stream_code", rec.StreamCode,
		"recording_id", rec.ID,
		"index", idx,
		"size_bytes", seg.Size,
	)
}
