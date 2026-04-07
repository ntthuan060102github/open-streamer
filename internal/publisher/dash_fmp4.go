package publisher

// dash_fmp4.go — ISO BMFF (fMP4) packager for live MPEG-DASH.
//
// Data flow:
//
//	Buffer → FeedWirePacket → aligned TS → io.Pipe → mpeg2.TSDemuxer
//	                                                         │
//	                        ┌────────────────────────────────┤
//	                        │ onTSFrame (H.264/H.265 AU, AAC ADTS) │
//	                        └────────────────────────────────┘
//	                                         │
//	                          queue: vAnnex/vDTS/vPTS, aRaw/aPTS
//	                                         │
//	                     ticker ─► flushSegmentLocked
//	                                         │
//	                          init_v.mp4 / init_a.mp4
//	                          seg_v_NNNNN.m4s / seg_a_NNNNN.m4s
//	                          index.mpd (per-shard or root via abrMaster)
//
// Codec support: H.264 + H.265 + AAC-LC.  MP3 is logged and skipped.
//
// Segment trigger: queued video duration ≥ segSec (in 90 kHz ticks) AND the
// queue starts with an IDR, OR wall-clock elapsed ≥ segSec (fallback for
// audio-only or streams with wide keyframe intervals).

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Eyevinn/mp4ff/aac"
	"github.com/Eyevinn/mp4ff/avc"
	"github.com/Eyevinn/mp4ff/hevc"
	"github.com/Eyevinn/mp4ff/mp4"
	mpeg2 "github.com/yapingcat/gomedia/go-mpeg2"

	"github.com/ntthuan060102github/open-streamer/internal/buffer"
	"github.com/ntthuan060102github/open-streamer/internal/domain"
	"github.com/ntthuan060102github/open-streamer/internal/tsmux"
)

// dashVideoTimescale is the standard MPEG timescale for video tracks (90 kHz).
const dashVideoTimescale = 90000

// SegmentTemplate $Number$ patterns used by DASH clients (Shaka, dash.js, …).
const (
	dashVideoMediaPattern = `seg_v_$Number%05d$.m4s`
	dashAudioMediaPattern = `seg_a_$Number%05d$.m4s`
)

// dashRunOpts carries per-rendition ABR metadata.  nil = single-rendition mode.
type dashRunOpts struct {
	abrMaster         *dashABRMaster
	abrSlug           string
	videoBandwidthBps int
	packAudio         bool
}

// runDASHFMP4Packager is the entry point used by both serveDASH and serveDASHAdaptive.
func runDASHFMP4Packager(
	ctx context.Context,
	streamID domain.StreamCode,
	sub *buffer.Subscriber,
	streamDir string,
	manifestPath string,
	segSec, window, history int,
	ephemeral bool,
	opts *dashRunOpts,
) {
	if segSec <= 0 {
		segSec = 2
	}
	if window <= 0 {
		window = 12
	}
	if history < 0 {
		history = 0
	}

	p := &dashFMP4Packager{
		streamID:     streamID,
		streamDir:    streamDir,
		manifestPath: manifestPath,
		segSec:       segSec,
		window:       window,
		history:      history,
		ephemeral:    ephemeral,
		videoCodec:   "avc1.42E01E",
		audioCodec:   "mp4a.40.2",
		width:        1280,
		height:       720,
		packAudio:    true,
	}
	if opts != nil {
		p.abrMaster = opts.abrMaster
		p.abrSlug = opts.abrSlug
		if opts.videoBandwidthBps > 0 {
			p.videoBW = opts.videoBandwidthBps
		}
		p.packAudio = opts.packAudio
	}

	p.run(ctx, sub)
}

// dashFMP4Packager holds the mutable per-stream state for one DASH output shard.
type dashFMP4Packager struct {
	streamID     domain.StreamCode
	streamDir    string
	manifestPath string // empty when abrMaster writes the root MPD
	segSec       int
	window       int
	history      int
	ephemeral    bool

	mu sync.Mutex

	segmentStart time.Time

	// Video frame queue — Annex-B access units + ms timestamps from TSDemuxer.
	vAnnex  [][]byte
	vDTS    []uint64 // ms
	vPTS    []uint64 // ms
	videoPS []byte   // aggregate SPS+PPS bytes for init detection (cleared after init)

	// Audio frame queue — raw AAC (ADTS header stripped).
	aRaw [][]byte
	aPTS []uint64 // ms

	// Init segments (written once on first codec header detection).
	videoInit *mp4.InitSegment
	audioInit *mp4.InitSegment
	videoTID  uint32
	audioTID  uint32
	audioSR   int // sample rate in Hz

	// Codec strings for MPD representations.
	videoCodec string // e.g. "avc1.4d401f"
	audioCodec string // "mp4a.40.2"
	width      int
	height     int

	// isHEVC is set once the video stream is identified as H.265.
	isHEVC bool

	// Segment counters (1-based, match $Number$ in filenames).
	vSegN uint64
	aSegN uint64

	// Continuous decode timeline (tfdt) in track timescale.
	videoNextDecode uint64 // 90 kHz
	audioNextDecode uint64 // audioSR Hz

	// Sliding-window state for MPD generation.
	onDiskV    []string // filenames of written video segments
	onDiskA    []string // filenames of written audio segments
	vSegDurs   []uint64 // per-segment duration in 90 kHz ticks
	aSegDurs   []uint64 // per-segment duration in audioSR ticks
	vSegStarts []uint64 // tfdt of first sample per video segment
	aSegStarts []uint64 // tfdt of first sample per audio segment

	// Set on first segment available; needed for MPD availabilityStartTime.
	availabilityStart time.Time

	// ABR wiring.
	abrMaster *dashABRMaster
	abrSlug   string
	videoBW   int  // override bandwidth for ABR root MPD
	packAudio bool // false for non-best ABR renditions
}

