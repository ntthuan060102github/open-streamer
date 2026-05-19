package push

// rtmp_writer.go — bridges the publisher-side TS-demux pipeline to a lal
// `*rtmp.ServerSession` for serving external RTMP play clients.
//
// The publisher (internal/publisher/serve_rtmp.go) reads from the Buffer
// Hub, demuxes the MPEG-TS bytes via gomedia's TSDemuxer, and emits one
// callback per H.264 / AAC access unit. Per-access-unit data is in:
//
//   - H.264: Annex-B start codes, SPS/PPS prepended on IDR (rtmp.go's
//     handling guarantees this on the ingest side; the same shape is
//     preserved through the buffer hub).
//   - AAC:   ADTS-prefixed bytes (7-byte header + raw frame).
//
// To send those to the lal session, we wrap each access unit into the
// FLV tag format the RTMP wire protocol expects (the same format lal's
// own demuxer parses on the receive side):
//
//   - First H.264 frame: derive SPS/PPS from Annex-B, build an AVC
//     sequence header tag (FLV PacketType=0 = AVCDecoderConfigurationRecord),
//     send.
//   - Subsequent H.264: convert Annex-B → AVCC, build a NALU tag (PacketType=1).
//   - First AAC frame:   parse ADTS header → AscContext, build an AAC
//     sequence header tag (PacketType=0 = AudioSpecificConfig), send.
//   - Subsequent AAC: strip ADTS header, build raw frame tag (PacketType=1).
//
// Sequence headers must precede NALU / raw audio frames otherwise the
// receiving player can't initialise its decoder. The writer enforces
// this by latching `seqSent` flags on first frame.
//
// strict players also expect:
//
//   - An `onMetaData` AMF0 script tag with codec IDs *before* any AV data.
//     Without it they refuse to play with "unknown stream" — even if all
//     subsequent video / audio tags are valid. Lenient players (LAL pull,
//     preview clients) tolerate the omission, which is why the bug went
//     unnoticed against open-streamer ↔ open-streamer.
//   - Sequence headers at timestamp 0, not at the first frame's DTS.
//     The wire timestamp on the seq header is informational; players
//     timestamp-sort the GOP and a non-zero seq-header timestamp pushes
//     it past the first NALU, breaking decoder init.
//
// To satisfy them, we always emit:
//
//   1. onMetaData (CSID 5, timestamp 0) on first WriteFrame call.
//   2. AVC seq header (CSID 7, timestamp 0) on first H.264 frame.
//   3. AAC seq header (CSID 6, timestamp 0) on first AAC frame.
//   4. NALU / raw audio (CSIDs 7/6) at their actual DTS.

import (
	"bytes"
	"fmt"
	"log/slog"

	eyevinnaac "github.com/Eyevinn/mp4ff/aac"
	"github.com/q191201771/lal/pkg/aac"
	"github.com/q191201771/lal/pkg/avc"
	"github.com/q191201771/lal/pkg/base"
	"github.com/q191201771/lal/pkg/rtmp"
)

// FrameKind classifies an access unit so the writer doesn't depend on
// the source TS demuxer's codec enum.
type FrameKind int

// FrameKind values.
const (
	FrameKindUnknown FrameKind = iota
	FrameKindH264
	FrameKindAAC
)

// adtsHeaderLength is the standard 7-byte ADTS header (no CRC). LAL
// emits this length on the ingest side; the demuxer downstream of buffer
// hub does too. Frames shorter than this are malformed and dropped.
const adtsHeaderLength = 7

// RTMPFrameWriter sends one access unit at a time to a lal ServerSession,
// emitting onMetaData and AVC / AAC sequence headers automatically on
// the first frame of each codec.
type RTMPFrameWriter struct {
	session *rtmp.ServerSession

	width      uint32 // parsed from SPS on first IDR; 0 until then
	height     uint32
	avcSeqSent bool
	aacSeqSent bool
}

// NewRTMPFrameWriter wraps session for per-frame writes.
func NewRTMPFrameWriter(session *rtmp.ServerSession) *RTMPFrameWriter {
	return &RTMPFrameWriter{session: session}
}

