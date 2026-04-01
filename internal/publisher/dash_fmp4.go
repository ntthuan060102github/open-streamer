package publisher

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
	"github.com/Eyevinn/mp4ff/mp4"
	mpeg2 "github.com/yapingcat/gomedia/go-mpeg2"

	"github.com/open-streamer/open-streamer/internal/buffer"
	"github.com/open-streamer/open-streamer/internal/domain"
)

const dashVideoTimescale = 90000

// dashSegMediaPatterns are used with SegmentTemplate $Number%05d$ (Shaka / Flussonic-style); must match writeSegment filenames.
const (
	dashVideoMediaPattern = `seg_v_$Number%05d$.m4s`
	dashAudioMediaPattern = `seg_a_$Number%05d$.m4s`
)

// parseDashSegMediaNumber returns the numeric suffix in seg_v_00054.m4s / seg_a_00012.m4s (0 if invalid).
func parseDashSegMediaNumber(kind byte, name string) int {
	prefix := "seg_" + string(kind) + "_"
	if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".m4s") {
		return 0
	}
	s := strings.TrimSuffix(strings.TrimPrefix(name, prefix), ".m4s")
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}

// runDASHFMP4Packager writes ISO BMFF init + fragmented media (.m4s) and a dynamic MPD (ISO 23009-1, isoff-live profile).
func runDASHFMP4Packager(
	ctx context.Context,
	streamID domain.StreamCode,
	sub *buffer.Subscriber,
	streamDir string,
	manifestPath string,
	segSec, window, history int,
	ephemeral bool,
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
	}

	pr, pw := io.Pipe()

	go func() {
		defer func() { _ = pw.Close() }()
		for {
			select {
			case <-ctx.Done():
				return
			case pkt, ok := <-sub.Recv():
				if !ok {
					return
				}
				if _, err := pw.Write(pkt); err != nil {
					return
				}
			}
		}
	}()

	demux := mpeg2.NewTSDemuxer()
	demux.OnFrame = p.onTSFrame

	demuxDone := make(chan error, 1)
	go func() {
		demuxDone <- demux.Input(pr)
	}()

	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			_ = pr.Close()
			<-demuxDone
			p.flushManifest()
			return
		case err := <-demuxDone:
			if err != nil && ctx.Err() == nil {
				slog.Warn("publisher: DASH TS demux ended", "stream_code", streamID, "err", err)
			}
			p.flushManifest()
			return
		case <-tick.C:
			p.mu.Lock()
			if p.videoInit == nil && p.audioInit == nil && len(p.vAnnex) == 0 && len(p.aRaw) == 0 {
				p.mu.Unlock()
				continue
			}
			wallDue := !p.segmentStart.IsZero() && time.Since(p.segmentStart) >= time.Duration(p.segSec)*time.Second
			targetV := uint64(p.segSec) * uint64(dashVideoTimescale)
			vDurQ := totalQueuedVideoDur90k(p.vDTS)
			audioNeed := 1<<30 // no audio flush by count
			if p.audioInit != nil {
				audioNeed = p.audioFramesPerSegment()
			}
			// Do not use vDurQ as a trigger before the first IDR (avoids busy-loop while skipping pre-GOP frames).
			videoByDur := p.videoInit != nil && vDurQ >= targetV &&
				(p.vSegN > 0 || p.queuedVideoStartsWithIDRLocked())
			audioOnlyByCount := p.videoInit == nil && p.audioInit != nil && len(p.aRaw) >= audioNeed
			mediaFlush := videoByDur || audioOnlyByCount
			if wallDue || mediaFlush {
				if err := p.flushSegmentLocked(); err != nil {
					slog.Warn("publisher: DASH segment flush failed", "stream_code", streamID, "err", err)
				}
			}
			p.mu.Unlock()
		}
	}
}

