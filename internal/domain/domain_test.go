package domain

import (
	"strings"
	"testing"
)

func TestValidateStreamCode(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantErr string
	}{
		{"empty", "", "required"},
		{"whitespace only", "   ", "required"},
		{"valid lowercase", "live", ""},
		{"valid mixed", "Live_42", ""},
		{"valid digits", "1234567890", ""},
		{"valid underscore", "_test_", ""},
		{"too long", strings.Repeat("a", MaxStreamCodeLen+1), "exceeds max length"},
		{"contains dash", "live-stream", "must contain only"},
		{"contains slash", "live/stream", "must contain only"},
		{"contains dollar", "$raw$live", "must contain only"},
		{"contains space", "live stream", "must contain only"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateStreamCode(c.in)
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("want ok, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("want error containing %q, got nil", c.wantErr)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("want error containing %q, got %q", c.wantErr, err.Error())
			}
		})
	}
}

func TestValidateStreamCodeMaxLength(t *testing.T) {
	if err := ValidateStreamCode(strings.Repeat("a", MaxStreamCodeLen)); err != nil {
		t.Fatalf("max length should pass: %v", err)
	}
}

func TestStreamValidateInputPriorities(t *testing.T) {
	cases := []struct {
		name    string
		inputs  []Input
		wantErr bool
	}{
		{"nil safe", nil, false},
		{"empty", []Input{}, false},
		{"single ok", []Input{{Priority: 0}}, false},
		{"sorted contiguous", []Input{{Priority: 0}, {Priority: 1}, {Priority: 2}}, false},
		{"non-zero start", []Input{{Priority: 1}}, true},
		{"gap", []Input{{Priority: 0}, {Priority: 2}}, true},
		{"duplicate", []Input{{Priority: 0}, {Priority: 0}}, true},
		{"out of order", []Input{{Priority: 1}, {Priority: 0}}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &Stream{Inputs: c.inputs}
			err := s.ValidateInputPriorities()
			if (err != nil) != c.wantErr {
				t.Fatalf("want err=%v, got %v", c.wantErr, err)
			}
		})
	}
}

func TestStreamValidateInputPrioritiesNilReceiver(t *testing.T) {
	var s *Stream
	if err := s.ValidateInputPriorities(); err != nil {
		t.Fatalf("nil receiver must not error: %v", err)
	}
}

func TestStreamValidateUniqueInputs(t *testing.T) {
	s := &Stream{Inputs: []Input{
		{URL: "rtmp://a"},
		{URL: "rtmp://b"},
		{URL: "  rtmp://a  "}, // duplicate after TrimSpace
	}}
	err := s.ValidateUniqueInputs()
	if err == nil {
		t.Fatal("expected duplicate error")
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("want duplicate error, got %v", err)
	}
}

func TestStreamValidateUniqueInputsAllUnique(t *testing.T) {
	s := &Stream{Inputs: []Input{
		{URL: "rtmp://a"},
		{URL: "rtmp://b"},
		{URL: "rtmp://c"},
	}}
	if err := s.ValidateUniqueInputs(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestStreamValidateUniqueInputsNilReceiver(t *testing.T) {
	var s *Stream
	if err := s.ValidateUniqueInputs(); err != nil {
		t.Fatalf("nil receiver must not error: %v", err)
	}
}

func TestAVPacketCloneNil(t *testing.T) {
	var p *AVPacket
	if c := p.Clone(); c != nil {
		t.Fatalf("nil clone must stay nil, got %+v", c)
	}
}

func TestAVPacketCloneDeepCopiesData(t *testing.T) {
	src := &AVPacket{
		Codec:    AVCodecAAC,
		Data:     []byte{1, 2, 3},
		PTSms:    100,
		DTSms:    100,
		KeyFrame: true,
	}
	c := src.Clone()
	if c == src {
		t.Fatal("Clone returned same pointer")
	}
	if c.Codec != src.Codec || c.PTSms != 100 || !c.KeyFrame {
		t.Fatalf("fields not copied: %+v", c)
	}

	c.Data[0] = 0xFF
	if src.Data[0] != 1 {
		t.Fatal("Clone shared data backing array")
	}
}

func TestAVPacketCloneEmptyData(t *testing.T) {
	src := &AVPacket{Codec: AVCodecH264}
	c := src.Clone()
	if c.Data != nil {
		t.Fatalf("empty Data should remain nil, got %v", c.Data)
	}
}

func TestStreamCodeFilterNil(t *testing.T) {
	var f *StreamCodeFilter
	if !f.Matches("anything") {
		t.Fatal("nil filter should match everything")
	}
}

func TestStreamCodeFilterOnlyTakesPrecedence(t *testing.T) {
	f := &StreamCodeFilter{
		Only:   []StreamCode{"a", "b"},
		Except: []StreamCode{"a"}, // ignored when Only is set
	}
	if !f.Matches("a") {
		t.Error("Only=[a] should match a despite Except=[a]")
	}
	if !f.Matches("b") {
		t.Error("Only=[a,b] should match b")
	}
	if f.Matches("c") {
		t.Error("Only=[a,b] must not match c")
	}
}

func TestStreamCodeFilterExcept(t *testing.T) {
	f := &StreamCodeFilter{Except: []StreamCode{"x", "y"}}
	if f.Matches("x") {
		t.Error("Except=[x,y] must not match x")
	}
	if !f.Matches("z") {
		t.Error("Except=[x,y] must match z")
	}
}

func TestStreamCodeFilterEmpty(t *testing.T) {
	f := &StreamCodeFilter{}
	if !f.Matches("anything") {
		t.Fatal("empty filter (no Only, no Except) must match everything")
	}
}
