package pull

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// PIDs in the allowlist are forwarded; everything else is dropped.
func TestPIDFilter_KeepsListedDropsRest(t *testing.T) {
	t.Parallel()
	keep := buildESPacket(0x100, 0xA1)
	drop1 := buildESPacket(0x101, 0xB1)
	drop2 := buildESPacket(0x500, 0xC1)
	keep2 := buildESPacket(0x100, 0xA2)

	chunk := concat(keep, drop1, drop2, keep2)
	stub := &stubTSChunkReader{chunks: [][]byte{chunk}}
	f := NewPIDFilter(stub, []int{0x100})
	require.NoError(t, f.Open(context.Background()))

	out, err := f.Read(context.Background())
	require.NoError(t, err)
	require.Len(t, out, 188*2, "exactly 2 packets (PID 0x100) expected")

	// Order preserved.
	assert.Equal(t, byte(0xA1), out[4])
	assert.Equal(t, byte(0xA2), out[4+188])
}

// Multiple PIDs in allowlist, mixed packets — all listed PIDs forwarded
// in source order.
func TestPIDFilter_MultiplePIDs(t *testing.T) {
	t.Parallel()
	pat := buildESPacket(0x000, 0x01) // PAT packet (raw bytes, fake content)
	pmt := buildESPacket(0x100, 0x02) // PMT
	video := buildESPacket(0x101, 0x03)
	audio := buildESPacket(0x102, 0x04)
	teletext := buildESPacket(0x200, 0x05) // dropped

	chunk := concat(pat, pmt, video, audio, teletext)
	stub := &stubTSChunkReader{chunks: [][]byte{chunk}}
	f := NewPIDFilter(stub, []int{0x000, 0x100, 0x101, 0x102})
	require.NoError(t, f.Open(context.Background()))

	out, err := f.Read(context.Background())
	require.NoError(t, err)
	require.Len(t, out, 188*4, "PAT + PMT + video + audio kept; teletext dropped")
	for i, expectMarker := range []byte{0x01, 0x02, 0x03, 0x04} {
		assert.Equal(t, expectMarker, out[i*188+4],
			"packet %d marker byte mismatch", i)
	}
}

// Out-of-range or negative PIDs in the config are silently ignored — the
// filter only honors valid TS PIDs (0..8191).
func TestPIDFilter_IgnoresOutOfRangePIDs(t *testing.T) {
	t.Parallel()
	keep := buildESPacket(0x100, 0xAA)
	stub := &stubTSChunkReader{chunks: [][]byte{keep}}
	f := NewPIDFilter(stub, []int{-1, 0x2000, 0x100, 99999})
	require.NoError(t, f.Open(context.Background()))

	out, err := f.Read(context.Background())
	require.NoError(t, err)
	require.Len(t, out, 188)
	assert.Len(t, f.keep, 1, "only the valid PID 0x100 should be in keep set")
}

// When all packets in a chunk are filtered out, Read loops to next chunk
// instead of returning empty. This matches MPTSProgramFilter's contract.
func TestPIDFilter_LoopsOnEmptyOutput(t *testing.T) {
	t.Parallel()
	allDropped := concat(
		buildESPacket(0x500, 0x01),
		buildESPacket(0x501, 0x02),
	)
	wantedNext := buildESPacket(0x100, 0xAB)

	stub := &stubTSChunkReader{chunks: [][]byte{allDropped, wantedNext}}
	f := NewPIDFilter(stub, []int{0x100})
	require.NoError(t, f.Open(context.Background()))

	out, err := f.Read(context.Background())
	require.NoError(t, err)
	require.Len(t, out, 188)
	assert.Equal(t, byte(0xAB), out[4])
}

// Misaligned input (sync byte not at offset 0) is realigned via firstSync —
// the filter must not panic and must still emit kept packets.
func TestPIDFilter_HandlesMisalignedInput(t *testing.T) {
	t.Parallel()
	keep := buildESPacket(0x100, 0xCD)
	chunk := make([]byte, 0, 4+len(keep))
	chunk = append(chunk, 0xDE, 0xAD, 0xBE, 0xEF)
	chunk = append(chunk, keep...)

	stub := &stubTSChunkReader{chunks: [][]byte{chunk}}
	f := NewPIDFilter(stub, []int{0x100})
	require.NoError(t, f.Open(context.Background()))

	out, err := f.Read(context.Background())
	require.NoError(t, err)
	require.Len(t, out, 188)
	assert.Equal(t, byte(0xCD), out[4])
}

// Helper: concatenate byte slices.
func concat(slices ...[]byte) []byte {
	total := 0
	for _, s := range slices {
		total += len(s)
	}
	out := make([]byte, 0, total)
	for _, s := range slices {
		out = append(out, s...)
	}
	return out
}