type dashFMP4Packager struct {
	streamID  domain.StreamCode
	streamDir string

	manifestPath string
	segSec       int
	window       int
	history      int
	ephemeral    bool

	mu sync.Mutex

	segmentStart time.Time

	// TS demux → queues
	vAnnex [][]byte // Annex-B access units
	vDTS   []uint64 // ms
	vPTS   []uint64 // ms

	aRaw [][]byte // AAC raw (no ADTS)
	aPTS []uint64 // ms ordering

	videoPS []byte // Annex-B bytes scanned for SPS/PPS

	videoInit *mp4.InitSegment
	audioInit *mp4.InitSegment
	videoTID  uint32
	audioTID  uint32
	audioSR   int

	videoCodec string
	audioCodec string
	width      int
	height     int

	hevcWarned bool

	vSegN uint64
	aSegN uint64

	// Continuous decode timeline in track timescale (Shaka/MSE require tfdt to match a coherent Period timeline; TS DTS is not aligned with audioNextDecode).
	videoNextDecode uint64 // 90 kHz
	audioNextDecode uint64 // audio mdhd timescale (sample rate)

	onDiskV []string
	onDiskA []string
	// Per-segment total duration in track timescale (video: 90kHz ticks; audio: sample_rate ticks, multiple of 1024).
	vSegDurs []uint64
	aSegDurs []uint64
	// First-sample decode time at segment start (matches tfdt in each .m4s); required on SegmentTimeline@t after window trim.
	vSegStarts []uint64
	aSegStarts []uint64

	// Set once when the first media segment exists; required for type=dynamic MPD (dash.js / ISO 23009-1).
	availabilityStart time.Time
}

func (p *dashFMP4Packager) onTSFrame(cid mpeg2.TS_STREAM_TYPE, frame []byte, pts, dts uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()

	switch cid {
	case mpeg2.TS_STREAM_AUDIO_MPEG1, mpeg2.TS_STREAM_AUDIO_MPEG2:
		// MP3 in TS — not packaged into fMP4 DASH in this packager.
		return
	case mpeg2.TS_STREAM_H264:
		if len(frame) == 0 {
			return
		}
		cp := append([]byte(nil), frame...)
		p.vAnnex = append(p.vAnnex, cp)
		p.vDTS = append(p.vDTS, dts)
		p.vPTS = append(p.vPTS, pts)
		p.videoPS = append(p.videoPS, cp...)
		p.tryInitVideoLocked()
		if p.segmentStart.IsZero() && p.videoInit != nil {
			p.segmentStart = time.Now()
		}

	case mpeg2.TS_STREAM_H265:
		if !p.hevcWarned {
			p.hevcWarned = true
			slog.Warn("publisher: DASH fMP4 packager ignores H.265 in TS for now (H.264 + AAC-LC only)",
				"stream_code", p.streamID,
			)
		}

	case mpeg2.TS_STREAM_AAC:
		if p.audioInit != nil {
			p.ingestADTSLocked(frame, pts)
			return
		}
		frames := splitADTSFrames(frame)
		for _, f := range frames {
			hdr, _, err := aac.DecodeADTSHeader(bytes.NewReader(f))
			if err != nil {
				continue
			}
			hlen := int(hdr.HeaderLength)
			if len(f) < hlen+int(hdr.PayloadLength) {
				continue
			}
			raw := append([]byte(nil), f[hlen:hlen+int(hdr.PayloadLength)]...)
			if p.audioInit == nil {
				if err := p.buildAudioInitLocked(hdr, raw); err != nil {
					continue
				}
			}
			p.aRaw = append(p.aRaw, raw)
			p.aPTS = append(p.aPTS, pts)
			if p.segmentStart.IsZero() && p.videoInit == nil && p.audioInit != nil {
				p.segmentStart = time.Now()
			}
		}
	default:
		return
	}
}

