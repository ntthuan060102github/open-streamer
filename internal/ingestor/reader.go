// Package ingestor handles raw stream ingestion.
//
// The ingestor accepts a plain URL from the user and automatically derives
// the protocol from the URL scheme — no protocol configuration is needed.
//
// Pull mode (server connects to remote source). All pull paths yield
// [domain.AVPacket] via [PacketReader] / [NewPacketReader]:
//   - UDP MPEG-TS  → TS demux → AVPacket
//   - HLS playlist → HTTP + M3U8 → TS demux → AVPacket
//   - Local file   → resolved through the VOD registry → TS demux → AVPacket
//   - SRT pull     → gosrt → TS demux → AVPacket
//   - RTMP pull    → native RTMP → AVPacket
//   - RTSP pull    → gortsplib → AVPacket
//
// Push mode (external encoder connects to our server):
//   - RTMP → RTMPServer (gomedia RTMP server handle, native FLV→MPEG-TS)
//   - SRT  → SRTServer  (gosrt native, MPEG-TS passthrough)
//
// Push mode is auto-detected: if the URL host is 0.0.0.0 / :: and the scheme
// is rtmp or srt, the ingestor starts/uses the shared push server instead of
// a pull worker.
//
// Local file inputs MUST take the form file://<vod_mount>/<relative/path>;
// bare paths and absolute file:/// URLs are rejected so the VOD mount layer
// is the single point of policy.
package ingestor

import (
	"context"
	"fmt"

	"github.com/ntt0601zcoder/open-streamer/config"
	"github.com/ntt0601zcoder/open-streamer/internal/domain"
	"github.com/ntt0601zcoder/open-streamer/internal/ingestor/pull"
	"github.com/ntt0601zcoder/open-streamer/pkg/protocol"
)

// VODResolver resolves a file:// URL referencing a registered VOD mount to an
// absolute filesystem path. It is the contract between the ingestor and the
// internal/vod registry — kept as an interface so tests can stub it.
type VODResolver interface {
	Resolve(rawURL string) (path string, loop bool, err error)
}

// PacketReader is a pull source that yields elementary-stream access units
// ([domain.AVPacket]).
type PacketReader interface {
	Open(ctx context.Context) error
	ReadPackets(ctx context.Context) ([]domain.AVPacket, error)
	Close() error
}

// NewPacketReader constructs the appropriate PacketReader for the given input URL.
// RTSP and RTMP pull emit native AVPackets; MPEG-TS transports are demuxed to AVPackets.
//
// For file:// URLs the VODResolver translates the URL to an absolute host path;
// a nil resolver or an unknown mount both produce an error. Any other scheme
// ignores the resolver.
//
// Returns an error when:
//   - The URL scheme is unrecognised
//   - The URL describes a push-listen address (handled by the push servers)
//   - The URL is a file:// reference that cannot be resolved against a VOD mount
func NewPacketReader(input domain.Input, cfg config.IngestorConfig, vods VODResolver) (PacketReader, error) {
	if protocol.IsPushListen(input.URL) {
		return nil, fmt.Errorf(
			"ingestor: %q is a push-listen address — handled by the push server, not a pull reader",
			input.URL,
		)
	}

	switch protocol.Detect(input.URL) {
	case protocol.KindRTSP:
		return pull.NewRTSPReader(input), nil
	case protocol.KindRTMP:
		return pull.NewRTMPReader(input), nil
	case protocol.KindUDP:
		return pull.NewTSDemuxPacketReader(pull.NewUDPReader(input)), nil
	case protocol.KindHLS:
		return pull.NewTSDemuxPacketReader(pull.NewHLSReader(input, cfg)), nil
	case protocol.KindFile:
		if vods == nil {
			return nil, fmt.Errorf("ingestor: cannot resolve %q — no VOD resolver configured", input.URL)
		}
		path, loop, err := vods.Resolve(input.URL)
		if err != nil {
			return nil, fmt.Errorf("ingestor: resolve file url: %w", err)
		}
		return pull.NewTSDemuxPacketReader(pull.NewFileReader(path, loop)), nil
	case protocol.KindSRT:
		return pull.NewTSDemuxPacketReader(pull.NewSRTReader(input)), nil
	case protocol.KindPublish, protocol.KindUnknown:
		// KindPublish is handled before this switch; KindUnknown falls through to error.
		fallthrough
	default:
		return nil, fmt.Errorf(
			"ingestor: cannot infer protocol from URL %q — unsupported scheme",
			input.URL,
		)
	}
}
