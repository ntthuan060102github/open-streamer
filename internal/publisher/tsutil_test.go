package publisher

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
)

func tsPacket(seq byte) []byte {
	p := make([]byte, tsPacketSize)
	p[0] = 0x47
	p[1] = seq
	return p
}

func TestTsPacketBufFeedSinglePacket(t *testing.T) {
	t.Parallel()
	var b tsPacketBuf
	pkt := tsPacket(1)
	out := b.Feed(pkt)
	require.Len(t, out, 1)
	require.Equal(t, pkt, out[0])
	require.Empty(t, b.carry)
}

func TestTsPacketBufFeedSplitAcrossFeeds(t *testing.T) {
	t.Parallel()
	var b tsPacketBuf
	p1 := tsPacket(1)
	half := 100
	out1 := b.Feed(p1[:half])
	require.Empty(t, out1)
	out2 := b.Feed(p1[half:])
	require.Len(t, out2, 1)
	require.True(t, bytes.Equal(p1, out2[0]))
}

func TestTsPacketBufFeedMultiplePackets(t *testing.T) {
	t.Parallel()
	var b tsPacketBuf
	a, c := tsPacket(1), tsPacket(3)
	buf := append(append([]byte{}, a...), c...)
	out := b.Feed(buf)
	require.Len(t, out, 2)
	require.Equal(t, a, out[0])
	require.Equal(t, c, out[1])
}

func TestTsPacketBufFeedJunkBeforeSync(t *testing.T) {
	t.Parallel()
	var b tsPacketBuf
	p := tsPacket(9)
	prefix := []byte{0x00, 0x11, 0x22}
	out := b.Feed(append(prefix, p...))
	require.Len(t, out, 1)
	require.Equal(t, p, out[0])
}

func TestIndexSync(t *testing.T) {
	t.Parallel()
	require.Equal(t, -1, indexSync([]byte{0x00, 0x01}))
	// indexSync only considers i where i+188 <= len(b); a lone 0x47 is too short
	pktAtZero := append([]byte{0x47}, bytes.Repeat([]byte{0}, tsPacketSize-1)...)
	require.Equal(t, 0, indexSync(pktAtZero))
	long := make([]byte, 300)
	long[50] = 0x47
	require.Equal(t, 50, indexSync(long))
}