func (p *dashFMP4Packager) tryInitVideoLocked() {
	if p.videoInit != nil || len(p.videoPS) < 20 {
		return
	}
	spss, ppss := avc.GetParameterSetsFromByteStream(p.videoPS)
	if len(spss) == 0 || len(ppss) == 0 {
		return
	}
	init := mp4.CreateEmptyInit()
	trak := init.AddEmptyTrack(dashVideoTimescale, "video", "und")
	if err := trak.SetAVCDescriptor("avc1", spss, ppss, true); err != nil {
		slog.Error("publisher: DASH SetAVCDescriptor failed", "stream_code", p.streamID, "err", err)
		return
	}
	sps, err := avc.ParseSPSNALUnit(spss[0], false)
	if err == nil && sps != nil {
		p.width = int(sps.Width)
		p.height = int(sps.Height)
		p.videoCodec = avc.CodecString("avc1", sps)
	}
	p.videoInit = init
	p.videoTID = trak.Tkhd.TrackID
	path := filepath.Join(p.streamDir, "init_v.mp4")
	if err := encodeInitToFile(path, init); err != nil {
		slog.Error("publisher: DASH write init_v failed", "stream_code", p.streamID, "err", err)
		p.videoInit = nil
		return
	}
}

func (p *dashFMP4Packager) buildAudioInitLocked(hdr *aac.ADTSHeader, firstRaw []byte) error {
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
	path := filepath.Join(p.streamDir, "init_a.mp4")
	if err := encodeInitToFile(path, init); err != nil {
		p.audioInit = nil
		return err
	}
	_ = firstRaw
	return nil
}

func (p *dashFMP4Packager) ingestADTSLocked(frame []byte, pts uint64) {
	for _, f := range splitADTSFrames(frame) {
		hdr, _, err := aac.DecodeADTSHeader(bytes.NewReader(f))
		if err != nil {
			continue
		}
		hlen := int(hdr.HeaderLength)
		pl := int(hdr.PayloadLength)
		if len(f) < hlen+pl {
			continue
		}
		raw := append([]byte(nil), f[hlen:hlen+pl]...)
		p.aRaw = append(p.aRaw, raw)
		p.aPTS = append(p.aPTS, pts)
	}
}

func splitADTSFrames(data []byte) [][]byte {
	var out [][]byte
	i := 0
	for i+7 <= len(data) {
		hdr, off, err := aac.DecodeADTSHeader(bytes.NewReader(data[i:]))
		if err != nil {
			i++
			continue
		}
		i += off
		total := int(hdr.HeaderLength) + int(hdr.PayloadLength)
		if i+total > len(data) {
			break
		}
		out = append(out, data[i:i+total])
		i += total
	}
	return out
}

func encodeInitToFile(path string, init *mp4.InitSegment) error {
	var buf bytes.Buffer
	if err := init.Encode(&buf); err != nil {
		return err
	}
	return writeFileAtomic(path, buf.Bytes())
}

func h264SampleDur90k(dts []uint64, i int) uint32 {
	dur := uint32(dashVideoTimescale / 30)
	if i+1 < len(dts) {
		delta := int64(dts[i+1]) - int64(dts[i])
		if delta > 0 {
			dur = uint32(delta * 90)
		}
	} else if i > 0 {
		delta := int64(dts[i]) - int64(dts[i-1])
		if delta > 0 {
			dur = uint32(delta * 90)
		}
	}
	if dur < 1 {
		dur = 1
	}
	return dur
}

func totalQueuedVideoDur90k(dts []uint64) uint64 {
	var sum uint64
	for i := range dts {
		sum += uint64(h264SampleDur90k(dts, i))
	}
	return sum
}

func (p *dashFMP4Packager) queuedVideoStartsWithIDRLocked() bool {
	if len(p.vAnnex) == 0 {
		return false
	}
	avcc := avc.ConvertByteStreamToNaluSample(p.vAnnex[0])
	return len(avcc) > 0 && avc.IsIDRSample(avcc)
}

