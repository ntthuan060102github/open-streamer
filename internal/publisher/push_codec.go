package publisher

// push_codec.go — convert demuxed H.264 / AAC frames into FLV-tag RtmpMsg
// with proper composition_time so B-frames render correctly at the receiver.
//
// Why this exists:
//
// lal's high-level helper `remux.AvPacket2RtmpRemuxer` is convenient but
// drops `pkt.Pts` — the FLV video tag's composition_time field (PTS - DTS,
// 24-bit signed) is always zero. For streams with B-frames, the receiver
// then displays frames in DTS order instead of PTS order, producing
// fine-grained motion jitter ("even but not smooth"). HLS / DASH / SRT do
// not have this problem because TS PES headers carry both PTS and DTS.
//
// pushCodecAdapter does the same job as `AvPacket2RtmpRemuxer` but preserves
// PTS-DTS as composition_time, so B-frames work end-to-end through RTMP push.
// Lal still handles the wire transport (chunking, handshake, ack) — we only
// own the FLV tag composition.
//
// Lifecycle: one adapter per push session. Reset on session restart so
// SPS/PPS / AAC ASC sequence headers re-emit at the new session's first frame.

import (
	"bytes"
	"encoding/binary"

	"github.com/q191201771/lal/pkg/aac"
	"github.com/q191201771/lal/pkg/avc"
	"github.com/q191201771/lal/pkg/base"
	"github.com/q191201771/lal/pkg/rtmp"
)

// pushCodecAdapter buffers SPS/PPS state and emits FLV tags via the onMsg
// callback. Single-threaded — caller drives it from the demuxer goroutine.
type pushCodecAdapter struct {
	// SPS/PPS state. When both are present, an AVCDecoderConfigurationRecord
	// (FLV "AVC sequence header") is emitted before the next slice tag.
	// Re-emitted automatically if the bytes change mid-stream (encoder
	// reconfiguration, source switch with different codec params).
	sps          []byte
	pps          []byte
	seqHeaderSet bool

	// AAC AudioSpecificConfig is derived from the first ADTS header we see
	// and emitted once as the audio sequence header before any raw AAC frame.
	ascSent bool

	// metadataSent gates the one-time `@setDataFrame onMetaData` emission so
	// receivers (common RTMP destinations) get codec hints up front.
	metadataSent bool

	// onMsg sinks the composed FLV-tag RtmpMsg — typically lal PushSession.WriteMsg.
	// Returning an error from onMsg is fatal for the session; the adapter
	// itself does not retry.
	onMsg func(base.RtmpMsg) error
}

// newPushCodecAdapter binds an output sink. The caller is responsible for
// invoking the appropriate Feed* method per demuxed frame.
func newPushCodecAdapter(onMsg func(base.RtmpMsg) error) *pushCodecAdapter {
	return &pushCodecAdapter{onMsg: onMsg}
}

