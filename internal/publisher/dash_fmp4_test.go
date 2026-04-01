package publisher

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseDashSegMediaNumber(t *testing.T) {
	t.Parallel()
	cases := []struct {
		kind byte
		name string
		want int
	}{
		{'v', "seg_v_00054.m4s", 54},
		{'v', "seg_v_00133.m4s", 133},
		{'a', "seg_a_00001.m4s", 1},
		{'a', "seg_a_00012.m4s", 12},
		{'v', "seg_a_00054.m4s", 0},
		{'v', "seg_v_54.m4s", 54},
		{'v', "wrong.mp4", 0},
		{'v', "", 0},
		{'v', "seg_v_notint.m4s", 0},
	}
	for i, tc := range cases {
		t.Run(fmt.Sprintf("%d_%c_%q", i, tc.kind, tc.name), func(t *testing.T) {
			t.Parallel()
			got := parseDashSegMediaNumber(tc.kind, tc.name)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestWindowTailUint64(t *testing.T) {
	t.Parallel()
	vals := []uint64{10, 20, 30, 40, 50}

	t.Run("n<=0 returns original", func(t *testing.T) {
		t.Parallel()
		require.Equal(t, vals, windowTailUint64(vals, 0))
		require.Equal(t, vals, windowTailUint64(vals, -1))
	})

	t.Run("empty", func(t *testing.T) {
		t.Parallel()
		require.Nil(t, windowTailUint64(nil, 3))
	})

	t.Run("tail three", func(t *testing.T) {
		t.Parallel()
		require.Equal(t, []uint64{30, 40, 50}, windowTailUint64(vals, 3))
	})

	t.Run("n larger than len", func(t *testing.T) {
		t.Parallel()
		require.Equal(t, vals, windowTailUint64(vals, 100))
	})
}

func TestH264SampleDur90k(t *testing.T) {
	t.Parallel()
	dts := []uint64{1000, 1033, 1066} // ms, ~33ms steps ~ 3000 ticks at 90k
	d0 := h264SampleDur90k(dts, 0)
	require.Greater(t, d0, uint32(0))
	d1 := h264SampleDur90k(dts, 1)
	require.Greater(t, d1, uint32(0))
	// last frame uses previous delta
	d2 := h264SampleDur90k(dts, 2)
	require.Greater(t, d2, uint32(0))
}

func TestH264SampleDur90kSingleFrame(t *testing.T) {
	t.Parallel()
	dts := []uint64{5000}
	d := h264SampleDur90k(dts, 0)
	require.GreaterOrEqual(t, d, uint32(1))
}

func TestTotalQueuedVideoDur90k(t *testing.T) {
	t.Parallel()
	dts := []uint64{0, 10, 20} // ms
	sum := totalQueuedVideoDur90k(dts)
	var manual uint64
	for i := range dts {
		manual += uint64(h264SampleDur90k(dts, i))
	}
	require.Equal(t, manual, sum)
	require.Greater(t, sum, uint64(0))
}

func TestDashFMP4AudioFramesPerSegment(t *testing.T) {
	t.Parallel()
	t.Run("48kHz 2s ceiling to AAC frames", func(t *testing.T) {
		t.Parallel()
		p := &dashFMP4Packager{audioSR: 48000, segSec: 2}
		// (96000 + 1023) / 1024 == 94
		require.Equal(t, 94, p.audioFramesPerSegment())
	})
	t.Run("44.1kHz 2s", func(t *testing.T) {
		t.Parallel()
		p := &dashFMP4Packager{audioSR: 44100, segSec: 2}
		require.Equal(t, 87, p.audioFramesPerSegment())
	})
	t.Run("zero sample rate", func(t *testing.T) {
		t.Parallel()
		p := &dashFMP4Packager{audioSR: 0, segSec: 2}
		require.Equal(t, 1, p.audioFramesPerSegment())
	})
}