// takeVideoFlushChunkLocked removes one chunk of Annex-B access units whose summed sample duration
// is at least targetTicks (90 kHz), or all queued units when wallDue is true. The first video
// segment starts at an IDR. Returns ok=false if nothing can be emitted yet.
func (p *dashFMP4Packager) takeVideoFlushChunkLocked(targetTicks uint64, wallDue bool) (annex [][]byte, dts []uint64, pts []uint64, totalDur uint64, ok bool) {
	if p.videoInit == nil || len(p.vAnnex) == 0 {
		return nil, nil, nil, 0, false
	}
	if p.vSegN == 0 {
		skip := 0
		for skip < len(p.vAnnex) {
			avcc := avc.ConvertByteStreamToNaluSample(p.vAnnex[skip])
			if len(avcc) > 0 && avc.IsIDRSample(avcc) {
				break
			}
			skip++
		}
		if skip >= len(p.vAnnex) {
			return nil, nil, nil, 0, false
		}
		if skip > 0 {
			p.vAnnex = append([][]byte(nil), p.vAnnex[skip:]...)
			p.vDTS = append([]uint64(nil), p.vDTS[skip:]...)
			p.vPTS = append([]uint64(nil), p.vPTS[skip:]...)
		}
	}
	n := len(p.vAnnex)
	var sum uint64
	end := 0
	for end < n {
		dur := uint64(h264SampleDur90k(p.vDTS, end))
		sum += dur
		end++
		if sum >= targetTicks {
			break
		}
	}
	if sum < targetTicks {
		if !wallDue {
			return nil, nil, nil, 0, false
		}
		end = n
		sum = totalQueuedVideoDur90k(p.vDTS)
		if end == 0 || sum == 0 {
			return nil, nil, nil, 0, false
		}
	}

	annex = append([][]byte(nil), p.vAnnex[:end]...)
	dts = append([]uint64(nil), p.vDTS[:end]...)
	pts = append([]uint64(nil), p.vPTS[:end]...)
	p.vAnnex = append([][]byte(nil), p.vAnnex[end:]...)
	p.vDTS = append([]uint64(nil), p.vDTS[end:]...)
	p.vPTS = append([]uint64(nil), p.vPTS[end:]...)
	return annex, dts, pts, sum, true
}

func (p *dashFMP4Packager) audioFramesPerSegment() int {
	if p.audioSR <= 0 {
		return 1
	}
	n := (p.audioSR*p.segSec + 1024 - 1) / 1024
	if n < 1 {
		n = 1
	}
	return n
}

// takeAudioFlushChunkLocked removes up to audioFramesPerSegment() AAC frames, or all queued frames
// when allowPartial is true and fewer are available.
func (p *dashFMP4Packager) takeAudioFlushChunkLocked(allowPartial bool) (raw [][]byte, totalDur uint64, ok bool) {
	if p.audioInit == nil || len(p.aRaw) == 0 {
		return nil, 0, false
	}
	want := p.audioFramesPerSegment()
	if len(p.aRaw) < want {
		if !allowPartial {
			return nil, 0, false
		}
		want = len(p.aRaw)
	}
	raw = append([][]byte(nil), p.aRaw[:want]...)
	p.aRaw = append([][]byte(nil), p.aRaw[want:]...)
	p.aPTS = append([]uint64(nil), p.aPTS[want:]...)
	totalDur = uint64(want * 1024)
	return raw, totalDur, true
}

