package publisher

const tsPacketSize = 188

// tsPacketBuf extracts 188-byte MPEG-TS packets from arbitrary chunk reads.
type tsPacketBuf struct {
	carry []byte
}

// Feed appends data and returns complete TS packets (each 188 bytes, sync 0x47).
func (b *tsPacketBuf) Feed(data []byte) [][]byte {
	if len(data) == 0 {
		return nil
	}
	buf := append(b.carry, data...)
	b.carry = b.carry[:0]

	var out [][]byte
	for len(buf) > 0 {
		i := indexSync(buf)
		if i < 0 {
			if len(buf) > tsPacketSize {
				buf = buf[len(buf)-tsPacketSize:]
			}
			b.carry = append(b.carry, buf...)
			return out
		}
		buf = buf[i:]
		if len(buf) < tsPacketSize {
			b.carry = append(b.carry, buf...)
			return out
		}
		pkt := make([]byte, tsPacketSize)
		copy(pkt, buf[:tsPacketSize])
		out = append(out, pkt)
		buf = buf[tsPacketSize:]
	}
	return out
}

func indexSync(b []byte) int {
	for i := 0; i+tsPacketSize <= len(b); i++ {
		if b[i] == 0x47 {
			return i
		}
	}
	return -1
}
