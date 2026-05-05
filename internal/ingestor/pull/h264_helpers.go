package pull

// h264_helpers.go — H.264 byte-format helpers shared by the RTSP reader
// and the gomedia-based RTMP reader.
//
// The two readers receive H.264 access units in different shapes:
//   - RTSP (gortsplib): AVCC length-prefixed NAL units (4-byte big-endian
//     length + NAL bytes, repeated).
//   - RTMP (gomedia): Annex-B start codes already, SPS/PPS prepended on IDR.
//
// Both must produce Annex-B output with SPS/PPS in front of every IDR so
// the downstream MPEG-TS muxer (tsmux.FromAV) emits decodable PES.

import "encoding/binary"

// h264ForTSMuxer converts AVCC length-prefixed bytes to Annex-B start codes.
// Returns the input unchanged when it is already Annex-B (no AVCC structure
// detected). Used at the RTSP boundary where source frames arrive in AVCC
// format; RTMP frames bypass this — gomedia delivers them already in
// Annex-B form via flv.AVCTagDemuxer.
func h264ForTSMuxer(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	if isH264AVCCAccessUnit(b) {
		return avccAccessUnitToAnnexB(b)
	}
	return b
}

// h264AccessUnitForTS prepends an Annex-B SPS+PPS prefix in front of an
// IDR access unit so downstream decoders can initialise from this single
// frame without needing the codec config out-of-band. Non-IDR frames pass
// through unchanged. Used by the RTSP reader; the RTMP reader doesn't need
// it because gomedia's flv demuxer already prepends SPS/PPS internally.
func h264AccessUnitForTS(isKeyFrame bool, psPrefixAnnexB, annexBAU []byte) []byte {
	if len(annexBAU) == 0 {
		return nil
	}
	if len(psPrefixAnnexB) == 0 || !isKeyFrame {
		return annexBAU
	}
	out := make([]byte, 0, len(psPrefixAnnexB)+len(annexBAU))
	out = append(out, psPrefixAnnexB...)
	out = append(out, annexBAU...)
	return out
}

// maxH264NALSize is the sanity cap that distinguishes AVCC length prefixes
// from Annex-B start codes. A "length" larger than this is taken as proof
// the bytes are not AVCC (real NALs almost never exceed 1 MiB).
const maxH264NALSize = 8 * 1024 * 1024

// isH264AVCCAccessUnit returns true when b is a valid sequence of
// 4-byte-length-prefixed NAL units. Cheap structural check — does not
// validate the NAL bytes themselves.
func isH264AVCCAccessUnit(b []byte) bool {
	i := 0
	for i+4 <= len(b) {
		n := int(binary.BigEndian.Uint32(b[i : i+4]))
		if n <= 0 || n > maxH264NALSize || i+4+n > len(b) {
			return false
		}
		i += 4 + n
	}
	return i == len(b)
}

// avccAccessUnitToAnnexB rewrites AVCC length-prefixed NAL units into
// 4-byte Annex-B start-code form. Returns the input unchanged on parse
// failure (defensive — caller already gated on isH264AVCCAccessUnit).
func avccAccessUnitToAnnexB(src []byte) []byte {
	var out []byte
	i := 0
	for i+4 <= len(src) {
		n := int(binary.BigEndian.Uint32(src[i : i+4]))
		i += 4
		if n <= 0 || i+n > len(src) {
			return src
		}
		out = append(out, 0, 0, 0, 1)
		out = append(out, src[i:i+n]...)
		i += n
	}
	if len(out) == 0 {
		return src
	}
	return out
}
