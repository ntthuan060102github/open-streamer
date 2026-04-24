package domain

// AVCodec identifies elementary stream codec for AVPacket payloads.
type AVCodec uint8

// AVCodec values.
const (
	AVCodecUnknown AVCodec = iota
	AVCodecH264
	AVCodecH265
	AVCodecAAC
)

// IsVideo reports whether the codec carries a video elementary stream.
func (c AVCodec) IsVideo() bool {
	return c == AVCodecH264 || c == AVCodecH265
}

// IsAudio reports whether the codec carries an audio elementary stream.
func (c AVCodec) IsAudio() bool {
	return c == AVCodecAAC
}

// AVPacket is one decoded video access unit (Annex B H.264/H.265) or one AAC frame (ADTS in Data).
// PTSms and DTSms are presentation / decode timestamps in milliseconds (MPEG-TS / gomedia convention).
type AVPacket struct {
	Codec         AVCodec
	Data          []byte
	PTSms         uint64
	DTSms         uint64
	KeyFrame      bool
	Discontinuity bool
}

// Clone returns a deep copy suitable for buffer fan-out.
func (p *AVPacket) Clone() *AVPacket {
	if p == nil {
		return nil
	}
	c := *p
	if len(p.Data) > 0 {
		c.Data = append([]byte(nil), p.Data...)
	}
	return &c
}
