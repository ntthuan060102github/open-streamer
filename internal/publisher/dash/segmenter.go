package dash

import "time"

// Segmenter decides when and how to cut a DASH media segment.
//
// The decision is intentionally simple compared to the v1 implementation:
//
//   - Cut at the most recent IDR once the video queue has accumulated
//     at least segDur of content. Every emitted fragment starts with a
//     keyframe (DASH startWithSAP="1").
//   - Cut on a wallclock safety-net (segDur × maxFactor since the last
//     cut) when no IDR has arrived — produces a non-IDR-aligned
//     fragment but prevents the live edge from stalling forever on a
//     pathological long-GOP source.
//   - Audio is sample-count-aligned: drain whichever audio frames have
//     PTSms in the video segment's time range.
//
// No drift cap, no inter-frame jump guard, no skip-until-IDR latch — the
// Normaliser anchors timestamps wallclock-accurately upstream, and the
// segmenter trusts what arrives. If something is wrong with the input,
// it's a Normaliser-level bug and gets fixed there, not papered over
// here with a defense layer.
type Segmenter struct {
	// segDur is the target segment duration (e.g. 2s for low-latency,
	// 4s for normal HLS/DASH live).
	segDur time.Duration
	// maxFactor multiplies segDur to set the safety-net wallclock
	// deadline. 3-4× is a good balance: tolerates long-GOP sources
	// (8-12s GOP for a 2s target) without producing many non-IDR cuts.
	maxFactor int

	lastCut time.Time

	// audioFrameResidualSamples accumulates the fractional-frame
	// remainder of V/A duration coupling across cuts. AAC frames are
	// exactly 1024 samples, but V dur × sr / 1000 rarely lands on a
	// 1024-sample boundary (e.g. 5 s @ 48 kHz = 240 000 samples =
	// 234.375 frames). Truncating the target frame count produces a
	// fixed per-cut deficit that compounds into V/A drift over the
	// session (test2 saw 1.23 s drift after 88 cuts at 8 ms/cut).
	// Accumulating the sub-frame remainder and bumping targetA up by
	// one frame whenever the residual exceeds 1024 samples drives the
	// cumulative error to zero.
	audioFrameResidualSamples uint64
}

// NewSegmenter returns a Segmenter targeting segDur per segment with
// maxFactor*segDur as the no-IDR safety deadline.
func NewSegmenter(segDur time.Duration, maxFactor int) *Segmenter {
	if maxFactor <= 1 {
		maxFactor = 3
	}
	return &Segmenter{segDur: segDur, maxFactor: maxFactor}
}

// MarkCut records that a segment has just been emitted. The safety-net
// deadline resets from now. Callers invoke this immediately after a
// successful write so the next cut is measured from the new wallclock.
func (s *Segmenter) MarkCut(now time.Time) {
	s.lastCut = now
}

// Reset clears any cut state — used at WaitingForPairing → Live or at
// SessionBoundary to ensure the first post-reset cut is measured from
// the new live timeline.
func (s *Segmenter) Reset() {
	s.lastCut = time.Time{}
	s.audioFrameResidualSamples = 0
}

// CutDecision reports whether the segmenter wants a cut now and what
// frames to drain. ok=false means "keep buffering".
type CutDecision struct {
	// Ok is true when a cut should happen; false when the segmenter
	// wants more frames.
	Ok bool

	// VideoCount is the number of OLDEST video frames to drain from
	// the queue. The last drained frame is the frame just BEFORE the
	// trailing IDR — the IDR itself STAYS queued so the NEXT segment
	// starts at a clean SAP boundary (DASH startWithSAP="1"). Without
	// this off-by-one, strict players (Safari/VT, Chrome-on-Mac
	// hardware decode) reject every segment after the first with
	// MEDIA_ERR_DECODE -12909, because each segment after #1 starts
	// with a P/B slice whose reference frames live in the previous
	// segment.
	//
	// On a safety-net cut with no IDR available, VideoCount = the
	// entire video queue at the moment of the cut.
	VideoCount int

	// AudioCount is the number of OLDEST audio frames to drain. Equal
	// to the count whose PTSms ≤ the last drained video frame's PTSms;
	// callers may overshoot by one frame to span the cut moment
	// cleanly. On audio-only or video-only streams the missing track
	// contributes 0.
	AudioCount int

	// IsIDRAligned reports whether the cut lands at an IDR boundary
	// (clean SAP) or a wallclock safety-net (non-SAP). Manifests can
	// emit startWithSAP="1" globally regardless — the safety-net case
	// is rare and players tolerate the occasional non-SAP fragment.
	IsIDRAligned bool
}

