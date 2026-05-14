package native

import (
	"testing"

	"github.com/ntt0601zcoder/open-streamer/internal/domain"
)

// TestAudioEncoderName verifies the domain.AudioCodec → libavcodec
// encoder-name lookup. Empty defaults to aac (same as the CLI
// backend's fallback when audio.codec is unset).
func TestAudioEncoderName(t *testing.T) {
	cases := []struct {
		codec domain.AudioCodec
		want  string
	}{
		{"", "aac"},
		{domain.AudioCodecAAC, "aac"},
		{domain.AudioCodecMP2, "mp2"},
		{domain.AudioCodecMP3, "libmp3lame"},
		{domain.AudioCodecAC3, "ac3"},
		{domain.AudioCodecEAC3, "eac3"},
		// Unknown values pass through verbatim so operators can pin
		// an explicit encoder name without expanding the dispatch.
		{"custom_encoder", "custom_encoder"},
	}
	for _, tc := range cases {
		t.Run(string(tc.codec), func(t *testing.T) {
			if got := audioEncoderName(tc.codec); got != tc.want {
				t.Errorf("audioEncoderName(%q) = %q, want %q", tc.codec, got, tc.want)
			}
		})
	}
}

// TestDefaultChannelLayout sanity-checks the operator-channel-count
// → libav layout pick. Anything not in {1, 2, 6} falls back to
// stereo since the count alone is ambiguous (4 could be quad / 3.1).
func TestDefaultChannelLayout(t *testing.T) {
	if got := defaultChannelLayout(1).String(); got == "" {
		t.Errorf("expected non-empty layout for mono, got empty")
	}
	if got := defaultChannelLayout(2).String(); got == "" {
		t.Errorf("expected non-empty layout for stereo, got empty")
	}
	if got := defaultChannelLayout(6).String(); got == "" {
		t.Errorf("expected non-empty layout for 5.1, got empty")
	}
	// Unknown channel count → must match the stereo layout exactly.
	stereo := defaultChannelLayout(2)
	unknown := defaultChannelLayout(99)
	if stereo.String() != unknown.String() {
		t.Errorf("expected stereo fallback for channels=99, got %q (stereo is %q)",
			unknown.String(), stereo.String())
	}
}

// TestNeedsAudioReencode_NilSrcGuard documents that needsAudioReencode
// must not be called with a nil src — callers (initAudio) gate on
// audioStream != nil first. This test just confirms the audio.Copy
// short-circuit lands before any src dereference.
func TestNeedsAudioReencode_CopyShortCircuit(t *testing.T) {
	// Copy=true must take the fast path without touching src.
	got := needsAudioReencode(domain.AudioTranscodeConfig{Copy: true}, nil)
	if got {
		t.Errorf("Copy=true must not require re-encode")
	}

	// Codec=copy is the same as Copy=true at the wire level — also
	// must short-circuit before src is read.
	got = needsAudioReencode(domain.AudioTranscodeConfig{Codec: domain.AudioCodecCopy}, nil)
	if got {
		t.Errorf("Codec=copy must not require re-encode")
	}
}
