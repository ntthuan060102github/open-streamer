package publisher

import (
	"context"
	"encoding/xml"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/ntthuan060102github/open-streamer/internal/buffer"
	"github.com/ntthuan060102github/open-streamer/internal/domain"
)

// serveDASHAdaptive starts one fMP4 packager goroutine per ABR rendition and a
// single dashABRMaster that periodically rewrites the root index.mpd.
//
// If no renditions are available (transcoder not configured), it falls back to
// single-rendition serveDASH.
func (s *Service) serveDASHAdaptive(ctx context.Context, stream *domain.Stream) {
	renditions := buffer.RenditionsForTranscoder(stream.Code, stream.Transcoder)
	if len(renditions) == 0 {
		s.serveDASH(ctx, s.mediaBufferFor(stream.Code))
		return
	}

	cfg := s.cfg.DASH
	rootDir := filepath.Join(cfg.Dir, string(stream.Code))

	if err := resetOutputDir(rootDir); err != nil {
		slog.Error("publisher: DASH ABR reset output dir",
			"stream_code", stream.Code, "err", err)
		return
	}

	rootManifest := filepath.Join(rootDir, "index.mpd")
	master := newDashABRMaster(rootManifest, stream.Code, cfg.LiveSegmentSec, cfg.LiveWindow)

	// The rendition with the highest bandwidth gets audio.
	bestIdx := buffer.BestRenditionIndex(renditions)

	slog.Info("publisher: DASH ABR serve started",
		"stream_code", stream.Code, "renditions", len(renditions), "dash_dir", rootDir)

	var wg sync.WaitGroup
	for i, r := range renditions {
		slug := r.Slug
		shardDir := filepath.Join(rootDir, slug)

		if err := resetOutputDir(shardDir); err != nil {
			slog.Error("publisher: DASH ABR reset shard dir",
				"stream_code", stream.Code, "slug", slug, "err", err)
			continue
		}

		opts := &dashRunOpts{
			abrMaster:         master,
			abrSlug:           slug,
			videoBandwidthBps: r.BandwidthBps(),
			packAudio:         i == bestIdx,
		}

		sub, err := s.buf.Subscribe(r.BufferID)
		if err != nil {
			slog.Error("publisher: DASH ABR subscribe failed",
				"stream_code", stream.Code, "rendition", slug, "err", err)
			continue
		}

		wg.Add(1)
		go func(rSub *buffer.Subscriber, shardD, sl string, o *dashRunOpts) {
			defer wg.Done()
			defer s.buf.Unsubscribe(r.BufferID, rSub)
			runDASHFMP4Packager(
				ctx,
				stream.Code,
				rSub,
				shardD,
				"", // no per-shard MPD; master writes the root
				cfg.LiveSegmentSec,
				cfg.LiveWindow,
				cfg.LiveHistory,
				cfg.LiveEphemeral,
				o,
			)
		}(sub, shardDir, slug, opts)
	}

	wg.Wait()
	master.stop()
	slog.Info("publisher: DASH ABR serve stopped", "stream_code", stream.Code)
}

// ─── dashABRMaster ────────────────────────────────────────────────────────

// dashABRRep is a point-in-time snapshot of one ABR shard used to build the root MPD.
type dashABRRep struct {
	slug       string
	videoBW    int
	videoCodec string
	width      int
	height     int
	audioSR    int
	audioCodec string
	hasVideo   bool
	hasAudio   bool

	vSegs   []string
	vDurs   []uint64
	vStarts []uint64
	aSegs   []string
	aDurs   []uint64
	aStarts []uint64

	availStart time.Time
	segSec     int
}

// dashABRMaster aggregates per-shard flush notifications and writes the root MPD.
// It debounces rapid successive notifications (e.g. all renditions flushing simultaneously).
type dashABRMaster struct {
	mu           sync.Mutex
	rootManifest string
	streamID     domain.StreamCode
	segSec       int
	window       int
	shards       map[string]*dashFMP4Packager // slug → latest packager reference
	debounce     *time.Timer
	stopped      bool
}