// Cut consults the queue and decides whether to emit a segment now.
// Pure function (no clock side-effect — caller MarkCut() to advance).
//
// Rules, in order of preference:
//
//  1. Video queue contains ≥ segDur of PTSms span AND has an IDR after
//     the first segDur worth of content: cut up to and including the
//     LAST IDR within the segDur+ window. IDR-aligned, no audio loss.
//  2. Video queue spans ≥ maxFactor × segDur with no usable IDR AND
//     enough wallclock has elapsed since lastCut: safety-net cut.
//     Drains everything queued; the next segment starts wherever the
//     next IDR appears.
//  3. Audio-only stream (no video init / no video frames): cut on
//     audio span ≥ segDur.
//  4. Otherwise: hold.
//
// audioSR is the audio sample rate (Hz). 0 if audio isn't packed —
// then the V/A coordination math is skipped and AudioCount stays 0.
//
// cutAudioOnly fires only when the stream is genuinely audio-only
// (haveVideo == false). For a V+A packager whose video queue is
// temporarily empty (cut just popped V frames, next V hasn't arrived
// yet, but audio frames keep arriving), the segmenter HOLDS the cut
// until V arrives. Without this guard, an A-only cut would emit
// `aSegN++` without `vSegN++`, baking a permanent segment-number
// divergence (V startN=N, A startN=N+1) that browser DASH players
// align by segment number — observed on test2 as a 5.4-second
// content offset visible as severe lip-sync drift even though the
// manifest tfdt gap was sub-second.
func (s *Segmenter) Cut(now time.Time, q *FrameQueue, haveVideo, haveAudio bool, audioSR uint64) CutDecision {
	segDurMS := uint64(s.segDur.Milliseconds()) //nolint:gosec // segDur > 0 by construction
	maxElapsedMS := segDurMS * uint64(s.maxFactor)

	switch {
	case haveVideo && q.VideoLen() > 0:
		return s.cutVideo(now, q, segDurMS, maxElapsedMS, haveAudio, audioSR)
	case !haveVideo && haveAudio && q.AudioLen() > 0:
		return s.cutAudioOnly(q, segDurMS)
	}
	return CutDecision{}
}

// cutVideo handles the dual-track or video-only stream. Returns the
// IDR-aligned cut when possible, the safety-net cut when wallclock has
// run out, or {Ok:false} when more content is needed.
func (s *Segmenter) cutVideo(now time.Time, q *FrameQueue, segDurMS, maxElapsedMS uint64, haveAudio bool, audioSR uint64) CutDecision {
	idx := s.findIDRCutPoint(q, segDurMS)
	if idx >= 0 {
		return s.buildCutDecision(q, idx, haveAudio, true, audioSR)
	}

	// Safety-net: enough wallclock elapsed since last cut without an
	// IDR landing inside the desired window. Use the caller-provided
	// `now` so tests with a synthetic clock measure deterministically.
	if !s.lastCut.IsZero() {
		elapsed := now.Sub(s.lastCut).Milliseconds()
		if elapsed > 0 && uint64(elapsed) >= maxElapsedMS { //nolint:gosec // bounded by branch above
			return s.buildCutDecision(q, q.VideoLen()-1, haveAudio, false, audioSR)
		}
	}
	// Cold-start safety-net: lastCut is zero (first cut ever). Allow
	// the queue to grow up to maxElapsedMS of PTS span before forcing
	// a cut; before that, wait for an IDR.
	if s.lastCut.IsZero() && q.VideoSpanMS() >= maxElapsedMS {
		return s.buildCutDecision(q, q.VideoLen()-1, haveAudio, false, audioSR)
	}
	return CutDecision{}
}