func (p *dashFMP4Packager) flushSegmentLocked() error {
	if p.videoInit == nil && p.audioInit == nil {
		return nil
	}

	wallDue := !p.segmentStart.IsZero() && time.Since(p.segmentStart) >= time.Duration(p.segSec)*time.Second
	targetV := uint64(p.segSec) * uint64(dashVideoTimescale)
	vDurQ := totalQueuedVideoDur90k(p.vDTS)
	videoByDur := p.videoInit != nil && vDurQ >= targetV &&
		(p.vSegN > 0 || p.queuedVideoStartsWithIDRLocked())
	audioOnlyByCount := p.videoInit == nil && p.audioInit != nil &&
		len(p.aRaw) >= p.audioFramesPerSegment()
	should := wallDue || videoByDur || audioOnlyByCount
	if !should {
		return nil
	}

	audioAllowPartial := wallDue
	vAnnex, vDts, vPts, vTotalDur, vOk := p.takeVideoFlushChunkLocked(targetV, wallDue)
	if vOk {
		audioAllowPartial = true
	}
	var aRaw [][]byte
	var aTotalDur uint64
	var aOk bool
	// Avoid emitting audio-only segments while H.264 is waiting for the first IDR.
	if p.videoInit == nil || vOk || wallDue {
		aRaw, aTotalDur, aOk = p.takeAudioFlushChunkLocked(audioAllowPartial)
	}

	if !vOk && !aOk {
		if wallDue {
			p.segmentStart = time.Now()
		}
		return nil
	}

	if vOk {
		nextV := p.vSegN + 1
		name := fmt.Sprintf("seg_v_%05d.m4s", nextV)
		if err := p.writeVideoSegmentLocked(name, uint32(nextV), vAnnex, vDts, vPts); err != nil {
			return err
		}
		p.vSegN = nextV
		p.onDiskV = append(p.onDiskV, name)
		p.vSegDurs = append(p.vSegDurs, vTotalDur)
	}
	if aOk {
		nextA := p.aSegN + 1
		name := fmt.Sprintf("seg_a_%05d.m4s", nextA)
		if err := p.writeAudioSegmentLocked(name, uint32(nextA), aRaw); err != nil {
			return err
		}
		p.aSegN = nextA
		p.onDiskA = append(p.onDiskA, name)
		p.aSegDurs = append(p.aSegDurs, aTotalDur)
	}

	p.trimDiskLocked()
	p.segmentStart = time.Now()
	if p.availabilityStart.IsZero() && (len(p.onDiskV) > 0 || len(p.onDiskA) > 0) {
		p.availabilityStart = time.Now().UTC()
	}
	return p.writeManifestLocked()
}

func (p *dashFMP4Packager) writeVideoSegmentLocked(baseName string, seqNr uint32, annex [][]byte, dts, pts []uint64) error {
	segStartDecode := p.videoNextDecode
	nextDecode := segStartDecode
	frag, err := mp4.CreateFragment(seqNr, p.videoTID)
	if err != nil {
		return err
	}
	for i := range annex {
		avcc := avc.ConvertByteStreamToNaluSample(annex[i])
		if len(avcc) == 0 {
			continue
		}
		sync := avc.IsIDRSample(avcc)
		flags := mp4.NonSyncSampleFlags
		if sync {
			flags = mp4.SyncSampleFlags
		}
		dur := h264SampleDur90k(dts, i)
		cto := int32((pts[i] - dts[i]) * 90)
		dec := nextDecode
		nextDecode += uint64(dur)
		fs := mp4.FullSample{
			Sample: mp4.Sample{
				Flags:                 flags,
				Dur:                   dur,
				Size:                  uint32(len(avcc)),
				CompositionTimeOffset: cto,
			},
			DecodeTime: dec,
			Data:       avcc,
		}
		frag.AddFullSample(fs)
	}
	frag.EncOptimize = mp4.OptimizeTrun
	seg := mp4.NewMediaSegment()
	seg.AddFragment(frag)
	var buf bytes.Buffer
	if err := seg.Encode(&buf); err != nil {
		return err
	}
	if err := writeFileAtomic(filepath.Join(p.streamDir, baseName), buf.Bytes()); err != nil {
		return err
	}
	p.videoNextDecode = nextDecode
	p.vSegStarts = append(p.vSegStarts, segStartDecode)
	return nil
}