func newDashABRMaster(rootManifest string, streamID domain.StreamCode, segSec, window int) *dashABRMaster {
	return &dashABRMaster{
		rootManifest: rootManifest,
		streamID:     streamID,
		segSec:       segSec,
		window:       window,
		shards:       make(map[string]*dashFMP4Packager),
	}
}

// onShardUpdated is called (under p.mu) by each shard after every segment flush.
// It stores the shard reference and schedules a debounced root MPD write.
func (m *dashABRMaster) onShardUpdated(p *dashFMP4Packager) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopped {
		return
	}
	m.shards[p.abrSlug] = p
	if m.debounce != nil {
		m.debounce.Stop()
	}
	m.debounce = time.AfterFunc(150*time.Millisecond, m.flushRoot)
}

// stop cancels any pending debounced timer and triggers a final MPD write.
func (m *dashABRMaster) stop() {
	m.mu.Lock()
	if m.debounce != nil {
		m.debounce.Stop()
	}
	m.stopped = true
	m.mu.Unlock()
	m.flushRoot()
}

// flushRoot builds and writes the root ABR MPD from the latest shard snapshots.
func (m *dashABRMaster) flushRoot() {
	m.mu.Lock()
	if len(m.shards) == 0 {
		m.mu.Unlock()
		return
	}

	// Snapshot shard state while holding m.mu but NOT the individual p.mu.
	// We can do this safely because we hold references and these fields are only
	// mutated under p.mu; we take consistent snapshots below under p.mu.
	slugs := make([]string, 0, len(m.shards))
	for s := range m.shards {
		slugs = append(slugs, s)
	}
	sort.Strings(slugs)

	snaps := make([]dashABRRep, 0, len(slugs))
	for _, sl := range slugs {
		p := m.shards[sl]
		p.mu.Lock()
		snap := dashABRRep{
			slug:       sl,
			videoBW:    p.effectiveVideoBW(),
			videoCodec: p.videoCodec,
			width:      p.width,
			height:     p.height,
			audioSR:    p.audioSR,
			audioCodec: p.audioCodec,
			hasVideo:   p.videoInit != nil,
			hasAudio:   p.audioInit != nil && p.packAudio,
			vSegs:      windowTail(append([]string(nil), p.onDiskV...), m.window),
			vDurs:      windowTailUint64(append([]uint64(nil), p.vSegDurs...), m.window),
			vStarts:    windowTailUint64(append([]uint64(nil), p.vSegStarts...), m.window),
			aSegs:      windowTail(append([]string(nil), p.onDiskA...), m.window),
			aDurs:      windowTailUint64(append([]uint64(nil), p.aSegDurs...), m.window),
			aStarts:    windowTailUint64(append([]uint64(nil), p.aSegStarts...), m.window),
			availStart: p.availabilityStart,
			segSec:     p.segSec,
		}
		p.mu.Unlock()
		snaps = append(snaps, snap)
	}
	m.mu.Unlock()

	if err := writeDASHABRRootMPD(m.rootManifest, m.segSec, m.window, snaps); err != nil {
		slog.Warn("publisher: DASH ABR root MPD write failed",
			"stream_code", m.streamID, "err", err)
	}
}

