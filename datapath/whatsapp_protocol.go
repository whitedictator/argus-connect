package datapath

// A conservative recognition of the WA 6.3 transport preface and bounded
// client-hello frame used by our anonymous protocol probe. No payload is logged.
// Absence does not prove the connection isn't WhatsApp (cached/resumed sessions).
func isWhatsAppNoiseStart(first []byte) bool {
	if offset := whatsappEdgeOffset(first); offset > 0 {
		first = first[offset:]
	}
	if len(first) < 11 || first[0] != 'W' || first[1] != 'A' || first[2] != 6 || first[3] != 3 {
		return false
	}
	n := int(first[4])<<16 | int(first[5])<<8 | int(first[6])
	if n < 36 || n > 65535 || first[7] != 0x12 {
		return false
	}
	// ClientHello may contain resumed/PQ fields; its protobuf length is not
	// always the 34-byte ephemeral-only hello used by the anonymous probe.
	size := 0
	for i := 0; i < 3 && 8+i < len(first); i++ {
		b := first[8+i]
		size |= int(b&0x7f) << (7 * i)
		if b&0x80 == 0 {
			if size < 34 || size+2+i != n {
				return false
			}
			if 9+i < len(first) && first[9+i] != 0x0a {
				return false
			}
			if 10+i < len(first) && first[10+i] != 32 {
				return false
			}
			return true
		}
	}
	return false
}

// ED is an optional edge-routing envelope, outside the Noise transcript.
// Never log its opaque contents or accept an unbounded length.
func whatsappEdgeOffset(first []byte) int {
	if len(first) < 7 || first[0] != 'E' || first[1] != 'D' || first[2] != 0 || first[3] != 1 {
		return 0
	}
	n := int(first[4])<<16 | int(first[5])<<8 | int(first[6])
	if n > 256 || len(first) < 7+n+11 {
		return 0
	}
	return 7 + n
}