// FeedH264 takes one H.264 access unit in Annex-B form and emits the
// corresponding FLV video tag(s):
//
//   - Caches SPS/PPS NALUs; emits AVCDecoderConfigurationRecord (sequence
//     header) when both are first available, or whenever they change.
//   - Drops AUD (NAL type 9) — FLV doesn't carry it.
//   - Aggregates the remaining slice NALUs into a single tag with
//     composition_time = clamp(ptsMs - dtsMs, 0, int24max).
//
// dtsMs / ptsMs are already-relative milliseconds (caller has subtracted
// the session base DTS).
func (a *pushCodecAdapter) FeedH264(annexB []byte, dtsMs, ptsMs int64) error {
	if len(annexB) == 0 {
		return nil
	}
	nals, err := avc.SplitNaluAnnexb(annexB)
	if err != nil {
		// Not all access units are well-formed (encoder hiccup at start,
		// partial PES). Drop silently — next frame should recover.
		_ = err
		return nil //nolint:nilerr // intentional: malformed frame is non-fatal
	}

	var slices [][]byte
	isKey := false
	spsPpsChanged := false

	for _, nal := range nals {
		if len(nal) == 0 {
			continue
		}
		switch avc.ParseNaluType(nal[0]) {
		case avc.NaluTypeAud:
			// Access unit delimiter — meaningful only inside Annex-B; FLV doesn't carry it.
			continue
		case avc.NaluTypeSps:
			if !bytes.Equal(a.sps, nal) {
				a.sps = append(a.sps[:0], nal...)
				spsPpsChanged = true
			}
		case avc.NaluTypePps:
			if !bytes.Equal(a.pps, nal) {
				a.pps = append(a.pps[:0], nal...)
				spsPpsChanged = true
			}
		case avc.NaluTypeIdrSlice:
			isKey = true
			slices = append(slices, nal)
		default:
			slices = append(slices, nal)
		}
	}

	if spsPpsChanged {
		a.seqHeaderSet = false
	}

	// Emit metadata once before the first media tag so receivers see codec hints.
	if err := a.ensureMetadata(); err != nil {
		return err
	}

	// Emit AVC sequence header when both SPS and PPS are available and
	// haven't been sent for the current SPS/PPS values.
	if !a.seqHeaderSet && len(a.sps) > 0 && len(a.pps) > 0 {
		sh, sErr := avc.BuildSeqHeaderFromSpsPps(a.sps, a.pps)
		if sErr != nil {
			_ = sErr
			return nil //nolint:nilerr // bad SPS/PPS — drop, wait for next IDR
		}
		if mErr := a.emitVideo(sh, uint32(dtsMs)); mErr != nil {
			return mErr
		}
		a.seqHeaderSet = true
	}

	if len(slices) == 0 {
		// Frame contained only SPS/PPS (or only AUD) — sequence header above
		// covered it. Nothing else to send.
		return nil
	}

	cts := ptsMs - dtsMs
	if cts < 0 {
		cts = 0
	}
	if cts > maxCompositionTime {
		cts = maxCompositionTime
	}

	payload := buildAVCNaluTag(slices, isKey, int32(cts))
	return a.emitVideo(payload, uint32(dtsMs))
}

// FeedAAC takes one ADTS-prefixed AAC frame and emits the corresponding FLV
// audio tag. The first call also emits AudioSpecificConfig as the audio
// sequence header. Frames shorter than the 7-byte ADTS header (or with no
// AAC payload after the header) are dropped silently.
func (a *pushCodecAdapter) FeedAAC(adtsFrame []byte, dtsMs int64) error {
	if len(adtsFrame) <= adtsHeaderSize {
		return nil
	}

	if err := a.ensureMetadata(); err != nil {
		return err
	}

	if !a.ascSent {
		ash, sErr := aac.MakeAudioDataSeqHeaderWithAdtsHeader(adtsFrame)
		if sErr != nil {
			_ = sErr
			return nil //nolint:nilerr // can't decode ADTS — drop, wait for next frame
		}
		if mErr := a.emitAudio(ash, uint32(dtsMs)); mErr != nil {
			return mErr
		}
		a.ascSent = true
	}

	raw := adtsFrame[adtsHeaderSize:]
	payload := make([]byte, 2+len(raw))
	payload[0] = aacAudioTagHeader // SoundFormat=AAC | 44kHz | 16bit | stereo
	payload[1] = base.RtmpAacPacketTypeRaw
	copy(payload[2:], raw)
	return a.emitAudio(payload, uint32(dtsMs))
}

