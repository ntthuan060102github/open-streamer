package pull

// rtsp.go — RTSP pull ingestor using nareix/joy4/format/rtsp.
//
// joy4 performs DESCRIBE → SETUP → PLAY internally inside Dial + Streams, so
// the caller only needs to call ReadPacket in a loop — identical to the RTMP reader.
//
// Codec support: H.264 (AVCC → Annex-B) and AAC (raw → ADTS).
// The same package-level helpers used by RTMPReader are reused here:
//   - filterSupportedRTMPStreams
//   - h264ForTSMuxer / h264AccessUnitForTS
//
// Mid-stream codec changes (SPS/PPS rotation that some cameras emit) are
// handled by calling HandleCodecDataChange() and continuing the loop.

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	joyav "github.com/nareix/joy4/av"
	"github.com/nareix/joy4/codec/aacparser"
	joyh264 "github.com/nareix/joy4/codec/h264parser"
	joyrtsp "github.com/nareix/joy4/format/rtsp"

	gocodec "github.com/yapingcat/gomedia/go-codec"

	"github.com/ntthuan060102github/open-streamer/internal/domain"
)

const rtspChanSize = 16384

// RTSPReader connects to a remote RTSP server in play (pull) mode and emits domain.AVPacket.
//
// It is structurally identical to RTMPReader: Dial → Streams → readLoop → ReadPackets.
// H.264 AVCC access units are converted to Annex-B; SPS/PPS from the SDP are injected
// on every IDR frame.  AAC frames are wrapped in ADTS when the raw payload is bare.
type RTSPReader struct {
	input domain.Input

	mu   sync.Mutex
	conn *joyrtsp.Client
	pkts chan domain.AVPacket
	done chan struct{}

	filtered            []joyav.CodecData
	idxMap              map[int]int // source stream index → index in filtered
	h264KeyPrefixAnnexB [][]byte   // per filtered index: 4-byte-SC + SPS + 4-byte-SC + PPS
	aacCfg              *aacparser.MPEG4AudioConfig
}

// NewRTSPReader constructs an RTSPReader without opening a connection.
func NewRTSPReader(input domain.Input) *RTSPReader {
	return &RTSPReader{input: input}
}

// Open dials the RTSP source, negotiates tracks, and starts the background read loop.
// It blocks until DESCRIBE + SETUP + PLAY + first codec probing complete.
func (r *RTSPReader) Open(ctx context.Context) error {
	_ = ctx // joy4 dial has no context-aware API

	r.mu.Lock()
	if r.conn != nil {
		r.mu.Unlock()
		return fmt.Errorf("rtsp reader: already open")
	}
	r.pkts = make(chan domain.AVPacket, rtspChanSize)
	r.done = make(chan struct{})
	r.mu.Unlock()

	connectTimeout := time.Duration(r.input.Net.ConnectTimeoutSec) * time.Second
	if connectTimeout == 0 {
		connectTimeout = 10 * time.Second
	}

	conn, err := joyrtsp.DialTimeout(r.input.URL, connectTimeout)
	if err != nil {
		r.abortOpen()
		return fmt.Errorf("rtsp reader: dial %q: %w", r.input.URL, err)
	}

	streams, err := conn.Streams()
	if err != nil {
		_ = conn.Close()
		r.abortOpen()
		return fmt.Errorf("rtsp reader: streams %q: %w", r.input.URL, err)
	}

	r.scanStreams(streams)

	filtered, idxMap := filterSupportedRTMPStreams(streams)
	if len(filtered) == 0 {
		_ = conn.Close()
		r.abortOpen()
		return fmt.Errorf("rtsp reader: no supported streams in %q (need H264 or AAC)", r.input.URL)
	}

	h264Prefix := r.buildH264Prefixes(filtered)

	r.mu.Lock()
	r.conn = conn
	r.filtered = filtered
	r.idxMap = idxMap
	r.h264KeyPrefixAnnexB = h264Prefix
	r.mu.Unlock()

	go r.readLoop()
	return nil
}

// scanStreams logs discovered tracks and captures the AAC config for ADTS wrapping.
func (r *RTSPReader) scanStreams(streams []joyav.CodecData) {
	for i, s := range streams {
		supported := s.Type() == joyav.H264 || s.Type() == joyav.AAC
		slog.Info("rtsp reader: source stream discovered",
			"url", r.input.URL,
			"source_stream_idx", i,
			"codec", s.Type().String(),
			"supported", supported,
		)
		if s.Type() == joyav.AAC {
			if cd, ok := s.(aacparser.CodecData); ok {
				cfg := cd.Config
				r.aacCfg = &cfg
			}
		}
	}
}

// buildH264Prefixes constructs the Annex-B SPS+PPS prefix for each H.264 filtered track.
func (r *RTSPReader) buildH264Prefixes(filtered []joyav.CodecData) [][]byte {
	out := make([][]byte, len(filtered))
	for i, s := range filtered {
		if s.Type() != joyav.H264 {
			continue
		}
		cd, ok := s.(joyh264.CodecData)
		if !ok {
			slog.Warn("rtsp reader: H264 track has no h264parser.CodecData — SPS/PPS injection disabled",
				"url", r.input.URL, "es_stream_idx", i)
			continue
		}
		sps, pps := cd.SPS(), cd.PPS()
		if len(sps) == 0 || len(pps) == 0 {
			continue
		}
		var b []byte
		b = append(b, 0, 0, 0, 1)
		b = append(b, sps...)
		b = append(b, 0, 0, 0, 1)
		b = append(b, pps...)
		out[i] = b
	}
	return out
}