// run is the main goroutine: wires the TS pipe, demuxer, and flush ticker.
func (p *dashFMP4Packager) run(ctx context.Context, sub *buffer.Subscriber) {
	// tb is a buffered pipe: Write never blocks, Read blocks only when empty.
	// This replaces io.Pipe (unbuffered) which caused the producer goroutine to
	// block on Write while the demuxer held p.mu in onTSFrame, preventing the
	// producer from reading new packets; the buffer hub then dropped them.
	tb := newTSBuffer()

	// Producer: buffer → FeedWirePacket → aligned TS → tb.
	go func() {
		defer tb.Close()
		var tsCarry []byte
		var avMux *tsmux.FromAV

		for {
			select {
			case <-ctx.Done():
				return
			case pkt, ok := <-sub.Recv():
				if !ok {
					return
				}
				// On source switch the TS muxer will reset, producing a fresh stream
				// from timestamp 0.  Any bytes in tsCarry belong to the previous source;
				// discard them so the pipe never receives a hybrid 188-byte packet that
				// mixes data from two independent TS streams.
				if pkt.AV != nil && pkt.AV.Discontinuity {
					tsCarry = tsCarry[:0]
				}
				tsmux.FeedWirePacket(pkt.TS, pkt.AV, &avMux, func(b []byte) {
					tsCarry = append(tsCarry, b...)
					for len(tsCarry) >= 188 {
						if tsCarry[0] != 0x47 {
							idx := bytes.IndexByte(tsCarry, 0x47)
							if idx < 0 {
								if len(tsCarry) > 187 {
									tsCarry = tsCarry[len(tsCarry)-187:]
								}
								return
							}
							tsCarry = tsCarry[idx:]
							if len(tsCarry) < 188 {
								return
							}
						}
						if len(tsCarry) >= 376 && tsCarry[188] != 0x47 {
							tsCarry = tsCarry[1:]
							continue
						}
						if _, err := tb.Write(tsCarry[:188]); err != nil {
							return
						}
						tsCarry = tsCarry[188:]
					}
				})
			}
		}
	}()

	// Demuxer: tb → onTSFrame (H.264 AUs and AAC ADTS frames).
	demux := mpeg2.NewTSDemuxer()
	demux.OnFrame = p.onTSFrame

	demuxDone := make(chan error, 1)
	go func() {
		demuxDone <- demux.Input(tb)
	}()

	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()

	segDur := time.Duration(p.segSec) * time.Second

	for {
		select {
		case <-ctx.Done():
			tb.Close()
			<-demuxDone
			p.mu.Lock()
			_ = p.flushSegmentLocked()
			p.mu.Unlock()
			return

		case err := <-demuxDone:
			if err != nil && ctx.Err() == nil {
				slog.Warn("publisher: DASH TS demux ended",
					"stream_code", p.streamID, "err", err)
			}
			p.mu.Lock()
			_ = p.flushSegmentLocked()
			p.mu.Unlock()
			return

		case <-tick.C:
			p.mu.Lock()

			// No data yet.
			if p.videoInit == nil && p.audioInit == nil {
				p.mu.Unlock()
				continue
			}

			wallDue := !p.segmentStart.IsZero() &&
				time.Since(p.segmentStart) >= segDur

			targetV := uint64(p.segSec) * dashVideoTimescale
			queuedV := totalQueuedVideoDur90k(p.vDTS)
			videoByDur := p.videoInit != nil && len(p.vAnnex) > 0 &&
				queuedV >= targetV &&
				(p.vSegN > 0 || p.queuedVideoStartsWithIDRLocked())

			audioOnly := p.videoInit == nil && p.audioInit != nil &&
				len(p.aRaw) >= p.audioFramesPerSegment()

			if wallDue || videoByDur || audioOnly {
				if err := p.flushSegmentLocked(); err != nil {
					slog.Warn("publisher: DASH segment flush failed",
						"stream_code", p.streamID, "err", err)
				}
			}
			p.mu.Unlock()
		}
	}
}