// PreloadAvcSeqHeader builds + sends the AVC sequence header tag using the
// supplied SPS/PPS NAL bytes (Annex-B form, no start codes), bypassing the
// usual "wait for first frame containing SPS+PPS" path in writeH264.
//
// Use case: the publisher's H264 frame source can't deliver SPS/PPS in
// the access unit (e.g. gomedia's TSDemuxer strips parameter-set NAL
// units before invoking OnFrame, so the writer never sees them in the
// per-frame Annex-B). Without this preload, writeH264 drops every frame
// silently waiting for SPS that never arrives, and downstream RTMP
// clients (e.g. test5's lal puller) never receive the seq header tag,
// leaving their AVCDecoderConfigurationRecord cache empty — every
// subsequent IDR they propagate downstream lacks SPS/PPS too.
//
// Caller must extract SPS+PPS from elsewhere (e.g. raw TS PES scan in
// the publisher's producer goroutine) and invoke this BEFORE the first
// WriteFrame so onMetaData and seq header tags land at timestamp 0.
//
// Idempotent: subsequent calls are no-ops once avcSeqSent is true.
func (w *RTMPFrameWriter) PreloadAvcSeqHeader(sps, pps []byte) error {
	if w.avcSeqSent {
		slog.Debug("rtmp writer: PreloadAvcSeqHeader skipped (seq already sent)")
		return nil
	}
	if len(sps) == 0 || len(pps) == 0 {
		slog.Debug("rtmp writer: PreloadAvcSeqHeader skipped (empty SPS/PPS)",
			"sps_size", len(sps), "pps_size", len(pps))
		return nil
	}
	var ctx avc.Context
	if err := avc.ParseSps(sps, &ctx); err == nil {
		w.width, w.height = ctx.Width, ctx.Height
	}
	if err := w.sendMetadata(); err != nil {
		return err
	}
	seqTag, err := avc.BuildSeqHeaderFromSpsPps(sps, pps)
	if err != nil {
		return fmt.Errorf("rtmp writer: build avc seq header (preload): %w", err)
	}
	if err := w.send(base.RtmpTypeIdVideo, 0, seqTag); err != nil {
		return err
	}
	w.avcSeqSent = true
	slog.Info("rtmp writer: AVC seq header sent via preload",
		"sps_size", len(sps), "pps_size", len(pps),
		"width", w.width, "height", w.height,
		"tag_size", len(seqTag))
	return nil
}

// WriteFrame sends one access unit. dts/pts are RTMP wire timestamps in
// milliseconds. Returns an error if the underlying TCP write fails.
//
// Audio frames received before the AVC sequence header (and onMetaData)
// have been sent are dropped: strict players parse onMetaData first to
// pick the audio decoder, and tags arriving before it are at best
// ignored, at worst cause re-buffering / resync. The publisher already
// holds audio until the first video keyframe so this drop is rare.
func (w *RTMPFrameWriter) WriteFrame(kind FrameKind, data []byte, pts, dts uint32) error {
	if len(data) == 0 {
		return nil
	}
	switch kind {
	case FrameKindH264:
		return w.writeH264(data, pts, dts)
	case FrameKindAAC:
		if !w.avcSeqSent {
			return nil
		}
		return w.writeAAC(data, dts)
	case FrameKindUnknown:
		return nil
	}
	return nil
}

