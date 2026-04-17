package tsmux

import (
	"testing"

	"github.com/ntt0601zcoder/open-streamer/internal/domain"
)

func TestWriteNilOrEmptyIsNoOp(t *testing.T) {
	f := NewFromAV()
	called := false
	f.Write(nil, func(_ []byte) { called = true })
	f.Write(&domain.AVPacket{}, func(_ []byte) { called = true })
	f.Write(&domain.AVPacket{Codec: domain.AVCodecH264, Data: []byte{0xAA}}, nil)
	if called {
		t.Fatal("emit must not be called for nil/empty inputs")
	}
}

func TestWriteUnknownCodecIsNoOp(t *testing.T) {
	f := NewFromAV()
	called := false
	f.Write(
		&domain.AVPacket{Codec: domain.AVCodecUnknown, Data: []byte{0x01, 0x02}},
		func(_ []byte) { called = true },
	)
	if called {
		t.Fatal("unknown codec must not emit")
	}
}

func TestWriteH264EmitsTSPackets(t *testing.T) {
	f := NewFromAV()
	// Minimal AVCC -> Annex-B doesn't matter for the muxer; gomedia accepts whatever bytes.
	annexB := []byte{
		0x00, 0x00, 0x00, 0x01, 0x67, 0x42, 0x00, 0x1f, // SPS
		0x00, 0x00, 0x00, 0x01, 0x68, 0xce, 0x38, 0x80, // PPS
		0x00, 0x00, 0x00, 0x01, 0x65, 0x88, 0x84, 0x00, // IDR
	}
	var emitted [][]byte
	f.Write(
		&domain.AVPacket{Codec: domain.AVCodecH264, Data: annexB, PTSms: 100, DTSms: 100, KeyFrame: true},
		func(b []byte) { emitted = append(emitted, append([]byte(nil), b...)) },
	)
	if len(emitted) == 0 {
		t.Fatal("expected H.264 muxer to produce TS packets")
	}
	for i, p := range emitted {
		if len(p) != 188 {
			t.Errorf("pkt %d not 188 bytes: %d", i, len(p))
		}
		if p[0] != 0x47 {
			t.Errorf("pkt %d bad sync: %x", i, p[0])
		}
	}
}

func TestWriteAACEmitsTSPackets(t *testing.T) {
	f := NewFromAV()
	// 7-byte ADTS header + dummy AAC payload.
	aac := []byte{0xFF, 0xF1, 0x4C, 0x80, 0x01, 0x1F, 0xFC, 0x21, 0x00, 0x00}
	var n int
	f.Write(
		&domain.AVPacket{Codec: domain.AVCodecAAC, Data: aac, PTSms: 50},
		func(_ []byte) { n++ },
	)
	if n == 0 {
		t.Fatal("expected AAC muxer to emit TS packets")
	}
}

func TestWriteH265EmitsTSPackets(t *testing.T) {
	f := NewFromAV()
	hevc := []byte{
		0x00, 0x00, 0x00, 0x01, 0x40, 0x01, 0x0c, 0x01, // VPS
		0x00, 0x00, 0x00, 0x01, 0x42, 0x01, 0x01, 0x01, // SPS
		0x00, 0x00, 0x00, 0x01, 0x26, 0x01, 0xAB, // IDR slice (NAL type 19)
	}
	var n int
	f.Write(
		&domain.AVPacket{Codec: domain.AVCodecH265, Data: hevc, PTSms: 200, DTSms: 200, KeyFrame: true},
		func(_ []byte) { n++ },
	)
	if n == 0 {
		t.Fatal("expected H.265 muxer to emit TS packets")
	}
}

func TestKeyFrameH264(t *testing.T) {
	idr := []byte{0x00, 0x00, 0x00, 0x01, 0x65, 0x88, 0x84, 0x00}
	nonIDR := []byte{0x00, 0x00, 0x00, 0x01, 0x41, 0xAB, 0xCD}

	if !KeyFrameH264(idr) {
		t.Error("IDR slice should be detected as keyframe")
	}
	if KeyFrameH264(nonIDR) {
		t.Error("non-IDR slice should not be a keyframe")
	}
}

func TestKeyFrameH265(t *testing.T) {
	// IDR_W_RADL has nal_unit_type=19. Annex-B start code + nal_unit_header.
	idr := []byte{0x00, 0x00, 0x00, 0x01, 0x26, 0x01, 0xAB}
	nonIDR := []byte{0x00, 0x00, 0x00, 0x01, 0x02, 0x01, 0x00}

	if !KeyFrameH265(idr) {
		t.Error("IDR_W_RADL should be detected as keyframe")
	}
	if KeyFrameH265(nonIDR) {
		t.Error("trailing slice should not be a keyframe")
	}
}

func TestFeedWirePacketTSPassthrough(t *testing.T) {
	var got []byte
	var mux *FromAV
	FeedWirePacket(
		[]byte{0x47, 0x01, 0x02, 0x03},
		nil,
		&mux,
		func(b []byte) { got = append(got, b...) },
	)
	if mux != nil {
		t.Error("mux must not be allocated for raw TS path")
	}
	if len(got) != 4 || got[0] != 0x47 {
		t.Fatalf("TS not forwarded verbatim: %v", got)
	}
}

func TestFeedWirePacketAVAllocatesMuxLazily(t *testing.T) {
	var n int
	var mux *FromAV

	annexB := []byte{
		0x00, 0x00, 0x00, 0x01, 0x67, 0x42, 0x00, 0x1f,
		0x00, 0x00, 0x00, 0x01, 0x68, 0xce, 0x38, 0x80,
		0x00, 0x00, 0x00, 0x01, 0x65, 0x88, 0x84, 0x00,
	}
	FeedWirePacket(
		nil,
		&domain.AVPacket{Codec: domain.AVCodecH264, Data: annexB, PTSms: 1, DTSms: 1, KeyFrame: true},
		&mux,
		func(_ []byte) { n++ },
	)
	if mux == nil {
		t.Fatal("mux should be allocated on first AV packet")
	}
	if n == 0 {
		t.Fatal("expected emit calls from muxed AV packet")
	}

	saved := mux
	FeedWirePacket(
		nil,
		&domain.AVPacket{Codec: domain.AVCodecH264, Data: annexB, PTSms: 2, DTSms: 2, KeyFrame: true},
		&mux,
		func(_ []byte) {},
	)
	if mux != saved {
		t.Error("mux must be reused, not reallocated")
	}
}

func TestFeedWirePacketNilInputs(t *testing.T) {
	var mux *FromAV
	called := false
	FeedWirePacket(nil, nil, &mux, func(_ []byte) { called = true })
	FeedWirePacket(nil, &domain.AVPacket{}, &mux, func(_ []byte) { called = true })
	FeedWirePacket(nil, &domain.AVPacket{Codec: domain.AVCodecH264, Data: []byte{1}}, &mux, nil)
	if called {
		t.Fatal("emit must not be called for empty inputs")
	}
	if mux != nil {
		t.Fatal("mux must not be allocated for empty AV packet")
	}
}
