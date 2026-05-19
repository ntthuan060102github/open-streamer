package pull

// mpts_filter.go — extract a single MPEG-TS program from a multi-program
// transport stream (MPTS).
//
// Why this exists: DVB headends often carry many TV channels in one MPTS
// over a single multicast IP. Forwarding the whole MPTS into HLS / DASH
// segments produces players-confusing nb_programs > 1 output (interleaved
// DTS from unrelated channels, "Packet corrupt" warnings, decode_slice_header
// errors). Production media servers (production media servers) demultiplex by
// program_number so only the chosen channel reaches the publisher; this
// filter does the same at the byte level without decoding any frames.
//
// Filter pipeline:
//
//	TSChunkReader.Read ──► alignedFeed (188-byte split) ──► per-packet
//	                                                          │
//	                                          ┌───────────────┴────────────┐
//	                                          ▼                            ▼
//	                                    PID == 0 (PAT)          PID == pmtPID OR pid in esPIDs
//	                                          │                            │
//	                                  Rewrite PAT to single             Forward verbatim
//	                                  program + recompute CRC32        (preserves CC, AF, PCR)
//	                                          │                            │
//	                                          └────────►  output chunk ◄───┘
//
// Continuity counters of the original PAT packets are preserved (we replace
// only the payload of each PAT packet, keeping its TS header). PMT / ES
// packets are forwarded byte-for-byte. Other PIDs (other programs' PMTs
// and ES, NULL packets, SDT/EIT/NIT) are dropped.
//
// Limitations (acceptable v1):
//   - PAT and PMT must each fit in a single TS packet (section_length ≤ 183).
//     Real-world PAT is always tiny; PMT for one program is also small (well
//     under 100 bytes for a typical video+audio channel). Multi-section PSI
//     is rare and would be dropped — caller would never learn esPIDs and the
//     filter would emit only PAT.
//   - First few datagrams may be empty until a PAT arrives. Caller's reader
//     loop tolerates this — Read() loops internally so an empty post-filter
//     chunk is never surfaced.

import (
	"context"
	"encoding/binary"
)

// tsPacketSize is the fixed MPEG-TS packet length on the wire.
const tsPacketSize = 188

// MPTSProgramFilter wraps a TSChunkReader and emits chunks containing only
// TS packets belonging to one program from the source MPTS.
type MPTSProgramFilter struct {
	inner TSChunkReader

	// program is the program_number selected from PAT.
	program int

	// Learned PSI state. Reset on Open() so reconnection cycles re-learn
	// from a fresh PAT (the source may publish a different PMT layout
	// after stream restart).
	pmtPID uint16          // PMT PID for the wanted program (0 = unknown)
	esPIDs map[uint16]bool // ES PIDs (incl. PCR PID) of the wanted program

	// carry holds bytes left over when the chunk from inner is not 188-byte
	// aligned (rare for UDP / HLS but possible for File reads).
	carry []byte
}

// NewMPTSProgramFilter wraps r so each Read returns a chunk containing only
// the chosen program. program must be ≥ 1; passing 0 or less is a config
// error and reader.go is responsible for not constructing the filter then.
func NewMPTSProgramFilter(r TSChunkReader, program int) *MPTSProgramFilter {
	return &MPTSProgramFilter{
		inner:   r,
		program: program,
		esPIDs:  make(map[uint16]bool),
	}
}

// Open delegates to the wrapped reader and resets PSI state so a
// reconnection picks up a possibly-changed PMT layout.
func (f *MPTSProgramFilter) Open(ctx context.Context) error {
	f.pmtPID = 0
	f.esPIDs = make(map[uint16]bool)
	f.carry = f.carry[:0]
	return f.inner.Open(ctx)
}

// Close delegates to the wrapped reader.
func (f *MPTSProgramFilter) Close() error {
	return f.inner.Close()
}

// Read returns the next non-empty filtered chunk. If filtering produces an
// empty result for the upstream chunk (e.g. early datagrams that contain
// only other programs' packets), it loops to the next upstream chunk so
// the caller never sees a zero-length non-error read.
func (f *MPTSProgramFilter) Read(ctx context.Context) ([]byte, error) {
	for {
		chunk, err := f.inner.Read(ctx)
		if err != nil {
			return nil, err
		}
		out := f.filter(chunk)
		if len(out) > 0 {
			return out, nil
		}
		// All packets in this chunk were filtered out — try next one.
	}
}

// filter walks the chunk packet-by-packet and returns the bytes to forward.
// Carries unaligned trailing bytes for the next call so a sync-byte split
// across chunks is recovered.
func (f *MPTSProgramFilter) filter(chunk []byte) []byte {
	f.carry = append(f.carry, chunk...)

	// Drop leading bytes until first 0x47 sync byte. Invalid input
	// (no sync byte in many KB) → keep last 187 bytes as carry so we don't
	// miss a sync straddling chunk boundary.
	if i := firstSync(f.carry); i > 0 {
		f.carry = f.carry[i:]
	}

	out := make([]byte, 0, len(f.carry))
	for len(f.carry) >= tsPacketSize {
		pkt := f.carry[:tsPacketSize]
		if pkt[0] != 0x47 {
			// Re-align: drop one byte and search forward.
			f.carry = f.carry[1:]
			if i := firstSync(f.carry); i > 0 {
				f.carry = f.carry[i:]
			}
			continue
		}
		if forwarded := f.processPacket(pkt); forwarded != nil {
			out = append(out, forwarded...)
		}
		f.carry = f.carry[tsPacketSize:]
	}
	return out
}

