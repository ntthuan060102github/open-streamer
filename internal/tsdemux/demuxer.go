// Package tsdemux is a thin callback-style wrapper around
// github.com/asticode/go-astits's pull-style Demuxer. The wrapper
// preserves the OnFrame/Input call shape that the rest of
// Open-Streamer used while we were on gomedia, so consumers don't
// need to refactor their goroutine model.
//
// One Demuxer per input pipeline (HLS-pull reader, DASH packager,
// RTMP/RTSP publisher demux, …). Re-use across stream restarts by
// constructing a fresh Demuxer per session boundary — the underlying
// astits.Demuxer accumulates PSI / PES assembly state and must be
// rebuilt on a fresh source.
package tsdemux

import (
	"context"
	"errors"
	"io"

	"github.com/asticode/go-astits"
)

// StreamType is a re-export of astits.StreamType so callers can use
// tsdemux.StreamTypeH264 etc. without importing astits directly.
type StreamType = astits.StreamType

// Canonical stream types we care about. astits exposes many more —
// add aliases here as we start supporting them. MPEG-1 / MPEG-2
// audio aliases are kept around for downstream code that wants to
// explicitly mark them as "unsupported, intentionally drop" in
// codec switches.
const (
	StreamTypeH264       = astits.StreamTypeH264Video
	StreamTypeH265       = astits.StreamTypeH265Video
	StreamTypeAAC        = astits.StreamTypeAACAudio
	StreamTypeMPEG1Audio = astits.StreamTypeMPEG1Audio
	StreamTypeMPEG2Audio = astits.StreamTypeMPEG2Audio
)

// OnFrameFunc fires once per fully-assembled PES whose PID was
// declared in a PMT. ptsMs / dtsMs are converted from astits's 90 kHz
// ClockReference to milliseconds (the unit the rest of Open-Streamer
// uses on the AVPacket boundary).
//
// When the upstream PES uses PTSDTSIndicator=OnlyPTS (no DTS field on
// the wire — the spec's "DTS is implicitly PTS" shortcut for I/P-frame-
// only video and all AAC audio), dtsMs is filled with ptsMs so callers
// see the spec-implied value rather than zero. Returning 0 for the
// missing DTS — the original contract — meant every consumer had to
// remember the substitution; one production caller (serve_rtsp.go)
// missed it, and computeAudioRTP(0) collapsed the AAC RTP timeline
// into +1-tick monotonic-clamp emits that strict ffmpeg consumers rejected
// as non-monotonic. Doing the substitution here at the boundary makes
// the contract impossible to miss.
type OnFrameFunc func(cid StreamType, frame []byte, ptsMs, dtsMs uint64)

// Demuxer is the public type callers construct. Set OnFrame, then
// call Input(r). Input blocks until r reaches EOF or returns a read
// error; cancellation comes via closing r.
//
// Zero-value Demuxer is usable but OnFrame must be set before Input.
type Demuxer struct {
	OnFrame OnFrameFunc
}

// New constructs a fresh Demuxer. Equivalent to &Demuxer{}; provided
// for symmetry with tsmux.NewFromAV and to make the construction site
// grep-able.
func New() *Demuxer { return &Demuxer{} }

// Input consumes the TS stream from r, calling d.OnFrame for each
// PES that lands on a PID announced in a PMT. Blocks until r is
// drained or an unrecoverable read error occurs. io.EOF and
// astits.ErrNoMorePackets are treated as clean termination and
// return nil; everything else is propagated.
//
// ctx is forwarded to astits.NewDemuxer so cancellation propagates
// to the demuxer's internal state. Pass the caller's goroutine ctx —
// for the pump-based callers (tsdemux_packet_reader, dash/demux,
// publisher serve / push) this is the worker's read-loop ctx.
//
// Stream-type lookup: astits's pull API returns PSI + PES as
// independent DemuxerData records. We accumulate PID → StreamType
// from PMT records and consult it on each PES. PES that arrive
// before the first PMT are dropped (no stream type known) —
// matches gomedia's behaviour and the spec's expectation that
// PMT precedes its referenced PES.
func (d *Demuxer) Input(ctx context.Context, r io.Reader) error {
	if d.OnFrame == nil {
		return errors.New("tsdemux: OnFrame not set")
	}
	dmx := astits.NewDemuxer(ctx, r)
	pidStream := make(map[uint16]astits.StreamType)
	for {
		data, err := dmx.NextData()
		if err != nil {
			if errors.Is(err, astits.ErrNoMorePackets) || errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if data.PMT != nil {
			for _, es := range data.PMT.ElementaryStreams {
				pidStream[es.ElementaryPID] = es.StreamType
			}
			continue
		}
		if data.PES == nil {
			continue
		}
		st, ok := pidStream[data.PID]
		if !ok {
			continue
		}
		ptsMs, dtsMs := pesTimestampsMs(data.PES)
		d.OnFrame(st, data.PES.Data, ptsMs, dtsMs)
	}
}

// pesTimestampsMs converts a PES's optional-header PTS / DTS from
// astits's 90 kHz clock reference to milliseconds. Returns (0, 0)
// when the PES has no optional header (rare but valid for some
// stream types) or when the PTS field is absent.
//
// When the PES uses PTSDTSIndicator=OnlyPTS (no DTS field on the
// wire), dtsMs is filled with ptsMs — see OnFrameFunc's docstring
// for why this lives at the boundary rather than at every caller.
func pesTimestampsMs(p *astits.PESData) (ptsMs, dtsMs uint64) {
	if p == nil || p.Header == nil || p.Header.OptionalHeader == nil {
		return 0, 0
	}
	oh := p.Header.OptionalHeader
	if oh.PTS != nil {
		ptsMs = uint64(oh.PTS.Base) / 90 //nolint:gosec // clock base is non-negative for valid PES
	}
	if oh.DTS != nil {
		dtsMs = uint64(oh.DTS.Base) / 90 //nolint:gosec
	} else {
		// OnlyPTS PES — per ISO/IEC 13818-1 § 2.4.3.7 the absence of
		// the DTS field means DTS equals PTS. Substitute here so every
		// downstream consumer sees the spec value (instead of zero).
		dtsMs = ptsMs
	}
	return ptsMs, dtsMs
}