// onTSFrame is the TSDemuxer callback; it runs in the demuxer goroutine.
func (p *dashFMP4Packager) onTSFrame(cid mpeg2.TS_STREAM_TYPE, frame []byte, pts, dts uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()

	switch cid {
	case mpeg2.TS_STREAM_AUDIO_MPEG1, mpeg2.TS_STREAM_AUDIO_MPEG2:
		// MP3 in TS — not supported in DASH fMP4.
		return

	case mpeg2.TS_STREAM_H264:
		if len(frame) == 0 {
			return
		}
		// Detect source switch: timestamps should be monotonically increasing.
		// A backward jump of more than 1 second means a new source started.
		// Flush accumulated frames from the old source before mixing in new ones;
		// this prevents uint64 underflow in duration computation inside
		// writeVideoSegmentLocked that would produce astronomically large samples.
		if len(p.vDTS) > 0 && dts+1000 < p.vDTS[len(p.vDTS)-1] {
			_ = p.flushSegmentLocked()
		}
		cp := append([]byte(nil), frame...)
		p.vAnnex = append(p.vAnnex, cp)
		p.vDTS = append(p.vDTS, dts)
		p.vPTS = append(p.vPTS, pts)

		// Accumulate SPS/PPS until the init segment is written.
		if p.videoInit == nil {
			p.videoPS = append(p.videoPS, cp...)
			p.tryInitVideoLocked()
		}
		if p.segmentStart.IsZero() && p.videoInit != nil {
			p.segmentStart = time.Now()
		}

	case mpeg2.TS_STREAM_H265:
		if len(frame) == 0 {
			return
		}
		if len(p.vDTS) > 0 && dts+1000 < p.vDTS[len(p.vDTS)-1] {
			_ = p.flushSegmentLocked()
		}
		cp := append([]byte(nil), frame...)
		p.vAnnex = append(p.vAnnex, cp)
		p.vDTS = append(p.vDTS, dts)
		p.vPTS = append(p.vPTS, pts)

		if p.videoInit == nil {
			p.isHEVC = true
			p.videoPS = append(p.videoPS, cp...)
			p.tryInitVideoH265Locked()
		}
		if p.segmentStart.IsZero() && p.videoInit != nil {
			p.segmentStart = time.Now()
		}

	case mpeg2.TS_STREAM_AAC:
		if !p.packAudio {
			return
		}
		// Same backward-jump guard as for video: flush before mixing frames
		// from two different sources in the audio queue.
		if len(p.aPTS) > 0 && pts+1000 < p.aPTS[len(p.aPTS)-1] {
			_ = p.flushSegmentLocked()
		}
		// A PES payload may contain multiple concatenated ADTS frames.
		pos := 0
		for pos+7 <= len(frame) {
			hdr, _, err := aac.DecodeADTSHeader(bytes.NewReader(frame[pos:]))
			if err != nil {
				pos++
				continue
			}
			hLen := int(hdr.HeaderLength)
			pLen := int(hdr.PayloadLength)
			if pos+hLen+pLen > len(frame) {
				break
			}
			raw := append([]byte(nil), frame[pos+hLen:pos+hLen+pLen]...)
			if p.audioInit == nil {
				if err := p.buildAudioInitLocked(hdr); err != nil {
					pos += hLen + pLen
					continue
				}
			}
			p.aRaw = append(p.aRaw, raw)
			p.aPTS = append(p.aPTS, pts)
			if p.segmentStart.IsZero() && p.videoInit == nil && p.audioInit != nil {
				p.segmentStart = time.Now()
			}
			pos += hLen + pLen
		}

	default:
		return
	}
}

// tryInitVideoLocked scans videoPS for SPS/PPS and, when found, writes init_v.mp4.
// Caller must hold p.mu.
func (p *dashFMP4Packager) tryInitVideoLocked() {
	if p.videoInit != nil || len(p.videoPS) < 20 {
		return
	}
	// ExtractNalusOfTypeFromByteStream expects 3-byte start codes (0x00 0x00 0x01).
	psBuf := annexB4To3ForPSExtract(p.videoPS)
	spss := avc.ExtractNalusOfTypeFromByteStream(avc.NALU_SPS, psBuf, false)
	ppss := avc.ExtractNalusOfTypeFromByteStream(avc.NALU_PPS, psBuf, false)
	if len(spss) == 0 || len(ppss) == 0 {
		return
	}

	init := mp4.CreateEmptyInit()
	trak := init.AddEmptyTrack(dashVideoTimescale, "video", "und")
	if err := trak.SetAVCDescriptor("avc1", spss, ppss, true); err != nil {
		slog.Error("publisher: DASH SetAVCDescriptor failed",
			"stream_code", p.streamID, "err", err)
		return
	}

	if sps, err := avc.ParseSPSNALUnit(spss[0], false); err == nil && sps != nil {
		p.width = int(sps.Width)
		p.height = int(sps.Height)
		p.videoCodec = avc.CodecString("avc1", sps)
	}

	p.videoInit = init
	p.videoTID = trak.Tkhd.TrackID
	p.videoPS = nil // no longer needed

	if err := encodeInitToFile(filepath.Join(p.streamDir, "init_v.mp4"), init); err != nil {
		slog.Error("publisher: DASH write init_v.mp4 failed",
			"stream_code", p.streamID, "err", err)
		p.videoInit = nil
	}
}

