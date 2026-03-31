package push

import (
	"context"
	"log/slog"
	"strings"

	srt "github.com/datarhei/gosrt"

	"github.com/open-streamer/open-streamer/config"
	"github.com/open-streamer/open-streamer/internal/buffer"
	"github.com/open-streamer/open-streamer/internal/domain"
)

// SRTServer is a single-port SRT push server.
// External encoders connect with an SRT StreamID that encodes the stream key:
//
//	srt://<host>:<port>?streamid=live/<stream_key>
//
// SRT transports raw MPEG-TS, so no format conversion is needed — received
// bytes are forwarded directly to the Buffer Hub.
//
// For publishing use mode=publish (or just connect — server treats it as PUBLISH).
type SRTServer struct {
	cfg      config.IngestorConfig
	registry Registry
	server   *srt.Server
}

// NewSRTServer constructs an SRTServer.
func NewSRTServer(cfg config.IngestorConfig, registry Registry) *SRTServer {
	return &SRTServer{cfg: cfg, registry: registry}
}

// ListenAndServe starts the SRT listener and blocks until ctx is cancelled.
func (s *SRTServer) ListenAndServe(ctx context.Context) error {
	addr := s.cfg.SRTAddr
	if addr == "" {
		addr = ":9999"
	}

	srtCfg := srt.DefaultConfig()

	s.server = &srt.Server{
		Addr:   addr,
		Config: &srtCfg,

		// HandleConnect is called for every new connection request.
		// We inspect the StreamID to decide: PUBLISH, SUBSCRIBE, or REJECT.
		HandleConnect: func(req srt.ConnRequest) srt.ConnType {
			streamKey := extractStreamKey(req.StreamId())
			if _, _, err := s.registry.Lookup(streamKey); err != nil {
				slog.Warn("srt: rejecting unknown stream key",
					"stream_id_raw", req.StreamId(),
					"stream_key", streamKey,
				)
				return srt.REJECT
			}
			return srt.PUBLISH
		},

		// HandlePublish is called when an accepted PUBLISH connection is ready.
		HandlePublish: func(conn srt.Conn) {
			streamKey := extractStreamKey(conn.StreamId())
			streamID, buf, err := s.registry.Lookup(streamKey)
			if err != nil {
				// Should not happen — HandleConnect already checked.
				conn.Close()
				return
			}
			slog.Info("srt: publisher connected",
				"stream_key", streamKey,
				"stream_code", streamID,
				"remote", conn.RemoteAddr(),
			)
			handleSRTConn(ctx, conn, streamID, buf)
		},
	}

	slog.Info("srt server: listening", "addr", addr)

	// Shutdown the server when ctx is cancelled.
	go func() {
		<-ctx.Done()
		s.server.Shutdown()
	}()

	if err := s.server.ListenAndServe(); err != nil && err != srt.ErrServerClosed {
		if ctx.Err() != nil {
			return nil // clean shutdown
		}
		return err
	}
	return nil
}

// handleSRTConn reads MPEG-TS from the SRT connection and writes to the buffer.
func handleSRTConn(ctx context.Context, conn srt.Conn, streamID domain.StreamCode, buf *buffer.Service) {
	defer conn.Close()
	defer slog.Info("srt: publisher disconnected", "stream_code", streamID)

	readBuf := make([]byte, 1316*4) // read a few SRT payloads at a time

	for {
		if ctx.Err() != nil {
			return
		}

		n, err := conn.Read(readBuf)
		if n > 0 {
			pkt := make([]byte, n)
			copy(pkt, readBuf[:n])
			if writeErr := buf.Write(streamID, buffer.Packet(pkt)); writeErr != nil {
				slog.Error("srt: buffer write failed",
					"stream_code", streamID,
					"err", writeErr,
				)
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// extractStreamKey parses the stream key from an SRT StreamID.
// Supports common formats:
//
//	"live/mykey"   → "mykey"
//	"mykey"        → "mykey"
//	"#!::r=live/mykey,..." (SRT access control format) → "mykey"
func extractStreamKey(streamID string) string {
	// SRT access control format: "#!::r=...,m=publish,..."
	if strings.HasPrefix(streamID, "#!::") {
		for _, part := range strings.Split(streamID[4:], ",") {
			kv := strings.SplitN(part, "=", 2)
			if len(kv) == 2 && kv[0] == "r" {
				streamID = kv[1]
				break
			}
		}
	}

	// Strip leading "live/" or any app-path prefix.
	if idx := strings.LastIndex(streamID, "/"); idx >= 0 {
		return streamID[idx+1:]
	}
	return streamID
}
