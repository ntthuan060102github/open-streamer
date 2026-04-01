package publisher

import (
	"context"
	"encoding/xml"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ntthuan060102github/open-streamer/internal/buffer"
	"github.com/ntthuan060102github/open-streamer/internal/domain"
)

// dashRunOpts configures one shard of a multi-bitrate DASH ladder (nil = single-representation packager).
type dashRunOpts struct {
	abrMaster         *dashABRMaster
	abrSlug           string
	videoBandwidthBps int
	packAudio         bool
}

// dashABRMaster writes one root index.mpd that references per-slug subdirectories.
type dashABRMaster struct {
	mu       sync.Mutex
	rootPath string
	bestSlug string
	segSec   int
	window   int
	streamID domain.StreamCode
	reps     map[string]*dashFMP4Packager
	debounce *time.Timer
}

func newDashABRMaster(rootPath, bestSlug string, segSec, window int, streamID domain.StreamCode) *dashABRMaster {
	return &dashABRMaster{
		rootPath: rootPath,
		bestSlug: bestSlug,
		segSec:   segSec,
		window:   window,
		streamID: streamID,
		reps:     make(map[string]*dashFMP4Packager),
	}
}

func (m *dashABRMaster) onShardUpdated(p *dashFMP4Packager) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p.abrSlug == "" {
		return
	}
	m.reps[p.abrSlug] = p
	if m.debounce != nil {
		m.debounce.Stop()
	}
	m.debounce = time.AfterFunc(100*time.Millisecond, func() {
		m.flushRoot()
	})
}

func (m *dashABRMaster) flushRoot() {
	m.mu.Lock()
	slugs := make([]string, 0, len(m.reps))
	for s := range m.reps {
		slugs = append(slugs, s)
	}
	sort.Strings(slugs)
	rootPath := m.rootPath
	best := m.bestSlug
	segSec := m.segSec
	win := m.window
	streamID := m.streamID
	repsSnap := make(map[string]*dashFMP4Packager, len(m.reps))
	for k, v := range m.reps {
		repsSnap[k] = v
	}
	m.mu.Unlock()

	vreps := make([]dashABRVRep, 0, len(slugs))
	var ast time.Time

	for _, slug := range slugs {
		p := repsSnap[slug]
		if p == nil {
			continue
		}
		p.mu.Lock()
		if p.videoInit == nil || len(p.onDiskV) == 0 {
			p.mu.Unlock()
			continue
		}
		if !p.availabilityStart.IsZero() && (ast.IsZero() || p.availabilityStart.Before(ast)) {
			ast = p.availabilityStart
		}
		vreps = append(vreps, dashABRVRep{
			slug:   slug,
			width:  p.width,
			height: p.height,
			bw:     p.effectiveVideoBW(),
			codec:  p.videoCodec,
			segs:   append([]string(nil), windowTail(p.onDiskV, win)...),
			durs:   append([]uint64(nil), windowTailUint64(p.vSegDurs, win)...),
			starts: append([]uint64(nil), windowTailUint64(p.vSegStarts, win)...),
		})
		p.mu.Unlock()
	}

	var audioPack *dashFMP4Packager
	if best != "" {
		audioPack = repsSnap[best]
	}

	if len(vreps) == 0 {
		return
	}

	if err := writeDashABRRootMPD(rootPath, streamID, ast, segSec, win, vreps, audioPack, best); err != nil {
		slog.Warn("publisher: DASH ABR root MPD write failed", "stream_code", streamID, "err", err)
	}
}

// dashABRVRep is a snapshot of one video ladder rung for the root MPD.
type dashABRVRep struct {
	slug   string
	width  int
	height int
	bw     int
	codec  string
	segs   []string
	durs   []uint64
	starts []uint64
}