// tryInitVideoH265Locked scans videoPS for VPS/SPS/PPS and, when found, writes init_v.mp4.
// Caller must hold p.mu.
func (p *dashFMP4Packager) tryInitVideoH265Locked() {
	if p.videoInit != nil || len(p.videoPS) < 20 {
		return
	}
	psBuf := annexB4To3ForPSExtract(p.videoPS)
	vpss, spss, ppss := hevc.GetParameterSetsFromByteStream(psBuf)
	if len(spss) == 0 || len(ppss) == 0 {
		return
	}

	init := mp4.CreateEmptyInit()
	trak := init.AddEmptyTrack(dashVideoTimescale, "video", "und")
	if err := trak.SetHEVCDescriptor("hvc1", vpss, spss, ppss, nil, true); err != nil {
		slog.Error("publisher: DASH SetHEVCDescriptor failed",
			"stream_code", p.streamID, "err", err)
		return
	}

	if sps, err := hevc.ParseSPSNALUnit(spss[0]); err == nil && sps != nil {
		w, h := sps.ImageSize()
		p.width = int(w)
		p.height = int(h)
		p.videoCodec = hevc.CodecString("hvc1", sps)
	}

	p.videoInit = init
	p.videoTID = trak.Tkhd.TrackID
	p.videoPS = nil

	if err := encodeInitToFile(filepath.Join(p.streamDir, "init_v.mp4"), init); err != nil {
		slog.Error("publisher: DASH write init_v.mp4 (H.265) failed",
			"stream_code", p.streamID, "err", err)
		p.videoInit = nil
	}
}

// buildAudioInitLocked creates and writes init_a.mp4 from the first ADTS header.
// Caller must hold p.mu.
func (p *dashFMP4Packager) buildAudioInitLocked(hdr *aac.ADTSHeader) error {
	sr := int(hdr.Frequency())
	if sr <= 0 {
		return fmt.Errorf("invalid AAC sample rate")
	}
	init := mp4.CreateEmptyInit()
	trak := init.AddEmptyTrack(uint32(sr), "audio", "und")
	if err := trak.SetAACDescriptor(aac.AAClc, sr); err != nil {
		return err
	}
	p.audioInit = init
	p.audioTID = trak.Tkhd.TrackID
	p.audioSR = sr
	p.audioNextDecode = 0
	p.audioCodec = "mp4a.40.2"

	if err := encodeInitToFile(filepath.Join(p.streamDir, "init_a.mp4"), init); err != nil {
		p.audioInit = nil
		return err
	}
	return nil
}

// audioFramesPerSegment returns the expected number of 1024-sample AAC frames per segment.
func (p *dashFMP4Packager) audioFramesPerSegment() int {
	if p.audioSR <= 0 {
		return 94 // safe default for 48 kHz × 2 s
	}
	return (p.segSec*p.audioSR + 1023) / 1024
}

// queuedVideoStartsWithIDRLocked returns true when the first queued AU is an IDR/IRAP.
// Caller must hold p.mu.
func (p *dashFMP4Packager) queuedVideoStartsWithIDRLocked() bool {
	if len(p.vAnnex) == 0 {
		return false
	}
	if p.isHEVC {
		return tsmux.KeyFrameH265(p.vAnnex[0])
	}
	return tsmux.KeyFrameH264(p.vAnnex[0])
}

// effectiveVideoBW returns the video bandwidth for MPD representations.
func (p *dashFMP4Packager) effectiveVideoBW() int {
	if p.videoBW > 0 {
		return p.videoBW
	}
	return 5_000_000
}

// flushSegmentLocked writes all queued video and audio frames as .m4s segments,
// trims old segments from disk, and updates the manifest.
// Caller must hold p.mu.
func (p *dashFMP4Packager) flushSegmentLocked() error {
	flushed := false

	if p.videoInit != nil && len(p.vAnnex) > 0 {
		if err := p.writeVideoSegmentLocked(); err != nil {
			slog.Warn("publisher: DASH write video segment",
				"stream_code", p.streamID, "err", err)
		} else {
			flushed = true
		}
	}

	if p.audioInit != nil && len(p.aRaw) > 0 {
		if err := p.writeAudioSegmentLocked(); err != nil {
			slog.Warn("publisher: DASH write audio segment",
				"stream_code", p.streamID, "err", err)
		} else {
			flushed = true
		}
	}

	// Reset queues.
	p.vAnnex = p.vAnnex[:0]
	p.vDTS = p.vDTS[:0]
	p.vPTS = p.vPTS[:0]
	p.aRaw = p.aRaw[:0]
	p.aPTS = p.aPTS[:0]
	p.segmentStart = time.Now()

	if !flushed {
		return nil
	}
	if p.availabilityStart.IsZero() {
		p.availabilityStart = time.Now()
	}
	p.trimDiskLocked()
	return p.writeManifestLocked()
}