// findIDRCutPoint searches for the FIRST IDR whose PTSms is at least
// segDurMS past the first frame, and returns the index of the frame
// JUST BEFORE that IDR — i.e. the last frame to include in the
// outgoing segment. The IDR itself stays queued so the NEXT segment
// starts at a clean SAP boundary. Returns -1 when no qualifying IDR
// is queued.
//
// Cut-just-before-IDR (vs cut-including-IDR) is mandatory for DASH
// startWithSAP="1": every emitted segment must start with an IDR.
// The previous return-i form had the IDR closing the current segment,
// leaving the next segment to start with a P/B slice whose reference
// frames lived in the previous segment. Tolerant players (Firefox,
// software-decode Chrome) accepted that and used the prior segment's
// IDR for refs; strict pipelines (Safari/VideoToolbox, Chrome-on-Mac
// HW decode) hard-fail with MEDIA_ERR_DECODE -12909
// (VTDecompressionOutputCallback bad-data) on every segment after #0.
//
// Picking the FIRST IDR past segDur (rather than the LATEST in queue)
// keeps each segment ≤ ~segDur in frame-PTS span, which matters
// because the published segment dur (= frame-PTS span) must NOT exceed
// the inter-cut wallclock interval — otherwise the MPD's per-segment
// (t, d) values would describe overlapping ranges and strict players
// reject the manifest. Production hit this exact problem on HLS-pull
// sources: V queue accumulated 6+ seconds of frames between cuts (HLS
// chunks deliver a segment at a time), and the latest-IDR pick
// produced 6.5 s of frame span for a cut that should have spanned
// ~5 s wallclock.
//
// First-IDR-past-segDur trades latency for correctness: short-GOP
// sources may produce slightly smaller-than-segDur segments, but the
// timeline always stays non-overlapping.
func (s *Segmenter) findIDRCutPoint(q *FrameQueue, segDurMS uint64) int {
	first, ok := q.FirstVideo()
	if !ok {
		return -1
	}
	if q.VideoSpanMS() < segDurMS {
		return -1
	}
	// Scan from oldest to newest; the FIRST IDR whose PTS is >=
	// first+segDur defines the cut boundary, and we return the index
	// of the frame BEFORE it so the IDR stays for the next segment.
	for i := 1; i < q.VideoLen(); i++ {
		ptsMS, ok := q.videoPTSAt(i)
		if !ok {
			continue
		}
		if ptsMS-first.PTSms < segDurMS {
			continue
		}
		if vf, ok := q.videoAt(i); ok && vf.IsIDR {
			return i - 1
		}
	}
	return -1
}