func (p *dashFMP4Packager) writeAudioSegmentLocked(baseName string, seqNr uint32, raw [][]byte) error {
	segStartDecode := p.audioNextDecode
	nextDecode := segStartDecode
	frag, err := mp4.CreateFragment(seqNr, p.audioTID)
	if err != nil {
		return err
	}
	const aacDur = 1024
	for _, r := range raw {
		fs := mp4.FullSample{
			Sample: mp4.Sample{
				Flags:                 mp4.SyncSampleFlags,
				Dur:                   aacDur,
				Size:                  uint32(len(r)),
				CompositionTimeOffset: 0,
			},
			DecodeTime: nextDecode,
			Data:       r,
		}
		nextDecode += aacDur
		frag.AddFullSample(fs)
	}
	frag.EncOptimize = mp4.OptimizeTrun
	seg := mp4.NewMediaSegment()
	seg.AddFragment(frag)
	var buf bytes.Buffer
	if err := seg.Encode(&buf); err != nil {
		return err
	}
	if err := writeFileAtomic(filepath.Join(p.streamDir, baseName), buf.Bytes()); err != nil {
		return err
	}
	p.audioNextDecode = nextDecode
	p.aSegStarts = append(p.aSegStarts, segStartDecode)
	return nil
}

func (p *dashFMP4Packager) trimDiskLocked() {
	maxKeep := p.window + p.history
	if maxKeep < p.window {
		maxKeep = p.window
	}
	if !p.ephemeral {
		return
	}
	for len(p.onDiskV) > maxKeep {
		old := p.onDiskV[0]
		p.onDiskV = p.onDiskV[1:]
		if len(p.vSegDurs) > 0 {
			p.vSegDurs = p.vSegDurs[1:]
		}
		if len(p.vSegStarts) > 0 {
			p.vSegStarts = p.vSegStarts[1:]
		}
		_ = os.Remove(filepath.Join(p.streamDir, old))
	}
	for len(p.onDiskA) > maxKeep {
		old := p.onDiskA[0]
		p.onDiskA = p.onDiskA[1:]
		if len(p.aSegDurs) > 0 {
			p.aSegDurs = p.aSegDurs[1:]
		}
		if len(p.aSegStarts) > 0 {
			p.aSegStarts = p.aSegStarts[1:]
		}
		_ = os.Remove(filepath.Join(p.streamDir, old))
	}
}

func (p *dashFMP4Packager) flushManifest() {
	p.mu.Lock()
	defer p.mu.Unlock()
	_ = p.writeManifestLocked()
}

func windowTailUint64(vals []uint64, n int) []uint64 {
	if n <= 0 || len(vals) == 0 {
		return vals
	}
	if len(vals) <= n {
		return vals
	}
	return vals[len(vals)-n:]
}