// writeVideoSegmentLocked packages all queued H.264 AUs into one fMP4 .m4s file.
// Caller must hold p.mu.
func (p *dashFMP4Packager) writeVideoSegmentLocked() error {
	p.vSegN++
	name := fmt.Sprintf("seg_v_%05d.m4s", p.vSegN)
	path := filepath.Join(p.streamDir, name)

	frag, err := mp4.CreateFragment(uint32(p.vSegN), p.videoTID)
	if err != nil {
		p.vSegN--
		return fmt.Errorf("create video fragment: %w", err)
	}

	segStart := p.videoNextDecode
	var segDur uint64

	for i, au := range p.vAnnex {
		var avcc []byte
		if p.isHEVC {
			avcc = hevcAnnexBToAVCC(au)
		} else {
			avcc = h264AnnexBToAVCC(au)
		}
		if len(avcc) == 0 {
			continue
		}

		// Sample duration in 90 kHz ticks from inter-frame DTS delta.
		var dur uint32
		if i+1 < len(p.vDTS) {
			if d := p.vDTS[i+1] - p.vDTS[i]; d > 0 {
				dur = uint32(d * 90)
			}
		}
		if dur == 0 {
			// Fallback: spread total queued duration evenly.
			if len(p.vDTS) >= 2 {
				total := (p.vDTS[len(p.vDTS)-1] - p.vDTS[0]) * 90
				dur = uint32(total / uint64(len(p.vAnnex)))
			}
			if dur == 0 {
				dur = uint32(p.segSec) * dashVideoTimescale / uint32(len(p.vAnnex))
			}
		}

		flags := mp4.NonSyncSampleFlags
		isKey := p.isHEVC && tsmux.KeyFrameH265(au) || !p.isHEVC && tsmux.KeyFrameH264(au)
		if isKey {
			flags = mp4.SyncSampleFlags
		}

		var cto int32
		if i < len(p.vPTS) && i < len(p.vDTS) {
			cto = int32((int64(p.vPTS[i]) - int64(p.vDTS[i])) * 90)
		}

		frag.AddFullSample(mp4.FullSample{
			Sample: mp4.Sample{
				Flags:                 flags,
				Dur:                   dur,
				Size:                  uint32(len(avcc)),
				CompositionTimeOffset: cto,
			},
			DecodeTime: p.videoNextDecode + segDur,
			Data:       avcc,
		})
		segDur += uint64(dur)
	}

	if segDur == 0 {
		p.vSegN--
		return nil
	}

	seg := mp4.NewMediaSegment()
	seg.AddFragment(frag)
	var buf bytes.Buffer
	if err := seg.Encode(&buf); err != nil {
		p.vSegN--
		return fmt.Errorf("encode video segment: %w", err)
	}
	if err := writeFileAtomic(path, buf.Bytes()); err != nil {
		p.vSegN--
		return fmt.Errorf("write video segment: %w", err)
	}

	slog.Debug("publisher: DASH video segment",
		"stream_code", p.streamID, "segment", name,
		"frames", len(p.vAnnex), "bytes", buf.Len())

	p.onDiskV = append(p.onDiskV, name)
	p.vSegDurs = append(p.vSegDurs, segDur)
	p.vSegStarts = append(p.vSegStarts, segStart)
	p.videoNextDecode += segDur
	return nil
}

// writeAudioSegmentLocked packages all queued raw AAC frames into one fMP4 .m4s file.
// Caller must hold p.mu.
func (p *dashFMP4Packager) writeAudioSegmentLocked() error {
	p.aSegN++
	name := fmt.Sprintf("seg_a_%05d.m4s", p.aSegN)
	path := filepath.Join(p.streamDir, name)

	frag, err := mp4.CreateFragment(uint32(p.aSegN), p.audioTID)
	if err != nil {
		p.aSegN--
		return fmt.Errorf("create audio fragment: %w", err)
	}

	segStart := p.audioNextDecode
	var segDur uint64

	// Each AAC frame contains exactly 1024 samples at p.audioSR.
	const aacFrameSamples = uint32(1024)

	for _, raw := range p.aRaw {
		frag.AddFullSample(mp4.FullSample{
			Sample: mp4.Sample{
				Flags: mp4.SyncSampleFlags,
				Dur:   aacFrameSamples,
				Size:  uint32(len(raw)),
			},
			DecodeTime: p.audioNextDecode + segDur,
			Data:       raw,
		})
		segDur += uint64(aacFrameSamples)
	}

	if segDur == 0 {
		p.aSegN--
		return nil
	}

	seg := mp4.NewMediaSegment()
	seg.AddFragment(frag)
	var buf bytes.Buffer
	if err := seg.Encode(&buf); err != nil {
		p.aSegN--
		return fmt.Errorf("encode audio segment: %w", err)
	}
	if err := writeFileAtomic(path, buf.Bytes()); err != nil {
		p.aSegN--
		return fmt.Errorf("write audio segment: %w", err)
	}

	p.onDiskA = append(p.onDiskA, name)
	p.aSegDurs = append(p.aSegDurs, segDur)
	p.aSegStarts = append(p.aSegStarts, segStart)
	p.audioNextDecode += segDur
	return nil
}