// processPacket returns the bytes to emit for pkt, or nil to drop the packet.
// PAT is rewritten in place (returns a freshly-allocated 188-byte packet);
// kept-PIDs are returned as-is (a sub-slice into f.carry, but the caller
// copies via append so this is safe).
func (f *MPTSProgramFilter) processPacket(pkt []byte) []byte {
	pid := uint16(pkt[1]&0x1F)<<8 | uint16(pkt[2])
	pusi := pkt[1]&0x40 != 0

	switch {
	case pid == 0:
		return f.rewritePAT(pkt, pusi)
	case f.pmtPID != 0 && pid == f.pmtPID:
		f.parsePMT(pkt, pusi)
		return pkt
	case f.esPIDs[pid]:
		return pkt
	default:
		return nil
	}
}

// rewritePAT parses the source PAT, finds the wanted program's PMT PID,
// updates internal state, and returns a fresh 188-byte TS packet whose
// payload is a single-program PAT (with recomputed CRC32). Returns nil if
// the wanted program is not in this PAT, or if the PAT is malformed /
// multi-section (which we don't reassemble).
func (f *MPTSProgramFilter) rewritePAT(pkt []byte, pusi bool) []byte {
	if !pusi {
		// Continuation of a multi-packet PAT section — we don't reassemble.
		// Drop silently; the next PAT cycle will give us a fresh PUSI=1
		// packet to work with.
		return nil
	}
	payload := tsPayloadBytes(pkt)
	if len(payload) < 1 {
		return nil
	}
	ptr := int(payload[0])
	off := 1 + ptr
	if off+12 > len(payload) {
		return nil
	}
	section := payload[off:]
	if section[0] != 0x00 { // PAT table_id
		return nil
	}

	sectionLength := int(section[1]&0x0F)<<8 | int(section[2])
	if 3+sectionLength > len(section) {
		return nil // truncated section
	}
	tsID := uint16(section[3])<<8 | uint16(section[4])
	versionByte := section[5] // reserved(2) + version(5) + current_next(1)
	sectionNumber := section[6]
	lastSectionNumber := section[7]
	if sectionNumber != 0 || lastSectionNumber != 0 {
		// Multi-section PAT — drop. Real-world PAT is always single-section.
		return nil
	}

	// Walk programs to find the requested one.
	var pmtPID uint16
	progEnd := 3 + sectionLength - 4 // exclude trailing CRC32
	if progEnd > len(section) {
		return nil
	}
	for i := 8; i+4 <= progEnd; i += 4 {
		progNum := uint16(section[i])<<8 | uint16(section[i+1])
		pid := uint16(section[i+2]&0x1F)<<8 | uint16(section[i+3])
		if int(progNum) == f.program {
			pmtPID = pid
			break
		}
	}
	if pmtPID == 0 {
		return nil
	}

	// PMT PID changed (first PAT or PMT layout shift) → reset learned ES PIDs
	// so we re-learn from the next PMT cycle.
	if f.pmtPID != pmtPID {
		f.pmtPID = pmtPID
		f.esPIDs = make(map[uint16]bool)
	}

	return buildSingleProgramPATPacket(pkt, tsID, versionByte, f.program, pmtPID)
}

// parsePMT extracts ES PIDs (and the PCR PID) from a PMT packet for our
// program and updates f.esPIDs. Idempotent for unchanged PMTs.
func (f *MPTSProgramFilter) parsePMT(pkt []byte, pusi bool) {
	if !pusi {
		return // continuation — we don't reassemble multi-packet PMTs
	}
	payload := tsPayloadBytes(pkt)
	if len(payload) < 1 {
		return
	}
	ptr := int(payload[0])
	off := 1 + ptr
	if off+12 > len(payload) {
		return
	}
	section := payload[off:]
	if section[0] != 0x02 { // PMT table_id
		return
	}
	sectionLength := int(section[1]&0x0F)<<8 | int(section[2])
	if 3+sectionLength > len(section) {
		return
	}

	pcrPID := uint16(section[8]&0x1F)<<8 | uint16(section[9])
	progInfoLen := int(section[10]&0x0F)<<8 | int(section[11])
	pos := 12 + progInfoLen
	end := 3 + sectionLength - 4
	if end > len(section) || pos > end {
		return
	}

	newPIDs := make(map[uint16]bool)
	// Always include PCR PID — it carries timestamps even if not an ES PID.
	if pcrPID != 0 && pcrPID != 0x1FFF {
		newPIDs[pcrPID] = true
	}
	for pos+5 <= end {
		esPID := uint16(section[pos+1]&0x1F)<<8 | uint16(section[pos+2])
		esInfoLen := int(section[pos+3]&0x0F)<<8 | int(section[pos+4])
		newPIDs[esPID] = true
		pos += 5 + esInfoLen
	}
	f.esPIDs = newPIDs
}