// buildCutDecision packages the cut-up-to-frame-N answer. For a V+A
// stream, AudioCount is COUPLED to V's emitted duration so the
// per-segment V/A durations match in time-domain — the cure for the
// stream_a_raw 8 s / test2 4 s baked-in drift bug:
//
//   - V's emitted ms = (nextPTSms − first.PTSms) if a next frame is
//     queued; else (cutPTSms − first.PTSms).
//   - Target audio frames = round(V_ms × audioSR / 1024 / 1000); each
//     AAC frame is exactly 1024 samples.
//   - If the audio queue holds at least that many frames, AudioCount =
//     the target, and writeAudioSegment's dur math (N × 1024 ticks)
//     produces exactly the same time-domain duration as V's segment.
//   - If the audio queue is short, return Ok=false to HOLD the cut —
//     the next tick (50 ms) will retry. Stable-state sources never
//     trigger the hold; only catch-up bursts on HLS-pull / file
//     sources need it, and there only at startup.
//
// audioSR == 0 means caller wants the legacy count-by-PTS path (audio
// isn't packed in this shard); aCount stays at 0.
func (s *Segmenter) buildCutDecision(q *FrameQueue, lastVideoIdx int, haveAudio bool, idrAligned bool, audioSR uint64) CutDecision {
	if lastVideoIdx < 0 {
		return CutDecision{}
	}
	vCount := lastVideoIdx + 1
	aCount := 0
	if haveAudio && audioSR > 0 {
		first, ok1 := q.FirstVideo()
		if !ok1 {
			return CutDecision{}
		}
		nextPTSms, hasNext := q.videoPTSAt(vCount)
		var vDurMs uint64
		if hasNext && nextPTSms > first.PTSms {
			vDurMs = nextPTSms - first.PTSms
		} else if cutPTSms, ok := q.videoPTSAt(lastVideoIdx); ok && cutPTSms > first.PTSms {
			// Fallback: no frame queued past lastVideoIdx. Use
			// (cutPTSms − first.PTSms) PLUS one estimated per-frame
			// interval so the duration matches what computeVideoSegDurTicks
			// will write to the MPD (which also adds perFrame for the
			// last frame's own duration). Without this, my V/A coupling
			// targetA under-counts by one frame per cut and audio drifts
			// behind video — observed on test2 file://MP4 where the V
			// queue typically drains completely each cut, making this
			// fallback the common case (~40 ms/cut compound deficit).
			vDurMs = cutPTSms - first.PTSms
			if vCount > 1 {
				vDurMs += vDurMs / uint64(vCount-1) //nolint:gosec // vCount > 1 by branch
			}
		}
		if vDurMs > 0 {
			// Exact total samples that match V's duration:
			//   samples = V_dur_ms × sr / 1000
			// Round to whole 1024-sample AAC frames carrying the
			// fractional remainder forward in s.audioFrameResidualSamples
			// so cumulative A duration converges on cumulative V
			// duration regardless of how V dur aligns vs the 1024-sample
			// boundary (root cause of test2's 8 ms-per-cut V/A drift
			// before this residual accumulator).
			totalSamples := vDurMs*audioSR/1000 + s.audioFrameResidualSamples
			targetA := int(totalSamples / 1024) //nolint:gosec
			residual := totalSamples - uint64(targetA)*1024
			if q.AudioLen() < targetA {
				// Hold cut — audio queue hasn't caught up to V's range.
				// Next tick retries. Without this hold, V and A would
				// emit different durations and sequential tfdt would
				// bake permanent drift. The residual is NOT advanced
				// on hold; we re-compute it next tick from the same V
				// dur so retries see the same target.
				return CutDecision{}
			}
			aCount = targetA
			s.audioFrameResidualSamples = residual
		}
	}
	return CutDecision{
		Ok:           true,
		VideoCount:   vCount,
		AudioCount:   aCount,
		IsIDRAligned: idrAligned,
	}
}

// cutAudioOnly handles audio-only streams. Cuts when audio span >= segDur.
//
// Audio-segment size is bounded to ONE segDur worth of frames so the
// emitted MPD <S d=…> stays close to the operator-configured target
// (vs the entire queue, which can be up to maxQueueSpanMs = 30 s of
// audio after a startup burst — that produced 28-second audio
// segments on stream_a / test_copy ABR audio shards when the video
// queue happened to drain between V cuts and cutAudioOnly fired).
//
// Returns Ok=true with AudioCount = the number of frames whose
// PTSms ≤ first.PTSms + segDurMS, guaranteeing forward progress
// even when the queue has only the first frame.
func (s *Segmenter) cutAudioOnly(q *FrameQueue, segDurMS uint64) CutDecision {
	first, ok := q.FirstAudio()
	if !ok || q.AudioSpanMS() < segDurMS {
		return CutDecision{}
	}
	count := q.audioCountAtOrBefore(first.PTSms + segDurMS)
	if count == 0 {
		count = 1 // guarantee forward progress; should not normally hit
	}
	return CutDecision{
		Ok:           true,
		VideoCount:   0,
		AudioCount:   count,
		IsIDRAligned: true, // audio-only is always cleanly cuttable
	}
}