// trimDiskLocked removes old segments beyond window+history.
// Caller must hold p.mu.
func (p *dashFMP4Packager) trimDiskLocked() {
	maxKeep := p.window + p.history
	if maxKeep < p.window {
		maxKeep = p.window
	}
	if !p.ephemeral {
		return
	}
	for len(p.onDiskV) > maxKeep {
		_ = os.Remove(filepath.Join(p.streamDir, p.onDiskV[0]))
		p.onDiskV = p.onDiskV[1:]
		if len(p.vSegDurs) > 0 {
			p.vSegDurs = p.vSegDurs[1:]
		}
		if len(p.vSegStarts) > 0 {
			p.vSegStarts = p.vSegStarts[1:]
		}
	}
	for len(p.onDiskA) > maxKeep {
		_ = os.Remove(filepath.Join(p.streamDir, p.onDiskA[0]))
		p.onDiskA = p.onDiskA[1:]
		if len(p.aSegDurs) > 0 {
			p.aSegDurs = p.aSegDurs[1:]
		}
		if len(p.aSegStarts) > 0 {
			p.aSegStarts = p.aSegStarts[1:]
		}
	}
}

// writeManifestLocked serialises the sliding-window MPD.
// In ABR mode it notifies the master and returns without writing a per-shard file.
// Caller must hold p.mu.
func (p *dashFMP4Packager) writeManifestLocked() error {
	if p.manifestPath == "" && p.abrMaster == nil {
		return nil
	}

	vSegs := windowTail(p.onDiskV, p.window)
	vDurs := windowTailUint64(p.vSegDurs, p.window)
	vStarts := windowTailUint64(p.vSegStarts, p.window)
	aSegs := windowTail(p.onDiskA, p.window)
	aDurs := windowTailUint64(p.aSegDurs, p.window)
	aStarts := windowTailUint64(p.aSegStarts, p.window)

	if p.abrMaster != nil {
		p.abrMaster.onShardUpdated(p)
		return nil
	}

	doc := buildMPD(
		p.availabilityStart, p.segSec, p.window,
		p.videoInit, p.videoCodec, p.effectiveVideoBW(), p.width, p.height,
		vSegs, vDurs, vStarts,
		p.audioInit, p.audioCodec, p.audioSR,
		aSegs, aDurs, aStarts,
		"",
	)
	if doc == nil {
		return nil
	}
	out, err := xml.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(p.manifestPath, append([]byte(xml.Header), out...))
}

// ─── helpers ───────────────────────────────────────────────────────────────

// totalQueuedVideoDur90k returns the total duration of queued video in 90 kHz ticks.
func totalQueuedVideoDur90k(vDTS []uint64) uint64 {
	if len(vDTS) < 2 {
		return 0
	}
	return (vDTS[len(vDTS)-1] - vDTS[0]) * 90
}

// windowTailUint64 returns the last n elements of vals.
func windowTailUint64(vals []uint64, n int) []uint64 {
	if n <= 0 || len(vals) == 0 {
		return vals
	}
	if len(vals) <= n {
		return vals
	}
	return vals[len(vals)-n:]
}