// sendMetadata emits the onMetaData AMF0 script tag at timestamp 0,
// wrapped in `@setDataFrame`. Open Streamer's RTMP play path only
// supports H.264 + AAC so the codec IDs are hardcoded.
//
// `@setDataFrame` is an RTMP NetStream command that flags the AMF
// payload as cacheable stream metadata (vs. a one-shot data event).
// strict players ignore raw
// onMetaData tags lacking this prefix — they treat them as opaque
// data messages, never parse codec/resolution from them, then time
// out the play session ~7-8s later when their decoder still has no
// init params. Lenient players (LAL pull, ffplay) tolerate the
// omission, which is why the issue only surfaced against strict players.
func (w *RTMPFrameWriter) sendMetadata() error {
	width, height := -1, -1
	if w.width > 0 && w.height > 0 {
		width = int(w.width)   //nolint:gosec // SPS width fits in int on every platform we target
		height = int(w.height) //nolint:gosec
	}
	meta, err := rtmp.BuildMetadata(width, height, int(base.RtmpSoundFormatAac), int(base.RtmpCodecIdAvc))
	if err != nil {
		return fmt.Errorf("rtmp writer: build metadata: %w", err)
	}
	meta, err = rtmp.MetadataEnsureWithSdf(meta)
	if err != nil {
		return fmt.Errorf("rtmp writer: wrap metadata with @setDataFrame: %w", err)
	}
	header := base.RtmpHeader{
		Csid:         rtmp.CsidAmf,
		MsgTypeId:    base.RtmpTypeIdMetadata,
		MsgStreamId:  rtmp.Msid1,
		MsgLen:       uint32(len(meta)),
		TimestampAbs: 0,
	}
	chunks := rtmp.Message2Chunks(meta, &header)
	return w.session.Write(chunks)
}

// writeH264 sends a single H.264 access unit (Annex-B with optional
// SPS/PPS prefix on IDR). On the first frame carrying SPS+PPS, parses
// width/height from the SPS, emits onMetaData, then the AVC sequence
// header — both at timestamp 0 before the first NALU tag.
func (w *RTMPFrameWriter) writeH264(annexB []byte, pts, dts uint32) error {
	if !w.avcSeqSent {
		sps, pps, ok := extractSpsPpsFromAnnexB(annexB)
		if !ok {
			// Drop frames until we see one carrying SPS+PPS — the player
			// can't decode without them anyway.
			return nil
		}
		// Parse SPS for width/height so onMetaData carries them —
		// strict players use the metadata resolution to size the
		// video element before the first frame arrives.
		var ctx avc.Context
		if err := avc.ParseSps(sps, &ctx); err == nil {
			w.width, w.height = ctx.Width, ctx.Height
		}
		if err := w.sendMetadata(); err != nil {
			return err
		}
		seqTag, err := avc.BuildSeqHeaderFromSpsPps(sps, pps)
		if err != nil {
			return fmt.Errorf("rtmp writer: build avc seq header: %w", err)
		}
		// avc.BuildSeqHeaderFromSpsPps already prepends the 5-byte FLV
		// video tag header (FrameType<<4|CodecId, AVCPacketType=0,
		// CompositionTime=0). Wrapping it again with buildFLVAvcTag
		// would double the prefix — strict players then
		// misalign their AVCDecoderConfigurationRecord parser, see
		// 5 stray bytes inside the record, and fall back to "NAL type
		// 13 in extradata" warnings before failing every subsequent
		// NALU split. Send the buffer LAL returned as-is.
		if err := w.send(base.RtmpTypeIdVideo, 0, seqTag); err != nil {
			return err
		}
		w.avcSeqSent = true
	}

	// Build AVCC payload from slice NALs only — strict players
	// reject NALU tags that contain SPS / PPS / AUD because those belong
	// in the sequence header, not in the per-frame tag. avc.Annexb2Avcc
	// would copy *every* NAL including the SPS/PPS prefix on IDR; that
	// produces a tag the upstream parser interprets as malformed and
	// causes silent reset-by-peer mid-GOP. Mirrors LAL's avpacket2rtmp
	// remuxer behaviour (pkg/remux/avpacket2rtmp.go FeedAvPacket).
	avccData, isKey, hasSlice := buildAvccSliceOnly(annexB)
	if !hasSlice {
		// No coded slice in this access unit (rare — pure SPS/PPS update
		// frame between GOPs). Skip; the next frame will carry the slice.
		return nil
	}
	cts := int32(pts) - int32(dts) //nolint:gosec // RTMP CTS is signed 24-bit; we clamp on encode
	naluTag := buildFLVAvcTag(isKey, 1, cts, avccData)
	return w.send(base.RtmpTypeIdVideo, dts, naluTag)
}