func (p *dashFMP4Packager) writeManifestLocked() error {
	if p.manifestPath == "" {
		return nil
	}

	ast := ""
	if !p.availabilityStart.IsZero() {
		ast = p.availabilityStart.UTC().Format(time.RFC3339)
	}
	pub := time.Now().UTC().Format(time.RFC3339)
	minBuf := max(4, p.segSec*2)
	sugDelay := max(6, p.segSec*3)
	maxSegDur := max(p.segSec*3, p.segSec+1)
	doc := mpdRoot{
		XMLName:                    xml.Name{Local: "MPD"},
		XMLNS:                      "urn:mpeg:dash:schema:mpd:2011",
		Type:                       "dynamic",
		Profiles:                   "urn:mpeg:dash:profile:isoff-live:2011",
		MinBuffer:                  fmt.Sprintf("PT%dS", minBuf),
		SuggestedPresentationDelay: fmt.Sprintf("PT%dS", sugDelay),
		MaxSegmentDuration:         fmt.Sprintf("PT%dS", maxSegDur),
		AvailabilityStartTime:      ast,
		MinUpdate:                  fmt.Sprintf("PT%dS", p.segSec),
		BufferDepth:                fmt.Sprintf("PT%dS", p.segSec*p.window),
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

	vSegs := windowTail(p.onDiskV, p.window)
	vDurs := windowTailUint64(p.vSegDurs, p.window)
	vStarts := windowTailUint64(p.vSegStarts, p.window)
	if p.videoInit != nil && len(vSegs) > 0 {
		vTL := &mpdSegTimeline{S: make([]mpdSTimeline, 0, len(vSegs))}
		for i := range vSegs {
			d := uint64(p.segSec) * uint64(dashVideoTimescale)
			if i < len(vDurs) && vDurs[i] > 0 {
				d = vDurs[i]
			}
			st := mpdSTimeline{D: d}
			if i == 0 && len(vStarts) > 0 {
				t0 := vStarts[0]
				st.T = &t0
			}
			vTL.S = append(vTL.S, st)
		}
		vStartNum := parseDashSegMediaNumber('v', vSegs[0])
		if vStartNum <= 0 {
			vStartNum = 1
		}
		per.AdaptationSets = append(per.AdaptationSets, mpdAdaptationSet{
			ID:               "0",
			ContentType:      "video",
			MimeType:         "video/mp4",
			SegmentAlignment: "true",
			StartWithSAP:     "1",
			Representations: []mpdRepresentation{{
				ID:        "v0",
				MimeType:  "video/mp4",
				Codecs:    p.videoCodec,
				Bandwidth: 5_000_000,
				Width:     p.width,
				Height:    p.height,
				SegmentTemplate: mpdSegmentTemplate{
					Timescale:      dashVideoTimescale,
					Initialization: "init_v.mp4",
					Media:          dashVideoMediaPattern,
					StartNumber:    vStartNum,
					Timeline:       vTL,
				},
			}},
		})
	}

	aSegs := windowTail(p.onDiskA, p.window)
	aDurs := windowTailUint64(p.aSegDurs, p.window)
	aStarts := windowTailUint64(p.aSegStarts, p.window)
	if p.audioInit != nil && len(aSegs) > 0 {
		defADur := uint64(p.audioFramesPerSegment() * 1024)
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
		asr := p.audioSR
		per.AdaptationSets = append(per.AdaptationSets, mpdAdaptationSet{
			ID:               "1",
			ContentType:      "audio",
			MimeType:         "audio/mp4",
			SegmentAlignment: "true",
			StartWithSAP:     "1",
			Representations: []mpdRepresentation{{
				ID:                "a0",
				MimeType:          "audio/mp4",
				Codecs:            p.audioCodec,
				Bandwidth:         128_000,
				AudioSamplingRate: &asr,
				SegmentTemplate: mpdSegmentTemplate{
					Timescale:      p.audioSR,
					Initialization: "init_a.mp4",
					Media:          dashAudioMediaPattern,
					StartNumber:    aStartNum,
					Timeline:       aTL,
				},
			}},
		})
	}

	if len(per.AdaptationSets) == 0 {
		return nil
	}

	out, err := xml.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	hdr := []byte(xml.Header)
	return writeFileAtomic(p.manifestPath, append(hdr, out...))
}

// MPD XML structs (MPEG-DASH schema subset).
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
	ID                  string             `xml:"id,attr"`
	MimeType            string             `xml:"mimeType,attr"`
	Codecs              string             `xml:"codecs,attr"`
	Bandwidth           int                `xml:"bandwidth,attr"`
	Width               int                `xml:"width,attr,omitempty"`
	Height              int                `xml:"height,attr,omitempty"`
	AudioSamplingRate   *int               `xml:"audioSamplingRate,attr,omitempty"`
	SegmentTemplate     mpdSegmentTemplate `xml:"SegmentTemplate"`
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
