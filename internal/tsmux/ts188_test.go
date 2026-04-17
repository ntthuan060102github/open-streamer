package tsmux

import (
	"bytes"
	"testing"
)

// makeTS returns n consecutive 188-byte TS packets, each filled with a unique byte
// value derived from the packet index.  Every packet starts with the 0x47 sync byte.
func makeTS(n int) []byte {
	out := make([]byte, 0, n*188)
	for i := range n {
		pkt := make([]byte, 188)
		pkt[0] = 0x47
		for j := 1; j < 188; j++ {
			pkt[j] = byte(i + 1)
		}
		out = append(out, pkt...)
	}
	return out
}

func TestDrainEmitsAlignedPackets(t *testing.T) {
	var carry []byte
	var emitted [][]byte
	emit := func(b []byte) { emitted = append(emitted, append([]byte(nil), b...)) }

	DrainTS188Aligned(&carry, makeTS(3), emit)

	if len(emitted) != 3 {
		t.Fatalf("want 3 packets, got %d", len(emitted))
	}
	for i, p := range emitted {
		if len(p) != 188 || p[0] != 0x47 {
			t.Errorf("pkt %d wrong shape: len=%d sync=%x", i, len(p), p[0])
		}
	}
	if len(carry) != 0 {
		t.Fatalf("carry should be empty after exact-multiple input, got %d bytes", len(carry))
	}
}

func TestDrainBuffersIncompletePacket(t *testing.T) {
	var carry []byte
	var n int
	emit := func(_ []byte) { n++ }

	full := makeTS(2)
	DrainTS188Aligned(&carry, full[:300], emit)
	if n != 1 {
		t.Fatalf("expected 1 emit (one full packet + remainder), got %d", n)
	}
	if len(carry) == 0 {
		t.Fatalf("expected leftover bytes in carry")
	}

	DrainTS188Aligned(&carry, full[300:], emit)
	if n != 2 {
		t.Fatalf("expected 2 emits after second feed, got %d", n)
	}
}

func TestDrainResyncsAfterGarbage(t *testing.T) {
	garbage := make([]byte, 0, 4)
	garbage = append(garbage, 0x00, 0x11, 0x22, 0x33)
	in := append(garbage, makeTS(2)...)

	var carry []byte
	var emitted [][]byte
	emit := func(b []byte) { emitted = append(emitted, append([]byte(nil), b...)) }

	DrainTS188Aligned(&carry, in, emit)
	if len(emitted) != 2 {
		t.Fatalf("want 2 packets after resync, got %d", len(emitted))
	}
}

func TestDrainDropsOldDataWhenNoSyncFound(t *testing.T) {
	junk := bytes.Repeat([]byte{0x00}, 500)

	var carry []byte
	var n int
	emit := func(_ []byte) { n++ }

	DrainTS188Aligned(&carry, junk, emit)
	if n != 0 {
		t.Fatalf("no packets should emit from pure junk, got %d", n)
	}
	// Carry must shrink to <188 bytes (truncated retry window).
	if len(carry) > 187 {
		t.Fatalf("carry not truncated: %d bytes", len(carry))
	}
}

func TestDrainSkipsFalseSyncByte(t *testing.T) {
	// 0x47 at position 0 followed by 187 bytes (false sync), then bytes that
	// do NOT start with 0x47 at position 188.  The sliding-window check
	// (carry[188] != 0x47) must drop the false sync and resync to the real
	// 0x47 at position 200.
	bad := append([]byte{0x47}, bytes.Repeat([]byte{0x00}, 199)...)
	bad = append(bad, makeTS(1)...) // real packet starts at offset 200

	var carry []byte
	var emitted [][]byte
	emit := func(b []byte) { emitted = append(emitted, append([]byte(nil), b...)) }

	DrainTS188Aligned(&carry, bad, emit)
	if len(emitted) != 1 {
		t.Fatalf("want exactly 1 real packet emitted, got %d", len(emitted))
	}
	if emitted[0][0] != 0x47 || emitted[0][1] != 0x01 {
		t.Fatalf("emitted wrong packet: %x", emitted[0][:8])
	}
}

func TestDrainNilEmitIsNoOp(t *testing.T) {
	var carry []byte
	DrainTS188Aligned(&carry, makeTS(2), nil)
	if len(carry) != 0 {
		t.Fatalf("carry mutated when emit is nil: %d", len(carry))
	}
}