func writeDashABRRootMPD(
	rootPath string,
	_ domain.StreamCode,
	availabilityStart time.Time,
	segSec, window int,
	vreps []dashABRVRep,
	audioPack *dashFMP4Packager,
	bestSlug string,
) error {
	ast := ""
	if !availabilityStart.IsZero() {
		ast = availabilityStart.UTC().Format(time.RFC3339)
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
		Periods: []mpdPeriod{{
			ID:    "0",
			Start: "PT0S",
		}},
		UTCTiming: &mpdUTCTiming{
			SchemeIDURI: "urn:mpeg:dash:utc:direct:2014",
			Value:       pub,
		},
	}
	per := &doc.Periods[0]

	videoReps := make([]mpdRepresentation, 0, len(vreps))
	for i := range vreps {
		vr := &vreps[i]
		vSegs := vr.segs
		vDurs := vr.durs
		vStarts := vr.starts
		if len(vSegs) == 0 {
			continue
		}
		vTL := &mpdSegTimeline{S: make([]mpdSTimeline, 0, len(vSegs))}
		for j := range vSegs {
			d := uint64(segSec) * uint64(dashVideoTimescale)
			if j < len(vDurs) && vDurs[j] > 0 {
				d = vDurs[j]
			}
			st := mpdSTimeline{D: d}
			if j == 0 && len(vStarts) > 0 {
				t0 := vStarts[0]
				st.T = &t0
			}
			vTL.S = append(vTL.S, st)
		}
		vStartNum := parseDashSegMediaNumber('v', vSegs[0])
		if vStartNum <= 0 {
			vStartNum = 1
		}
		base := vr.slug + "/"
		videoReps = append(videoReps, mpdRepresentation{
			ID:        fmt.Sprintf("v_%s", vr.slug),
			MimeType:  "video/mp4",
			Codecs:    vr.codec,
			Bandwidth: vr.bw,
			Width:     vr.width,
			Height:    vr.height,
			BaseURL:   base,
			SegmentTemplate: mpdSegmentTemplate{
				Timescale:      dashVideoTimescale,
				Initialization: "init_v.mp4",
				Media:          dashVideoMediaPattern,
				StartNumber:    vStartNum,
				Timeline:       vTL,
			},
		})
	}
	if len(videoReps) > 0 {
		per.AdaptationSets = append(per.AdaptationSets, mpdAdaptationSet{
			ID:               "0",
			ContentType:      "video",
			MimeType:         "video/mp4",
			SegmentAlignment: "true",
			StartWithSAP:     "1",
			Representations:  videoReps,
		})
	}

	if audioPack != nil && bestSlug != "" {
		audioPack.mu.Lock()
		aSegs := windowTail(audioPack.onDiskA, window)
		aDurs := windowTailUint64(audioPack.aSegDurs, window)
		aStarts := windowTailUint64(audioPack.aSegStarts, window)
		audioSR := audioPack.audioSR
		audioCodec := audioPack.audioCodec
		hasAudioInit := audioPack.audioInit != nil
		segPer := 0
		if hasAudioInit {
			segPer = audioPack.audioFramesPerSegment()
		}
		audioPack.mu.Unlock()

		if hasAudioInit && len(aSegs) > 0 {
			defADur := uint64(segPer * 1024)
			aTL := &mpdSegTimeline{S: make([]mpdSTimeline, 0, len(aSegs))}
			for i := range aSegs {
				d := defADur
				if i < len(aDurs) && aDurs[i] > 0 {
					d = aDurs[i]
				}
				st := mpdSTimeline{D: d}
				if i == 0 && len(aStarts) > 0 {
					t0 := aStarts[0]
					st.T = &t0
				}
				aTL.S = append(aTL.S, st)
			}
			aStartNum := parseDashSegMediaNumber('a', aSegs[0])
			if aStartNum <= 0 {
				aStartNum = 1
			}
			asr := audioSR
			per.AdaptationSets = append(per.AdaptationSets, mpdAdaptationSet{
				ID:               "1",
				ContentType:      "audio",
				MimeType:         "audio/mp4",
				SegmentAlignment: "true",
				StartWithSAP:     "1",
				Representations: []mpdRepresentation{{
					ID:                "a0",
					MimeType:          "audio/mp4",
					Codecs:            audioCodec,
					Bandwidth:         128_000,
					AudioSamplingRate: &asr,
					BaseURL:           bestSlug + "/",
					SegmentTemplate: mpdSegmentTemplate{
						Timescale:      audioSR,
						Initialization: "init_a.mp4",
						Media:          dashAudioMediaPattern,
						StartNumber:    aStartNum,
						Timeline:       aTL,
					},
				}},
			})
		}
	}

	if len(per.AdaptationSets) == 0 {
		return nil
	}

	out, err := xml.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	hdr := []byte(xml.Header)
	return writeFileAtomic(rootPath, append(hdr, out...))
}

func (s *Service) serveDASHAdaptive(ctx context.Context, stream *domain.Stream) {
	code := stream.Code
	dashDir := strings.TrimSpace(s.cfg.DASH.Dir)
	if dashDir == "" {
		slog.Error("publisher: DASH ABR — dash.dir empty", "stream_code", code)
		return
	}

	rends := buffer.RenditionsForTranscoder(code, stream.Transcoder)
	if len(rends) == 0 {
		s.serveDASH(ctx, code)
		return
	}

	streamBase := filepath.Join(dashDir, string(code))
	if err := resetOutputDir(streamBase); err != nil {
		slog.Error("publisher: DASH ABR setup dir failed", "stream_code", code, "dir", streamBase, "err", err)
		return
	}

	bestIdx := buffer.BestRenditionIndex(rends)
	bestSlug := rends[bestIdx].Slug
	rootMPD := filepath.Join(streamBase, "index.mpd")
	master := newDashABRMaster(rootMPD, bestSlug,
		s.cfg.DASH.LiveSegmentSec,
		s.cfg.DASH.LiveWindow,
		code,
	)

	segSec := s.cfg.DASH.LiveSegmentSec
	win := s.cfg.DASH.LiveWindow
	hist := s.cfg.DASH.LiveHistory
	eph := s.cfg.DASH.LiveEphemeral

	var wg sync.WaitGroup
	for _, r := range rends {
		wg.Add(1)
		go func(r buffer.RenditionPlayout) {
			defer wg.Done()
			sub, err := s.buf.Subscribe(r.BufferID)
			if err != nil {
				slog.Error("publisher: DASH ABR subscribe failed", "stream_code", code, "slug", r.Slug, "err", err)
				return
			}
			defer s.buf.Unsubscribe(r.BufferID, sub)

			shardDir := filepath.Join(streamBase, r.Slug)
			if err := os.MkdirAll(shardDir, 0o755); err != nil {
				slog.Error("publisher: DASH ABR mkdir failed", "stream_code", code, "slug", r.Slug, "err", err)
				return
			}
			opts := &dashRunOpts{
				abrMaster:         master,
				abrSlug:           r.Slug,
				videoBandwidthBps: r.BandwidthBps(),
				packAudio:         r.Slug == bestSlug,
			}
			runDASHFMP4Packager(ctx, code, sub, shardDir, "", segSec, win, hist, eph, opts)
		}(r)
	}
	wg.Wait()
}