// buildAvccSliceOnly walks the Annex-B access unit and returns the AVCC
// (4-byte BE length prefix per NAL) payload built from coded slice NALs
// only. SPS (type 7), PPS (type 8), AUD (type 9), and SEI (type 6) are
// dropped — those are either redundant with the seq header (SPS/PPS),
// not meaningful over RTMP (AUD), or noise that strict players reject
// (SEI in some codec profiles). Also reports whether any IDR slice was
// present, so the caller can flag the FLV FrameType correctly.
func buildAvccSliceOnly(annexB []byte) (avccPayload []byte, isKey bool, hasSlice bool) {
	out := make([]byte, 0, len(annexB))
	_ = avc.IterateNaluAnnexb(annexB, func(nal []byte) {
		if len(nal) == 0 {
			return
		}
		switch nal[0] & 0x1F {
		case 7, 8, 9, 6: // SPS, PPS, AUD, SEI — exclude from NALU tag
			return
		case 5: // IDR slice
			isKey = true
		}
		// AVCC: 4-byte BE length, then NAL bytes.
		out = append(out,
			byte(len(nal)>>24), byte(len(nal)>>16), byte(len(nal)>>8), byte(len(nal)),
		)
		out = append(out, nal...)
		hasSlice = true
	})
	return out, isKey, hasSlice
}

// writeAAC sends one RTMP audio tag PER ADTS frame contained in `adts`.
// Emits the AAC sequence header tag once at timestamp 0 before the first
// raw frame tag.
//
// gomedia's TSDemuxer delivers a complete PES payload via OnFrame, and a
// single PES often carries 4-8 concatenated ADTS frames (encoders bundle
// for efficiency — at 48 kHz, 8 frames = 170 ms of audio per delivery).
// RTMP convention is one raw AAC access unit per audio tag, so we must
// split the bundle and emit per-frame tags with monotonically increasing
// DTS. Without splitting, the entire bundle ships as one tag, the receiver
// (test5's RTMP pull → RTMPMsgConverter) treats it as a single AAC frame,
// and downstream consumers see the audio sample-count under-counted by
// the bundling factor — which surfaces in DASH as ~0.5 s audio segments
// (declared duration 24 × 1024 ticks) covering 4 s of actual audio data,
// and in HLS as audio that drifts then stutters because the TS muxer
// stamps the bundle with one PES PTS instead of per-frame PTSes.
func (w *RTMPFrameWriter) writeAAC(adts []byte, dts uint32) error {
	if len(adts) < adtsHeaderLength {
		return nil
	}
	if !w.aacSeqSent {
		asc, err := aac.MakeAscWithAdtsHeader(adts[:adtsHeaderLength])
		if err != nil {
			return fmt.Errorf("rtmp writer: extract asc from adts: %w", err)
		}
		seqTag := buildFLVAacTag(0, asc)
		if err := w.send(base.RtmpTypeIdAudio, 0, seqTag); err != nil {
			return err
		}
		w.aacSeqSent = true
	}

	return w.sendAACBundle(adts, dts)
}

// sendAACBundle iterates concatenated ADTS frames in `adts` and emits one
// RTMP raw-frame tag per ADTS frame, with each tag's DTS offset by
// frameIndex × 1024 / sampleRate (in ms) against the bundle's base DTS.
// Returns nil when adts has no parseable frames.
func (w *RTMPFrameWriter) sendAACBundle(adts []byte, baseDts uint32) error {
	pos := 0
	frameIndex := 0
	var sampleRate int
	for pos+adtsHeaderLength <= len(adts) {
		hdr, _, err := eyevinnaac.DecodeADTSHeader(bytes.NewReader(adts[pos:]))
		if err != nil {
			// Misaligned byte — advance one and try to resync.
			pos++
			continue
		}
		hLen := int(hdr.HeaderLength)
		pLen := int(hdr.PayloadLength)
		if hLen <= 0 || pLen <= 0 || pos+hLen+pLen > len(adts) {
			break
		}
		if sampleRate == 0 {
			sampleRate = int(hdr.Frequency())
		}
		if err := w.sendOneAACFrame(adts[pos+hLen:pos+hLen+pLen], baseDts, frameIndex, sampleRate); err != nil {
			return err
		}
		pos += hLen + pLen
		frameIndex++
	}
	return nil
}

