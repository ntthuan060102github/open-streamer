package pull

// pid_filter.go — pass through only an operator-specified set of TS PIDs.
//
// Use cases (mirrors Flussonic's `pids` input option):
//   - Source PSI is malformed or missing → MPTSProgramFilter can't auto-
//     detect ES PIDs; operator hardcodes the PIDs they know.
//   - Cherry-pick: the source is multi-language (1 video + N audios) but
//     downstream consumers only want one specific audio track.
//   - Drop unwanted PIDs: teletext / SCTE-35 / data PIDs that confuse some
//     players or waste segment bandwidth.
//
// Difference from MPTSProgramFilter: this filter does NOT touch PAT/PMT —
// it just forwards packets whose PID is in the allowlist and drops the rest.
// Operators are expected to include PSI PIDs (PID 0 for PAT and the PMT PID)
// in the list themselves; otherwise downstream players have nothing to
// decode the elementary streams against. Logging that case is deliberately
// out of scope: misconfiguration is the operator's responsibility, and
// silent forwarding matches Flussonic's behaviour.
//
// Composition: the reader factory chains MPTSProgramFilter → PIDFilter so
// `program` runs first (rewrites PAT, narrows to one program) and `pids`
// further restricts the output. Each filter is independent and either can
// be skipped when its config knob is unset.

import (
	"context"
)

// PIDFilter forwards only TS packets whose PID is in the configured set.
type PIDFilter struct {
	inner TSChunkReader
	keep  map[uint16]bool

	// carry preserves leftover bytes when the inner chunk is not 188-aligned.
	carry []byte
}

// NewPIDFilter wraps r so each Read returns a chunk containing only packets
// whose PID is in pids. PIDs outside the valid TS range (0..8191) are
// silently ignored. An empty pids list is treated as "drop everything",
// which the caller's reader.go avoids by not constructing the filter then.
func NewPIDFilter(r TSChunkReader, pids []int) *PIDFilter {
	keep := make(map[uint16]bool, len(pids))
	for _, p := range pids {
		if p >= 0 && p <= 0x1FFF {
			keep[uint16(p)] = true
		}
	}
	return &PIDFilter{inner: r, keep: keep}
}

// Open delegates and resets the alignment carry.
func (f *PIDFilter) Open(ctx context.Context) error {
	f.carry = f.carry[:0]
	return f.inner.Open(ctx)
}

// Close delegates to the wrapped reader.
func (f *PIDFilter) Close() error {
	return f.inner.Close()
}

// Read returns the next non-empty filtered chunk. Loops over inner reads
// when filtering produces zero bytes (e.g. a UDP datagram of all-dropped
// PIDs) so the caller never sees a zero-length non-error read.
func (f *PIDFilter) Read(ctx context.Context) ([]byte, error) {
	for {
		chunk, err := f.inner.Read(ctx)
		if err != nil {
			return nil, err
		}
		out := f.filter(chunk)
		if len(out) > 0 {
			return out, nil
		}
	}
}

// filter walks the chunk packet-by-packet (188 bytes each) and keeps only
// allowlisted PIDs. Carries unaligned trailing bytes for the next call.
func (f *PIDFilter) filter(chunk []byte) []byte {
	f.carry = append(f.carry, chunk...)
	if i := firstSync(f.carry); i > 0 {
		f.carry = f.carry[i:]
	}

	out := make([]byte, 0, len(f.carry))
	for len(f.carry) >= tsPacketSize {
		pkt := f.carry[:tsPacketSize]
		if pkt[0] != 0x47 {
			f.carry = f.carry[1:]
			if i := firstSync(f.carry); i > 0 {
				f.carry = f.carry[i:]
			}
			continue
		}
		pid := uint16(pkt[1]&0x1F)<<8 | uint16(pkt[2])
		if f.keep[pid] {
			out = append(out, pkt...)
		}
		f.carry = f.carry[tsPacketSize:]
	}
	return out
}

// Compile-time interface check.
var _ TSChunkReader = (*PIDFilter)(nil)