// writeDASHABRRootMPD serialises the multi-rendition root MPD to disk.
// The video AdaptationSet contains one Representation per rendition; the audio
// AdaptationSet contains the single audio Representation from the "best" shard.
func writeDASHABRRootMPD(path string, segSec, window int, snaps []dashABRRep) error {
	// Choose a representative availabilityStartTime (earliest non-zero).
	var availStart time.Time
	for _, s := range snaps {
		if !s.availStart.IsZero() && (availStart.IsZero() || s.availStart.Before(availStart)) {
			availStart = s.availStart
		}
	}

	ast := ""
	if !availStart.IsZero() {
		ast = availStart.UTC().Format(time.RFC3339)
	}
	pub := time.Now().UTC().Format(time.RFC3339)
	minBuf := max(4, segSec*2)
	sugDelay := max(6, segSec*3)
	maxSegDur := max(segSec*3, segSec+1)

	doc := mpdRoot{
		XMLName:                    xml.Name{Local: "MPD"},
		XMLNS:                      "urn:mpeg:dash:schema:mpd:2011",
		Type:                       "dynamic",
		Profiles:                   "urn:mpeg:dash:profile:isoff-live:2011",
		MinBuffer:                  fmt.Sprintf("PT%dS", minBuf),
		SuggestedPresentationDelay: fmt.Sprintf("PT%dS", sugDelay),
		MaxSegmentDuration:         fmt.Sprintf("PT%dS", maxSegDur),
		AvailabilityStartTime:      ast,
		MinUpdate:                  fmt.Sprintf("PT%dS", segSec),
		BufferDepth:                fmt.Sprintf("PT%dS", segSec*window),
		PublishTime:                pub,
		Periods:                    []mpdPeriod{{ID: "0", Start: "PT0S"}},
		UTCTiming: &mpdUTCTiming{
			SchemeIDURI: "urn:mpeg:dash:utc:direct:2014",
			Value:       pub,
		},
	}
	per := &doc.Periods[0]

	// Video AdaptationSet — one Representation per rendition.
	vAS := mpdAdaptationSet{
		ID:               "0",
		ContentType:      "video",
		MimeType:         "video/mp4",
		SegmentAlignment: "true",
		StartWithSAP:     "1",
	}
	for i, snap := range snaps {
		if !snap.hasVideo || len(snap.vSegs) == 0 {
			continue
		}
		vTL := buildSegTimeline(snap.vSegs, snap.vDurs, snap.vStarts)
		if vTL == nil {
			continue
		}
		startNum := parseDashSegNum('v', snap.vSegs[0])
		if startNum <= 0 {
			startNum = 1
		}
		baseURL := snap.slug + "/"
		vAS.Representations = append(vAS.Representations, mpdRepresentation{
			ID:        fmt.Sprintf("v%d", i),
			MimeType:  "video/mp4",
			Codecs:    snap.videoCodec,
			Bandwidth: snap.videoBW,
			Width:     snap.width,
			Height:    snap.height,
			BaseURL:   baseURL,
			SegmentTemplate: mpdSegmentTemplate{
				Timescale:      dashVideoTimescale,
				Initialization: "init_v.mp4",
				Media:          dashVideoMediaPattern,
				StartNumber:    startNum,
				Timeline:       vTL,
			},
		})
	}
	if len(vAS.Representations) > 0 {
		per.AdaptationSets = append(per.AdaptationSets, vAS)
	}

	// Audio AdaptationSet — taken from the first shard that has audio.
	for _, snap := range snaps {
		if !snap.hasAudio || len(snap.aSegs) == 0 {
			continue
		}
		aTL := buildSegTimeline(snap.aSegs, snap.aDurs, snap.aStarts)
		if aTL == nil {
			continue
		}
		startNum := parseDashSegNum('a', snap.aSegs[0])
		if startNum <= 0 {
			startNum = 1
		}
		asr := snap.audioSR
		baseURL := snap.slug + "/"
		per.AdaptationSets = append(per.AdaptationSets, mpdAdaptationSet{
			ID:               "1",
			ContentType:      "audio",
			MimeType:         "audio/mp4",
			SegmentAlignment: "true",
			StartWithSAP:     "1",
			Representations: []mpdRepresentation{{
				ID:                "a0",
				MimeType:          "audio/mp4",
				Codecs:            snap.audioCodec,
				Bandwidth:         128_000,
				AudioSamplingRate: &asr,
				BaseURL:           baseURL,
				SegmentTemplate: mpdSegmentTemplate{
					Timescale:      snap.audioSR,
					Initialization: "init_a.mp4",
					Media:          dashAudioMediaPattern,
					StartNumber:    startNum,
					Timeline:       aTL,
				},
			}},
		})
		break // only one audio rendition
	}

	if len(per.AdaptationSets) == 0 {
		return nil
	}

	out, err := xml.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append([]byte(xml.Header), out...))
}
