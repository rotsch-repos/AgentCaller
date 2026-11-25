package rtp

// AccessUnitSplitter cuts a continuous H.264 Annex-B byte stream, as an encoder writes
// it to a pipe (for example `ffmpeg -f h264 -`), into access units: one coded picture
// with its parameter sets, ready for Call.SendVideo.
//
// A byte stream carries no frame lengths, so a picture is only known to be complete when
// the next one begins. The splitter decides that from the first two bytes of the next
// NAL unit, not from its end, so an access unit is emitted as soon as the encoder starts
// writing the following picture. A new access unit begins at an access unit delimiter,
// a parameter set or SEI that follows picture data, or a slice whose first_mb_in_slice
// is zero (ITU-T H.264 section 7.4.1.2.3, the cases that occur with one picture per
// frame; redundant pictures and arbitrary slice order are not handled).
type AccessUnitSplitter struct {
	buf     []byte // unconsumed stream; once synced it starts at a NAL header
	synced  bool
	checked bool // the NAL at buf[0] was already tested for "starts a new access unit"
	scan    int  // buf[:scan] holds no start code
	au      []byte
	hasVCL  bool
}

// Write consumes the next bytes of the stream and returns the access units completed by
// them (usually none or one). The returned slices are owned by the caller.
func (s *AccessUnitSplitter) Write(p []byte) [][]byte {
	s.buf = append(s.buf, p...)
	var out [][]byte
	for {
		if !s.synced {
			i := findStartCode(s.buf, 0)
			if i < 0 {
				if len(s.buf) > 2 { // keep a possible start code prefix
					s.buf = append(s.buf[:0], s.buf[len(s.buf)-2:]...)
				}
				return out
			}
			s.buf = append(s.buf[:0], s.buf[i+3:]...)
			s.synced, s.checked, s.scan = true, false, 0
		}
		if !s.checked && len(s.buf) >= 2 {
			if s.hasVCL && startsAccessUnit(s.buf[0], s.buf[1]) {
				out = append(out, s.take())
			}
			s.checked = true
		}
		next := findStartCode(s.buf, s.scan)
		if next < 0 {
			s.scan = max(len(s.buf)-2, 0)
			return out
		}
		s.addNAL(s.buf[:next])
		s.buf = append(s.buf[:0], s.buf[next+3:]...)
		s.checked, s.scan = false, 0
	}
}

// Flush returns the access unit still being assembled, treating the buffered bytes as
// its last NAL unit. Call it at the end of the stream.
func (s *AccessUnitSplitter) Flush() []byte {
	if s.synced {
		if !s.checked && len(s.buf) >= 2 && s.hasVCL && startsAccessUnit(s.buf[0], s.buf[1]) {
			// Cannot return two units; the trailing one is a lone header, drop it.
			s.buf = s.buf[:0]
		}
		s.addNAL(s.buf)
	}
	s.buf, s.synced, s.checked, s.scan = s.buf[:0], false, false, 0
	if len(s.au) == 0 {
		return nil
	}
	return s.take()
}

func (s *AccessUnitSplitter) addNAL(nal []byte) {
	for len(nal) > 0 && nal[len(nal)-1] == 0 { // trailing_zero_8bits / 4-byte start code
		nal = nal[:len(nal)-1]
	}
	if len(nal) == 0 {
		return
	}
	s.au = append(s.au, 0, 0, 0, 1)
	s.au = append(s.au, nal...)
	if t := nal[0] & 0x1f; t >= 1 && t <= 5 {
		s.hasVCL = true
	}
}

func (s *AccessUnitSplitter) take() []byte {
	au := s.au
	s.au, s.hasVCL = nil, false
	return au
}

// startsAccessUnit reports whether a NAL unit with these first two bytes cannot belong
// to an access unit that already contains picture data.
func startsAccessUnit(header, first byte) bool {
	switch header & 0x1f {
	case 6, 7, 8, 9, 14, 15, 16, 17, 18: // SEI, SPS, PPS, AUD, prefix/subset/reserved
		return true
	case 1, 2, 5: // slice: first_mb_in_slice is ue(v), and the code for 0 is a single 1 bit
		return first&0x80 != 0
	}
	return false
}

// findStartCode returns the index of the next 00 00 01 at or after from, or -1.
func findStartCode(b []byte, from int) int {
	for i := from; i+2 < len(b); i++ {
		if b[i+2] > 1 {
			i += 2
			continue
		}
		if b[i] == 0 && b[i+1] == 0 && b[i+2] == 1 {
			return i
		}
	}
	return -1
}