// ensureMetadata emits the one-time `@setDataFrame onMetaData` AMF tag with
// codec hints — required by some strict ingest endpoints (common strict ingest endpoints, certain
// CDN pops) before they accept media. Mirrors what lal's AvPacket2RtmpRemuxer
// does on first emit.
func (a *pushCodecAdapter) ensureMetadata() error {
	if a.metadataSent {
		return nil
	}
	// We do not currently parse SPS for width/height — pass -1 so the
	// receiver derives dimensions from the AVC sequence header itself.
	bMeta, err := rtmp.BuildMetadata(-1, -1, int(base.RtmpSoundFormatAac), int(base.RtmpCodecIdAvc))
	if err != nil {
		return err
	}
	msg := base.RtmpMsg{
		Header: base.RtmpHeader{
			Csid:         rtmp.CsidAmf,
			MsgLen:       uint32(len(bMeta)),
			MsgTypeId:    base.RtmpTypeIdMetadata,
			MsgStreamId:  rtmp.Msid1,
			TimestampAbs: 0,
		},
		Payload: bMeta,
	}
	if err := a.onMsg(msg); err != nil {
		return err
	}
	a.metadataSent = true
	return nil
}

func (a *pushCodecAdapter) emitVideo(payload []byte, dtsMs uint32) error {
	return a.onMsg(base.RtmpMsg{
		Header: base.RtmpHeader{
			Csid:         rtmp.CsidVideo,
			MsgLen:       uint32(len(payload)),
			MsgTypeId:    base.RtmpTypeIdVideo,
			MsgStreamId:  rtmp.Msid1,
			TimestampAbs: dtsMs,
		},
		Payload: payload,
	})
}

func (a *pushCodecAdapter) emitAudio(payload []byte, dtsMs uint32) error {
	return a.onMsg(base.RtmpMsg{
		Header: base.RtmpHeader{
			Csid:         rtmp.CsidAudio,
			MsgLen:       uint32(len(payload)),
			MsgTypeId:    base.RtmpTypeIdAudio,
			MsgStreamId:  rtmp.Msid1,
			TimestampAbs: dtsMs,
		},
		Payload: payload,
	})
}

// buildAVCNaluTag composes an FLV "AVC NALU" video tag payload:
//
//	[FrameType+CodecID:1][PacketType=NALU:1][CompositionTime:int24][NALU_LEN:u32][NALU]...
//
// FrameType high nibble = 1 for keyframe (IDR), 2 for inter; low nibble = 7 (AVC).
// composition_time is signed 24-bit big-endian milliseconds (PTS - DTS).
func buildAVCNaluTag(slices [][]byte, isKey bool, ctsMs int32) []byte {
	totalLen := videoTagHeaderSize
	for _, n := range slices {
		totalLen += avccLenPrefixSize + len(n)
	}
	payload := make([]byte, videoTagHeaderSize, totalLen)
	if isKey {
		payload[0] = base.RtmpAvcKeyFrame
	} else {
		payload[0] = base.RtmpAvcInterFrame
	}
	payload[1] = base.RtmpAvcPacketTypeNalu
	payload[2] = byte(ctsMs >> 16)
	payload[3] = byte(ctsMs >> 8)
	payload[4] = byte(ctsMs)
	for _, n := range slices {
		var lenBuf [avccLenPrefixSize]byte
		binary.BigEndian.PutUint32(lenBuf[:], uint32(len(n)))
		payload = append(payload, lenBuf[:]...)
		payload = append(payload, n...)
	}
	return payload
}

const (
	videoTagHeaderSize = 5 // [type+codec][packet_type][cts:3]
	avccLenPrefixSize  = 4 // big-endian uint32 NALU length prefix
	adtsHeaderSize     = 7 // standard ADTS header (without CRC)
	// aacAudioTagHeader = SoundFormat=10(AAC) | SoundRate=3(44k) | SoundSize=1(16bit) | SoundType=1(stereo).
	aacAudioTagHeader byte = 0xAF
	// maxCompositionTime is int24 signed max (~16.7s). Real composition
	// offsets are <100 ms in practice; the cap is a defence against
	// pathological PTS values (encoder bug, demuxer wrap) that would
	// otherwise overflow into the FrameType nibble.
	maxCompositionTime int64 = 0x7fffff
)
