package tsmux

import "bytes"

// DrainTS188Aligned appends chunk to carry, then calls emit once per aligned 188-byte
// MPEG-TS packet (sync 0x47). Used so FFmpeg stdin receives transport-aligned writes.
func DrainTS188Aligned(carry *[]byte, chunk []byte, emit func([]byte)) {
	if emit == nil {
		return
	}
	*carry = append(*carry, chunk...)
	for len(*carry) >= 188 {
		if (*carry)[0] != 0x47 {
			idx := bytes.IndexByte(*carry, 0x47)
			if idx < 0 {
				if len(*carry) > 187 {
					*carry = (*carry)[len(*carry)-187:]
				}
				return
			}
			*carry = (*carry)[idx:]
			if len(*carry) < 188 {
				return
			}
		}
		if len(*carry) >= 376 && (*carry)[188] != 0x47 {
			*carry = (*carry)[1:]
			continue
		}
		emit((*carry)[:188])
		*carry = (*carry)[188:]
	}
}