// sendOneAACFrame ships one raw AAC access unit as an RTMP audio tag with
// DTS = baseDts + frameIndex × 1024 × 1000 / sampleRate. Skips empty or
// rate-less frames silently — the next frame in the bundle picks up the
// timeline correctly because the offset is computed from frameIndex, not
// a cumulative counter.
func (w *RTMPFrameWriter) sendOneAACFrame(rawAAC []byte, baseDts uint32, frameIndex, sampleRate int) error {
	if len(rawAAC) == 0 || sampleRate <= 0 {
		return nil
	}
	// Per-frame DTS in ms: each AAC-LC frame is 1024 samples; integer
	// arithmetic against frameIndex avoids accumulating per-frame
	// rounding error that a running ms counter would introduce.
	offsetMs := uint32(frameIndex * 1024 * 1000 / sampleRate)
	tag := buildFLVAacTag(1, rawAAC)
	return w.send(base.RtmpTypeIdAudio, baseDts+offsetMs, tag)
}

// send wraps payload in an RTMP message header and writes it to the
// session. The message is chunked once and pushed as a single TCP write —
// no per-chunk round-trips.
func (w *RTMPFrameWriter) send(typeID uint8, dts uint32, payload []byte) error {
	header := base.RtmpHeader{
		MsgTypeId:    typeID,
		TimestampAbs: dts,
		MsgLen:       uint32(len(payload)),
		MsgStreamId:  rtmp.Msid1,
	}
	switch typeID {
	case base.RtmpTypeIdVideo:
		header.Csid = rtmp.CsidVideo
	case base.RtmpTypeIdAudio:
		header.Csid = rtmp.CsidAudio
	}
	chunks := rtmp.Message2Chunks(payload, &header)
	return w.session.Write(chunks)
}

// buildFLVAvcTag wraps an AVC payload in the FLV video tag format the
// RTMP wire protocol expects. CompositionTime is encoded as a signed
// 24-bit big-endian integer in bytes 2-4.
func buildFLVAvcTag(isKey bool, packetType byte, cts int32, payload []byte) []byte {
	out := make([]byte, 5+len(payload))
	frameType := byte(2) // inter frame
	if isKey {
		frameType = 1
	}
	out[0] = (frameType << 4) | base.RtmpCodecIdAvc
	out[1] = packetType
	// Signed 24-bit big-endian. negative values clamped to zero — players
	// don't accept negative composition time and we'd rather skip the
	// adjustment than send a malformed tag.
	if cts < 0 {
		cts = 0
	}
	out[2] = byte(cts >> 16)
	out[3] = byte(cts >> 8)
	out[4] = byte(cts)
	copy(out[5:], payload)
	return out
}

// buildFLVAacTag wraps an AAC payload in the FLV audio tag format. The
// SoundFormat byte is fixed at 0xAF (SoundFormat=10 [AAC], SoundRate=3
// [44.1 kHz, ignored by AAC], SoundSize=1 [16-bit, ignored], SoundType=1
// [stereo, ignored]) — all the per-frame audio params come from the ASC
// in the seq header instead.
func buildFLVAacTag(packetType byte, payload []byte) []byte {
	out := make([]byte, 2+len(payload))
	out[0] = 0xAF
	out[1] = packetType
	copy(out[2:], payload)
	return out
}

// extractSpsPpsFromAnnexB walks a single Annex-B-formatted access unit
// and returns the first SPS (NAL type 7) and PPS (NAL type 8) it finds.
// ok=false when either is missing.
func extractSpsPpsFromAnnexB(annexB []byte) (sps, pps []byte, ok bool) {
	_ = avc.IterateNaluAnnexb(annexB, func(nal []byte) {
		if len(nal) == 0 {
			return
		}
		switch nal[0] & 0x1F {
		case 7: // SPS
			if sps == nil {
				sps = append([]byte(nil), nal...)
			}
		case 8: // PPS
			if pps == nil {
				pps = append([]byte(nil), nal...)
			}
		}
	})
	return sps, pps, sps != nil && pps != nil
}