// ─── Helpers ──────────────────────────────────────────────────────────────

// firstSync returns the index of the first 0x47 byte in b, or len(b) if not
// found. Callers slice b at the returned index to drop pre-sync garbage.
func firstSync(b []byte) int {
	for i, c := range b {
		if c == 0x47 {
			return i
		}
	}
	return len(b)
}

// tsPayloadBytes extracts the payload bytes of a TS packet, skipping the
// 4-byte header and any adaptation field. Returns nil if the packet has no
// payload or the adaptation field is malformed.
//
// Local copy (not shared with publisher's tsPayload) so this package has no
// upward dependency on internal/publisher.
func tsPayloadBytes(pkt []byte) []byte {
	if len(pkt) < 4 {
		return nil
	}
	adaptCtrl := (pkt[3] >> 4) & 0x03
	hasPayload := adaptCtrl&0x01 != 0
	if !hasPayload {
		return nil
	}
	off := 4
	if adaptCtrl&0x02 != 0 {
		if off >= len(pkt) {
			return nil
		}
		afLen := int(pkt[off])
		off += 1 + afLen
		if off >= len(pkt) {
			return nil
		}
	}
	return pkt[off:]
}

// buildSingleProgramPATPacket constructs a 188-byte TS packet whose payload
// is a PAT advertising exactly one program (programNumber → pmtPID). The
// returned packet preserves the source PAT packet's TS header (sync, PID,
// scrambling, continuity counter) and forces AFC=01 (payload only, no
// adaptation field) so downstream consumers see uninterrupted CC for PID 0.
func buildSingleProgramPATPacket(srcPkt []byte, tsID uint16, versionByte byte, programNumber int, pmtPID uint16) []byte {
	// section_length counts every byte AFTER the section_length field through
	// CRC32: tsID(2) + version(1) + section_number(1) + last_section_number(1)
	// + 1×program(4) + CRC32(4) = 13. Total layout from table_id to end of
	// CRC32 = 3 + 13 = 16 bytes (the size of `pat` below).
	const sectionLengthValue = 13

	pat := make([]byte, 16)
	pat[0] = 0x00 // table_id = PAT
	// section_syntax_indicator=1, '0', reserved=11 → upper byte 0xB0
	pat[1] = 0xB0 | byte((sectionLengthValue>>8)&0x0F)
	pat[2] = byte(sectionLengthValue & 0xFF)
	binary.BigEndian.PutUint16(pat[3:5], tsID)
	pat[5] = versionByte // reserved(2) + version(5) + current_next(1) preserved
	pat[6] = 0x00        // section_number
	pat[7] = 0x00        // last_section_number
	binary.BigEndian.PutUint16(pat[8:10], uint16(programNumber))
	// reserved(3 bits) + PMT_PID(13 bits): top 3 bits set to 1 per spec.
	pat[10] = 0xE0 | byte((pmtPID>>8)&0x1F)
	pat[11] = byte(pmtPID & 0xFF)
	crc := mpegCRC32(pat[:12])
	binary.BigEndian.PutUint32(pat[12:16], crc)

	// Build the TS packet: copy source TS header (4 bytes), force AFC=01,
	// pointer_field=0, then PAT bytes, then 0xFF stuffing to 188.
	out := make([]byte, tsPacketSize)
	copy(out[:4], srcPkt[:4])
	out[3] = (out[3] & 0xCF) | 0x10 // clear AFC bits, set AFC=01 (payload only)
	out[4] = 0x00                   // pointer_field
	copy(out[5:], pat)
	for i := 5 + len(pat); i < tsPacketSize; i++ {
		out[i] = 0xFF
	}
	return out
}

// crc32MPEGTable is the precomputed lookup table for the MPEG-2 systems
// CRC32 (poly 0x04C11DB7, MSB-first, init 0xFFFFFFFF, no inversion). Used
// for PAT / PMT section CRC. Generated once at package init.
var crc32MPEGTable [256]uint32

func init() {
	for i := uint32(0); i < 256; i++ {
		c := i << 24
		for range 8 {
			if c&0x80000000 != 0 {
				c = (c << 1) ^ 0x04C11DB7
			} else {
				c <<= 1
			}
		}
		crc32MPEGTable[i] = c
	}
}

// mpegCRC32 computes the MPEG-2 systems CRC32 over data. Used for PAT /
// PMT section CRC fields.
func mpegCRC32(data []byte) uint32 {
	crc := uint32(0xFFFFFFFF)
	for _, b := range data {
		crc = (crc << 8) ^ crc32MPEGTable[byte(crc>>24)^b]
	}
	return crc
}

// Compile-time interface check: MPTSProgramFilter must satisfy TSChunkReader
// so the reader factory can wrap it like any other raw-TS source.
var _ TSChunkReader = (*MPTSProgramFilter)(nil)
