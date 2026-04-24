// Package pull — mixer.go: in-process mixer:// PacketReader.
//
// MixerReader subscribes to TWO upstream stream buffers and forwards an
// interleaved AVPacket stream taking video from the first upstream and audio
// from the second:
//
//	mixer://<video_code>,<audio_code>[?audio_failure=continue]
//
// AV sync is best-effort: each upstream's packet timestamps pass through
// unchanged. For surveillance / camera-with-radio-audio use cases this is
// sufficient; lip-sync-critical scenarios are out of scope for v1.
//
// Failure policy:
//   - Video upstream tear-down ALWAYS aborts the mixer (returns io.EOF /
//     read error → manager triggers failover or marks input degraded).
//   - Audio upstream tear-down aborts by default. With
//     `?audio_failure=continue` the reader unsubscribes from audio and keeps
//     forwarding video-only — operator opt-in for "video must stay up no
//     matter what" semantics.
//
// Both upstreams MUST be single-stream (no ABR ladder); ValidateMixerShape
// rejects ABR upstreams at write time so reaching Open with one is a config
// bug.

package pull

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync/atomic"

	"github.com/ntt0601zcoder/open-streamer/internal/buffer"
	"github.com/ntt0601zcoder/open-streamer/internal/domain"
)

// MixerReader implements ingestor.PacketReader for `mixer://` URLs.
type MixerReader struct {
	video                domain.StreamCode
	audio                domain.StreamCode
	audioFailureContinue bool
	videoBufID           domain.StreamCode
	audioBufID           domain.StreamCode
	bufSvc               *buffer.Service
	videoSub             *buffer.Subscriber
	audioSub             *buffer.Subscriber // nil after audio tear-down in continue mode
	closed               atomic.Bool
}

// NewMixerReader constructs a MixerReader from a mixer:// input. Errors:
//   - URL malformed → propagates protocol error
//   - either upstream missing in lookup → runtime error; coordinator surfaces
//     as input degraded so the manager can switch to a fallback (none allowed
//     in v1, so effectively the stream stops)
//   - either upstream has ABR transcoder → mixer requires single-stream
//     upstreams in v1; reaching here with ABR is a configuration bug
func NewMixerReader(input domain.Input, bufSvc *buffer.Service, lookup StreamLookup) (*MixerReader, error) {
	if bufSvc == nil {
		return nil, errors.New("mixer reader: nil buffer service")
	}
	if lookup == nil {
		return nil, errors.New("mixer reader: nil stream lookup")
	}

	video, audio, audioContinue, err := domain.MixerInputSpec(input)
	if err != nil {
		return nil, fmt.Errorf("mixer reader: %w", err)
	}

	upV, ok := lookup(video)
	if !ok {
		return nil, fmt.Errorf("mixer reader: video upstream %q not found", video)
	}
	upA, ok := lookup(audio)
	if !ok {
		return nil, fmt.Errorf("mixer reader: audio upstream %q not found", audio)
	}
	if streamHasRenditions(upV) {
		return nil, fmt.Errorf(
			"mixer reader: video upstream %q has an ABR ladder — single-stream upstream required in v1",
			video,
		)
	}
	if streamHasRenditions(upA) {
		return nil, fmt.Errorf(
			"mixer reader: audio upstream %q has an ABR ladder — single-stream upstream required in v1",
			audio,
		)
	}

	return &MixerReader{
		video:                video,
		audio:                audio,
		audioFailureContinue: audioContinue,
		videoBufID:           buffer.PlaybackBufferID(video, upV.Transcoder),
		audioBufID:           buffer.PlaybackBufferID(audio, upA.Transcoder),
		bufSvc:               bufSvc,
	}, nil
}

// Open subscribes to both upstream buffers. Either subscribe failure undoes
// the other (no leaked subscriber).
func (r *MixerReader) Open(ctx context.Context) error {
	_ = ctx
	if r.closed.Load() {
		return errors.New("mixer reader: open after close")
	}
	vsub, err := r.bufSvc.Subscribe(r.videoBufID)
	if err != nil {
		return fmt.Errorf("mixer reader: subscribe video %q: %w", r.videoBufID, err)
	}
	asub, err := r.bufSvc.Subscribe(r.audioBufID)
	if err != nil {
		r.bufSvc.Unsubscribe(r.videoBufID, vsub)
		return fmt.Errorf("mixer reader: subscribe audio %q: %w", r.audioBufID, err)
	}
	r.videoSub = vsub
	r.audioSub = asub
	return nil
}

// ReadPackets blocks until one upstream packet arrives or ctx is done.
//
// Selection rules:
//   - video subscriber close → io.EOF (always; downstream stream stops)
//   - audio subscriber close + audio_failure=down (default) → io.EOF
//   - audio subscriber close + audio_failure=continue → unsubscribe audio
//     and continue forwarding video-only; subsequent calls only watch video
//   - packets whose codec doesn't match the source channel are dropped
//     (e.g. video upstream emitting an audio AAC packet — we want video only
//     from the video source)
func (r *MixerReader) ReadPackets(ctx context.Context) ([]domain.AVPacket, error) {
	if r.videoSub == nil {
		return nil, errors.New("mixer reader: ReadPackets called before Open")
	}

	// Re-derive audio channel each call so nil-ing audioSub after failure
	// disables that select case (a nil receive channel never selects).
	var audioCh <-chan buffer.Packet
	if r.audioSub != nil {
		audioCh = r.audioSub.Recv()
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case pkt, ok := <-r.videoSub.Recv():
		if !ok {
			return nil, fmt.Errorf("mixer reader: video upstream %q closed: %w", r.video, io.EOF)
		}
		if pkt.AV == nil || !pkt.AV.Codec.IsVideo() {
			// Audio (or unknown) frames from the video source are dropped —
			// caller (readLoop) treats nil batch as "try again".
			return nil, nil
		}
		return []domain.AVPacket{*pkt.AV}, nil
	case pkt, ok := <-audioCh:
		if !ok {
			return r.handleAudioClose()
		}
		if pkt.AV == nil || !pkt.AV.Codec.IsAudio() {
			return nil, nil
		}
		return []domain.AVPacket{*pkt.AV}, nil
	}
}

// handleAudioClose applies the audio-failure policy when the audio
// subscriber's channel closes (upstream torn down). In `continue` mode it
// drops the audio subscriber and tells the caller to try again (nil batch);
// otherwise it returns io.EOF to abort the whole stream.
func (r *MixerReader) handleAudioClose() ([]domain.AVPacket, error) {
	if !r.audioFailureContinue {
		return nil, fmt.Errorf("mixer reader: audio upstream %q closed: %w", r.audio, io.EOF)
	}
	slog.Warn("mixer reader: audio upstream closed, continuing video-only",
		"audio_upstream", r.audio,
	)
	r.bufSvc.Unsubscribe(r.audioBufID, r.audioSub)
	r.audioSub = nil
	return nil, nil
}

// Close unsubscribes from both upstreams. Idempotent.
func (r *MixerReader) Close() error {
	if r.closed.Swap(true) {
		return nil
	}
	if r.videoSub != nil {
		r.bufSvc.Unsubscribe(r.videoBufID, r.videoSub)
	}
	if r.audioSub != nil {
		r.bufSvc.Unsubscribe(r.audioBufID, r.audioSub)
	}
	return nil
}
