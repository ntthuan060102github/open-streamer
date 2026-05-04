package pull

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Helpers ---------------------------------------------------------------------

// buildRTPPacket produces a minimal RTP packet (fixed 12-byte header) plus payload.
func buildRTPPacket(payload []byte) []byte {
	hdr := make([]byte, 0, 12+len(payload))
	hdr = append(hdr,
		0x80, 0x21, // V=2, P=0, X=0, CC=0, M=0, PT=33
		0x00, 0x01, // seq
		0x00, 0x00, 0x00, 0x00, // timestamp
		0x00, 0x00, 0x00, 0x00, // SSRC
	)
	return append(hdr, payload...)
}

// buildRTPPacketWithCSRC prepends cc CSRC identifiers (each 4 bytes).
func buildRTPPacketWithCSRC(cc int, payload []byte) []byte {
	b0 := byte(0x80 | (cc & 0x0F)) // V=2, CC=cc
	hdr := []byte{
		b0, 0x21,
		0x00, 0x01,
		0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00,
	}
	for range cc {
		hdr = append(hdr, 0x00, 0x00, 0x00, 0x00)
	}
	return append(hdr, payload...)
}

// buildRTPPacketWithExt adds a 4-byte extension header + extWords × 4 bytes.
func buildRTPPacketWithExt(extWords int, payload []byte) []byte {
	hdr := []byte{
		0x90, 0x21, // V=2, X=1
		0x00, 0x01,
		0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00,
	}
	hdr = append(hdr, 0xAB, 0xCD) // profile-defined
	hdr = append(hdr, byte(extWords>>8), byte(extWords&0xFF))
	for range extWords {
		hdr = append(hdr, 0x00, 0x00, 0x00, 0x00)
	}
	return append(hdr, payload...)
}

// Tests -----------------------------------------------------------------------

func TestStripRTPHeader_PlainRTP(t *testing.T) {
	t.Parallel()
	ts := []byte{0x47, 0x01, 0x02, 0x03, 0x04, 0x05}
	pkt := buildRTPPacket(ts)
	got := stripRTPHeader(pkt)
	require.NotNil(t, got)
	assert.Equal(t, ts, got)
}

func TestStripRTPHeader_TooShort(t *testing.T) {
	t.Parallel()
	assert.Nil(t, stripRTPHeader([]byte{0x80, 0x21, 0x00}))
	assert.Nil(t, stripRTPHeader(nil))
}

func TestStripRTPHeader_WrongVersion(t *testing.T) {
	t.Parallel()
	// Version 1 (0x40) instead of 2 (0x80) — must be rejected.
	pkt := []byte{0x40, 0x21, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x47}
	assert.Nil(t, stripRTPHeader(pkt))
}

func TestStripRTPHeader_NonMPEGTSPayload(t *testing.T) {
	t.Parallel()
	// Payload does not start with 0x47 sync byte → rejected.
	pkt := buildRTPPacket([]byte{0xFF, 0x01, 0x02})
	assert.Nil(t, stripRTPHeader(pkt))
}

func TestStripRTPHeader_WithCSRC(t *testing.T) {
	t.Parallel()
	ts := []byte{0x47, 0x0A, 0x0B}
	// CC=3 → header is 12 + 3*4 = 24 bytes.
	pkt := buildRTPPacketWithCSRC(3, ts)
	got := stripRTPHeader(pkt)
	require.NotNil(t, got)
	assert.Equal(t, ts, got)
}

func TestStripRTPHeader_WithExtension(t *testing.T) {
	t.Parallel()
	ts := []byte{0x47, 0xDE, 0xAD, 0xBE, 0xEF}
	// 2 extension words = 8 extra bytes + 4-byte ext header → 12+4+8 = 24.
	pkt := buildRTPPacketWithExt(2, ts)
	got := stripRTPHeader(pkt)
	require.NotNil(t, got)
	assert.Equal(t, ts, got)
}

