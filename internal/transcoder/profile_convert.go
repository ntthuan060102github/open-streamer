package transcoder

import (
	"strconv"
	"strings"

	"github.com/ntt0601zcoder/open-streamer/internal/domain"
)

// profileToDomain converts the FFmpeg-CLI-flavored transcoder.Profile
// (Bitrate as "1500k", CodecProfile / CodecLevel naming) into the
// schema-canonical domain.VideoProfile that the native backend
// consumes. Inverse of the conversion the coordinator does in
// profilesFromVideoConfig — we round-trip back so native_run.go can
// feed the original YAML semantics into native.Pipeline.
func profileToDomain(p Profile) domain.VideoProfile {
	return domain.VideoProfile{
		Width:            p.Width,
		Height:           p.Height,
		Bitrate:          parseBitrateKbps(p.Bitrate),
		MaxBitrate:       p.MaxBitrate,
		Framerate:        p.Framerate,
		KeyframeInterval: p.KeyframeInterval,
		Codec:            domain.VideoCodec(p.Codec),
		Preset:           p.Preset,
		Profile:          p.CodecProfile,
		Level:            p.CodecLevel,
		Bframes:          p.Bframes,
		Refs:             p.Refs,
		SAR:              p.SAR,
		ResizeMode:       domain.ResizeMode(p.ResizeMode),
	}
}

// parseBitrateKbps strips the trailing "k" / "K" suffix the FFmpeg CLI
// convention uses and returns the integer kbps value. "1500k" → 1500.
// Returns 0 on parse failure so the encoder falls back to its default
// rather than crashing — matches existing CLI behaviour where a typo
// produced a libav warning rather than an immediate refuse.
func parseBitrateKbps(s string) int {
	s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSuffix(s, "k"), "K"))
	if s == "" {
		return 0
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}
