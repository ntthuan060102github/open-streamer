package runtime

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/ntt0601zcoder/open-streamer/config"
)

// transcoderRequiresRestart is the gate that decides whether toggling a
// global transcoder field forces every running stream to bounce. The intent:
// only fields that change in-flight FFmpeg behaviour cost a restart.
//
// FFmpegPath is the only behaviour-affecting field left at the global level
// since the multi-output toggle moved to per-stream config (Stream.Transcoder.Mode).
// Future hot-swappable fields should be added alongside the field itself.
func TestTranscoderRequiresRestart(t *testing.T) {
	t.Parallel()

	const defaultFFmpeg = "/usr/bin/ffmpeg"

	cases := []struct {
		name string
		old  *config.TranscoderConfig
		new  *config.TranscoderConfig
		want bool
	}{
		{
			name: "both nil → no restart",
			old:  nil,
			new:  nil,
			want: false,
		},
		{
			name: "FFmpegPath unchanged → no restart",
			old:  &config.TranscoderConfig{FFmpegPath: defaultFFmpeg},
			new:  &config.TranscoderConfig{FFmpegPath: defaultFFmpeg},
			want: false,
		},
		{
			name: "FFmpegPath swap requires restart",
			old:  &config.TranscoderConfig{FFmpegPath: defaultFFmpeg},
			new:  &config.TranscoderConfig{FFmpegPath: "/opt/ffmpeg/bin/ffmpeg"},
			want: true,
		},
		{
			name: "nil → empty config → no restart",
			old:  nil,
			new:  &config.TranscoderConfig{},
			want: false,
		},
		{
			name: "nil → FFmpegPath set requires restart",
			old:  nil,
			new:  &config.TranscoderConfig{FFmpegPath: defaultFFmpeg},
			want: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, transcoderRequiresRestart(tc.old, tc.new))
		})
	}
}