// h264AnnexBToAVCC converts an Annex-B H.264 access unit to AVCC (length-prefixed NALUs).
func h264AnnexBToAVCC(annexB []byte) []byte {
	if len(annexB) == 0 {
		return nil
	}
	avcc := avc.ConvertByteStreamToNaluSample(annexB)
	if len(avcc) > 0 {
		return avcc
	}
	// Fallback: extract NALUs manually and prefix each with a 4-byte big-endian length.
	nalus := avc.ExtractNalusFromByteStream(annexB)
	if len(nalus) == 0 {
		return nil
	}
	out := make([]byte, 0, len(nalus)*5) // 4-byte length prefix + approximate nalu size
	for _, nalu := range nalus {
		n := len(nalu)
		out = append(out, byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
		out = append(out, nalu...)
	}
	return out
}

// hevcAnnexBToAVCC converts an Annex-B H.265 access unit to length-prefixed NALUs.
func hevcAnnexBToAVCC(annexB []byte) []byte {
	if len(annexB) == 0 {
		return nil
	}
	nalus := splitAnnexBNALUs(annexB)
	if len(nalus) == 0 {
		return nil
	}
	out := make([]byte, 0, len(annexB))
	for _, nalu := range nalus {
		n := len(nalu)
		out = append(out, byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
		out = append(out, nalu...)
	}
	return out
}

// splitAnnexBNALUs splits Annex-B byte stream on 3- or 4-byte start codes.
func splitAnnexBNALUs(data []byte) [][]byte {
	var nalus [][]byte
	start := -1
	n := len(data)
	for i := 0; i < n-3; i++ {
		if data[i] == 0 && data[i+1] == 0 {
			if data[i+2] == 1 {
				if start >= 0 {
					end := i
					for end > start && data[end-1] == 0 {
						end--
					}
					if end > start {
						nalus = append(nalus, data[start:end])
					}
				}
				start = i + 3
				i += 2
			} else if data[i+2] == 0 && i+3 < n && data[i+3] == 1 {
				if start >= 0 {
					end := i
					for end > start && data[end-1] == 0 {
						end--
					}
					if end > start {
						nalus = append(nalus, data[start:end])
					}
				}
				start = i + 4
				i += 3
			}
		}
	}
	if start >= 0 && start < n {
		nalus = append(nalus, data[start:])
	}
	return nalus
}

// annexB4To3ForPSExtract replaces 4-byte Annex-B start codes with 3-byte ones
// so that mp4ff's Annex-B scanners (which use 0x00 0x00 0x01) work correctly.
func annexB4To3ForPSExtract(b []byte) []byte {
	return bytes.ReplaceAll(b, []byte{0, 0, 0, 1}, []byte{0, 0, 1})
}

// encodeInitToFile writes an mp4.InitSegment atomically to the given path.
func encodeInitToFile(path string, init *mp4.InitSegment) error {
	var buf bytes.Buffer
	if err := init.Encode(&buf); err != nil {
		return err
	}
	return writeFileAtomic(path, buf.Bytes())
}

// tsBuffer is a goroutine-safe, unbounded byte buffer implementing io.Reader and
// io.Writer. Unlike io.Pipe, Write never blocks; Read blocks only when the buffer
// is empty, and returns io.EOF once the buffer is closed and drained.
//
// This is used to decouple the packet-producer goroutine from the TS demuxer
// goroutine. With io.Pipe, a slow demuxer (holding p.mu in onTSFrame) would
// block the producer's Write, preventing it from reading new packets; the buffer
// hub then drops them with the "slow consumer" path, starving the segment queue.
type tsBuffer struct {
	mu   sync.Mutex
	cond *sync.Cond
	buf  []byte
	done bool
}

func newTSBuffer() *tsBuffer {
	b := &tsBuffer{}
	b.cond = sync.NewCond(&b.mu)
	return b
}

// Write appends p to the internal buffer and wakes any blocked Read. Never blocks.
func (b *tsBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	if b.done {
		b.mu.Unlock()
		return 0, io.ErrClosedPipe
	}
	b.buf = append(b.buf, p...)
	b.cond.Signal()
	b.mu.Unlock()
	return len(p), nil
}

// Read fills p from the internal buffer, blocking until data is available or
// the buffer is closed.
func (b *tsBuffer) Read(p []byte) (int, error) {
	b.mu.Lock()
	for len(b.buf) == 0 && !b.done {
		b.cond.Wait()
	}
	if len(b.buf) == 0 {
		b.mu.Unlock()
		return 0, io.EOF
	}
	n := copy(p, b.buf)
	b.buf = b.buf[n:]
	b.mu.Unlock()
	return n, nil
}

// Close marks the buffer as done, unblocking any blocked Read.
func (b *tsBuffer) Close() {
	b.mu.Lock()
	b.done = true
	b.cond.Broadcast()
	b.mu.Unlock()
}

// parseDashSegNum extracts the segment number from "seg_v_00042.m4s" / "seg_a_00012.m4s".
func parseDashSegNum(kind byte, name string) int {
	prefix := "seg_" + string(kind) + "_"
	if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".m4s") {
		return 0
	}
	s := strings.TrimSuffix(strings.TrimPrefix(name, prefix), ".m4s")
	n, _ := strconv.Atoi(s)
	return n
}

// buildSegTimeline constructs a SegmentTimeline from a window of (name, dur, start) triples.
// Only the first entry gets an explicit @t attribute; the rest are implied.
func buildSegTimeline(segs []string, durs []uint64, starts []uint64) *mpdSegTimeline {
	if len(segs) == 0 {
		return nil
	}
	tl := &mpdSegTimeline{S: make([]mpdSTimeline, 0, len(segs))}
	for i := range segs {
		var d uint64
		if i < len(durs) {
			d = durs[i]
		}
		if d == 0 {
			continue
		}
		s := mpdSTimeline{D: d}
		if i == 0 && len(starts) > 0 {
			t := starts[0]
			s.T = &t
		}
		tl.S = append(tl.S, s)
	}
	return tl
}

// buildMPD creates a dynamic MPD document from the provided per-track window slices.
// baseURL is prepended to all init/media patterns (empty for per-shard MPD).
// Returns nil when there are no segments available yet.
func buildMPD(
	availStart time.Time, segSec, window int,
	videoInit *mp4.InitSegment, videoCodec string, videoBW, vW, vH int,
	vSegs []string, vDurs, vStarts []uint64,
	audioInit *mp4.InitSegment, audioCodec string, audioSR int,
	aSegs []string, aDurs, aStarts []uint64,
	baseURL string,
) *mpdRoot {
	ast := ""
	if !availStart.IsZero() {
		ast = availStart.UTC().Format(time.RFC3339)
	}
	pub := time.Now().UTC().Format(time.RFC3339)
	minBuf := max(4, segSec*2)
	sugDelay := max(6, segSec*3)
	maxSegDur := max(segSec*3, segSec+1)

	doc := &mpdRoot{
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

	if videoInit != nil && len(vSegs) > 0 {
		vTL := buildSegTimeline(vSegs, vDurs, vStarts)
		if vTL != nil {
			vStartNum := parseDashSegNum('v', vSegs[0])
			if vStartNum <= 0 {
				vStartNum = 1
			}
			initV := baseURL + "init_v.mp4"
			mediaV := baseURL + dashVideoMediaPattern
			per.AdaptationSets = append(per.AdaptationSets, mpdAdaptationSet{
				ID:               "0",
				ContentType:      "video",
				MimeType:         "video/mp4",
				SegmentAlignment: "true",
				StartWithSAP:     "1",
				Representations: []mpdRepresentation{{
					ID:        "v0",
					MimeType:  "video/mp4",
					Codecs:    videoCodec,
					Bandwidth: videoBW,
					Width:     vW,
					Height:    vH,
					SegmentTemplate: mpdSegmentTemplate{
						Timescale:      dashVideoTimescale,
						Initialization: initV,
						Media:          mediaV,
						StartNumber:    vStartNum,
						Timeline:       vTL,
					},
				}},
			})
		}
	}

	if audioInit != nil && len(aSegs) > 0 {
		aTL := buildSegTimeline(aSegs, aDurs, aStarts)
		if aTL != nil {
			aStartNum := parseDashSegNum('a', aSegs[0])
			if aStartNum <= 0 {
				aStartNum = 1
			}
			asr := audioSR
			initA := baseURL + "init_a.mp4"
			mediaA := baseURL + dashAudioMediaPattern
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
					SegmentTemplate: mpdSegmentTemplate{
						Timescale:      audioSR,
						Initialization: initA,
						Media:          mediaA,
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
	return doc
}

// ─── MPD XML structs (MPEG-DASH schema subset) ─────────────────────────────

type mpdRoot struct {
	XMLName                    xml.Name      `xml:"MPD"`
	XMLNS                      string        `xml:"xmlns,attr"`
	Type                       string        `xml:"type,attr"`
	Profiles                   string        `xml:"profiles,attr"`
	MinBuffer                  string        `xml:"minBufferTime,attr"`
	SuggestedPresentationDelay string        `xml:"suggestedPresentationDelay,attr,omitempty"`
	MaxSegmentDuration         string        `xml:"maxSegmentDuration,attr,omitempty"`
	AvailabilityStartTime      string        `xml:"availabilityStartTime,attr,omitempty"`
	MinUpdate                  string        `xml:"minimumUpdatePeriod,attr"`
	BufferDepth                string        `xml:"timeShiftBufferDepth,attr"`
	PublishTime                string        `xml:"publishTime,attr"`
	Periods                    []mpdPeriod   `xml:"Period"`
	UTCTiming                  *mpdUTCTiming `xml:"UTCTiming,omitempty"`
}

type mpdUTCTiming struct {
	SchemeIDURI string `xml:"schemeIdUri,attr"`
	Value       string `xml:"value,attr"`
}

type mpdPeriod struct {
	ID             string             `xml:"id,attr"`
	Start          string             `xml:"start,attr"`
	AdaptationSets []mpdAdaptationSet `xml:"AdaptationSet"`
}

type mpdAdaptationSet struct {
	ID               string              `xml:"id,attr"`
	ContentType      string              `xml:"contentType,attr"`
	MimeType         string              `xml:"mimeType,attr"`
	SegmentAlignment string              `xml:"segmentAlignment,attr"`
	StartWithSAP     string              `xml:"startWithSAP,attr"`
	Representations  []mpdRepresentation `xml:"Representation"`
}

type mpdRepresentation struct {
	ID                string             `xml:"id,attr"`
	MimeType          string             `xml:"mimeType,attr"`
	Codecs            string             `xml:"codecs,attr"`
	Bandwidth         int                `xml:"bandwidth,attr"`
	Width             int                `xml:"width,attr,omitempty"`
	Height            int                `xml:"height,attr,omitempty"`
	AudioSamplingRate *int               `xml:"audioSamplingRate,attr,omitempty"`
	BaseURL           string             `xml:"BaseURL,omitempty"`
	SegmentTemplate   mpdSegmentTemplate `xml:"SegmentTemplate"`
}

type mpdSegmentTemplate struct {
	Timescale      int             `xml:"timescale,attr"`
	Initialization string          `xml:"initialization,attr"`
	Media          string          `xml:"media,attr"`
	StartNumber    int             `xml:"startNumber,attr"`
	Timeline       *mpdSegTimeline `xml:"SegmentTimeline"`
}

type mpdSegTimeline struct {
	S []mpdSTimeline `xml:"S"`
}

type mpdSTimeline struct {
	T *uint64 `xml:"t,attr,omitempty"`
	D uint64  `xml:"d,attr"`
}