func (r *RTSPReader) abortOpen() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pkts != nil {
		close(r.pkts)
	}
	r.pkts = nil
	r.done = nil
	r.conn = nil
}

func (r *RTSPReader) readLoop() {
	defer r.teardownAfterReadLoop()
	defer func() {
		if rec := recover(); rec != nil {
			slog.Error("rtsp reader: panic in joy4, closing connection",
				"url", r.input.URL, "panic", rec)
		}
	}()

	for {
		r.mu.Lock()
		c := r.conn
		r.mu.Unlock()
		if c == nil {
			return
		}
		if stop := r.readOnce(c); stop {
			return
		}
	}
}

// readOnce reads one packet from the RTSP client and emits it.
// Returns true when the read loop should stop.
func (r *RTSPReader) readOnce(c *joyrtsp.Client) bool {
	pkt, err := c.ReadPacket()
	if err != nil {
		if err == joyrtsp.ErrCodecDataChange {
			return !r.handleCodecDataChange(c)
		}
		slog.Warn("rtsp reader: read packet ended",
			"url", r.input.URL, "err", err)
		return true
	}
	return r.emitJoyRTSPPacket(pkt)
}

// emitJoyRTSPPacket converts one joy4 av.Packet to domain.AVPacket and sends it.
// Returns true when the caller should stop the read loop.
func (r *RTSPReader) emitJoyRTSPPacket(pkt joyav.Packet) bool {
	mapped, ok := r.idxMap[int(pkt.Idx)]
	if !ok || mapped < 0 || mapped >= len(r.filtered) {
		return false
	}
	codec := r.filtered[mapped].Type()
	dtsMS := uint64(pkt.Time / time.Millisecond)
	ptsMS := dtsMS + uint64(pkt.CompositionTime/time.Millisecond)

	r.mu.Lock()
	outCh := r.pkts
	doneCh := r.done
	r.mu.Unlock()
	if outCh == nil {
		return true
	}

	switch codec {
	case joyav.H264:
		annexB := h264ForTSMuxer(pkt.Data)
		data := h264AccessUnitForTS(pkt.IsKeyFrame, r.h264KeyPrefixAnnexB[mapped], annexB)
		if len(data) == 0 {
			return false
		}
		p := domain.AVPacket{
			Codec:    domain.AVCodecH264,
			Data:     data,
			PTSms:    ptsMS,
			DTSms:    dtsMS,
			KeyFrame: gocodec.IsH264IDRFrame(data),
		}
		select {
		case outCh <- p:
		case <-doneCh:
			return true
		}

	case joyav.AAC:
		data := aacFrameToADTS(r.aacCfg, pkt.Data)
		if len(data) == 0 {
			return false
		}
		p := domain.AVPacket{
			Codec: domain.AVCodecAAC,
			Data:  data,
			PTSms: dtsMS,
			DTSms: dtsMS,
		}
		select {
		case outCh <- p:
		case <-doneCh:
			return true
		}

	default:
		return false
	}
	return false
}

// handleCodecDataChange handles a mid-stream SPS/PPS rotation.
// It re-reads stream codec data and rebuilds the H.264 SPS/PPS prefix table
// so that subsequent IDR frames carry the updated parameter sets.
// Returns true to continue the read loop, false to abort.
func (r *RTSPReader) handleCodecDataChange(c *joyrtsp.Client) bool {
	newConn, err := c.HandleCodecDataChange()
	if err != nil {
		slog.Warn("rtsp reader: HandleCodecDataChange failed",
			"url", r.input.URL, "err", err)
		return false
	}

	streams, err := newConn.Streams()
	if err != nil {
		slog.Warn("rtsp reader: re-read streams after codec change failed",
			"url", r.input.URL, "err", err)
		_ = newConn.Close()
		return false
	}

	filtered, idxMap := filterSupportedRTMPStreams(streams)
	h264Prefix := r.buildH264Prefixes(filtered)

	r.mu.Lock()
	r.conn = newConn
	r.filtered = filtered
	r.idxMap = idxMap
	r.h264KeyPrefixAnnexB = h264Prefix
	r.mu.Unlock()

	slog.Info("rtsp reader: codec data updated", "url", r.input.URL)
	return true
}

func (r *RTSPReader) teardownAfterReadLoop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.conn != nil {
		_ = r.conn.Close()
		r.conn = nil
	}
	if r.done != nil {
		close(r.done)
		r.done = nil
	}
	if r.pkts != nil {
		close(r.pkts)
		r.pkts = nil
	}
}

// ReadPackets blocks until at least one AVPacket is available or the source ends.
func (r *RTSPReader) ReadPackets(ctx context.Context) ([]domain.AVPacket, error) {
	r.mu.Lock()
	ch := r.pkts
	r.mu.Unlock()
	if ch == nil {
		return nil, io.EOF
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case p, ok := <-ch:
		if !ok {
			return nil, io.EOF
		}
		batch := []domain.AVPacket{p}
		for len(batch) < 256 {
			select {
			case p2, ok2 := <-ch:
				if !ok2 {
					return batch, nil
				}
				batch = append(batch, p2)
			default:
				return batch, nil
			}
		}
		return batch, nil
	}
}

// Close closes the RTSP connection; the readLoop then closes the packet channels.
func (r *RTSPReader) Close() error {
	r.mu.Lock()
	c := r.conn
	r.conn = nil
	r.mu.Unlock()
	if c != nil {
		return c.Close()
	}
	return nil
}