func TestStripRTPHeader_ExtensionTruncated(t *testing.T) {
	t.Parallel()
	// X=1 but only 13 bytes total → extension header can't fit.
	pkt := []byte{0x90, 0x21, 0x00, 0x01, 0, 0, 0, 0, 0, 0, 0, 0, 0x47}
	assert.Nil(t, stripRTPHeader(pkt))
}

func TestStripRTPHeader_HeaderExceedsPacket(t *testing.T) {
	t.Parallel()
	// CC=5 → header needs 12+20=32 bytes; give only 14.
	pkt := make([]byte, 14)
	pkt[0] = 0x85 // V=2, CC=5
	assert.Nil(t, stripRTPHeader(pkt))
}

// FFmpeg-compatible URL syntax: udp://<iface>@<host>:<port> uses the userinfo
// as the multicast interface name. Operators copy-paste FFmpeg command lines
// into the input URL; the alias avoids the "stream silently runs on the wrong
// interface" trap.
func TestParseUDPOpts_IfaceFromUserInfo(t *testing.T) {
	t.Parallel()
	opts := parseUDPOpts("udp://ens5@239.0.113.1:5001")
	assert.Equal(t, "ens5", opts.iface)
}

// Explicit ?iface= overrides userinfo so an operator can override an inherited
// URL without rewriting the host part.
func TestParseUDPOpts_QueryIfaceWinsOverUserInfo(t *testing.T) {
	t.Parallel()
	opts := parseUDPOpts("udp://ens5@239.0.113.1:5001?iface=eth0")
	assert.Equal(t, "eth0", opts.iface)
}

// fifo_size is FFmpeg's name for the OS recv buffer; aliases buffer_size /
// recv_buffer_size are also accepted. Operators set this to absorb burst
// jitter on high-bitrate multicast (>100 Mbps) — the previous hardcoded 4 MiB
// filled in ~270 ms and caused kernel-level UDP drops.
func TestParseUDPOpts_OSRecvBufferAliases(t *testing.T) {
	t.Parallel()
	for _, url := range []string{
		"udp://239.0.113.1:5001?fifo_size=50000000",
		"udp://239.0.113.1:5001?buffer_size=50000000",
		"udp://239.0.113.1:5001?recv_buffer_size=50000000",
	} {
		opts := parseUDPOpts(url)
		assert.Equal(t, 50_000_000, opts.osBuf, url)
	}
}

// Default OS recv buffer must be the high-bitrate-friendly 16 MiB, not the old
// 4 MiB — preventing the "no fifo_size set, drops happen" footgun.
func TestParseUDPOpts_DefaultOSBuf(t *testing.T) {
	t.Parallel()
	opts := parseUDPOpts("udp://239.0.113.1:5001")
	assert.Equal(t, udpDefaultOSBuf, opts.osBuf)
	assert.Equal(t, 16*1024*1024, opts.osBuf)
}

// chan_buf is configurable so an operator pumping a >200 Mbps source can raise
// it without recompiling.
func TestParseUDPOpts_ChanBufOverride(t *testing.T) {
	t.Parallel()
	opts := parseUDPOpts("udp://239.0.113.1:5001?chan_buf=4096")
	assert.Equal(t, 4096, opts.chanBuf)
}

// Invalid / non-positive values fall back to defaults rather than create an
// unusable reader (chanBuf=0 would deadlock the pump).
func TestParseUDPOpts_NonPositiveValuesFallBackToDefaults(t *testing.T) {
	t.Parallel()
	opts := parseUDPOpts("udp://239.0.113.1:5001?fifo_size=0&chan_buf=-5&pkt_size=abc")
	assert.Equal(t, udpDefaultOSBuf, opts.osBuf)
	assert.Equal(t, udpDefaultChanBuf, opts.chanBuf)
	assert.Equal(t, udpDefaultPktSize, opts.pktSize)
}
