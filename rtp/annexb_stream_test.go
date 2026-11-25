package rtp

import (
	"bytes"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

var h264Fixtures = []string{"testsrc2_160x120_24f_3slices.h264", "testsrc2_160x120_24f_aud.h264"}

const fixtureFrames = 24

func splitStream(stream []byte, rng *rand.Rand) [][]byte {
	var s AccessUnitSplitter
	var aus [][]byte
	for rest := stream; len(rest) > 0; {
		n := len(rest)
		if rng != nil {
			n = min(1+rng.Intn(1500), n)
		}
		aus = append(aus, s.Write(rest[:n])...)
		rest = rest[n:]
	}
	if last := s.Flush(); last != nil {
		aus = append(aus, last)
	}
	return aus
}

// Every coded picture becomes exactly one access unit, no NAL unit is lost or reordered,
// and where the pipe happens to cut the stream makes no difference.
func TestAccessUnitSplitter(t *testing.T) {
	for _, name := range h264Fixtures {
		stream, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		want := splitStream(stream, nil)
		if len(want) != fixtureFrames {
			t.Fatalf("%s: %d access units, want %d", name, len(want), fixtureFrames)
		}
		var nals [][]byte
		for i, au := range want {
			pictures := 0
			for _, n := range SplitAnnexB(au) {
				if t := n[0] & 0x1f; (t == 1 || t == 5) && n[1]&0x80 != 0 {
					pictures++
				}
				nals = append(nals, n)
			}
			if pictures != 1 {
				t.Fatalf("%s: access unit %d holds %d pictures", name, i, pictures)
			}
		}
		orig := SplitAnnexB(stream)
		if len(nals) != len(orig) {
			t.Fatalf("%s: %d NAL units out, %d in", name, len(nals), len(orig))
		}
		for i := range orig {
			if !bytes.Equal(nals[i], orig[i]) {
				t.Fatalf("%s: NAL unit %d changed", name, i)
			}
		}
		for seed := int64(1); seed <= 20; seed++ {
			got := splitStream(stream, rand.New(rand.NewSource(seed)))
			if len(got) != len(want) {
				t.Fatalf("%s seed %d: %d access units, want %d", name, seed, len(got), len(want))
			}
			for i := range want {
				if !bytes.Equal(got[i], want[i]) {
					t.Fatalf("%s seed %d: access unit %d depends on chunking", name, seed, i)
				}
			}
		}
	}
}

// A picture must be handed on as soon as the next one starts, not when the next one is
// complete: that is the difference between one frame of delay and two.
func TestAccessUnitSplitterEmitsAtNextPictureStart(t *testing.T) {
	stream, err := os.ReadFile(filepath.Join("testdata", h264Fixtures[0]))
	if err != nil {
		t.Fatal(err)
	}
	aus := splitStream(stream, nil)
	first := len(aus[0])
	// The fixture uses the same start code lengths as the splitter writes? Find the
	// second picture's first NAL in the original stream instead of assuming so.
	second := SplitAnnexB(aus[1])[0]
	at := bytes.Index(stream, second)
	if at < 0 {
		t.Fatal("second picture not found in the stream")
	}
	var s AccessUnitSplitter
	if got := s.Write(stream[:at]); len(got) != 0 {
		t.Fatal("emitted before the next picture started")
	}
	got := s.Write(stream[at : at+2])
	if len(got) != 1 || len(got[0]) != first {
		t.Fatalf("first picture not emitted at the start of the second (got %d units)", len(got))
	}
}

// Packetise → depacketise must give back the same access units, and (if ffmpeg is
// installed) they must decode to the same pictures as the original stream.
func TestH264RTPRoundTrip(t *testing.T) {
	for _, name := range h264Fixtures {
		path := filepath.Join("testdata", name)
		stream, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var depack H264Depacketizer
		var rebuilt []byte
		packets := 0
		for _, au := range splitStream(stream, rand.New(rand.NewSource(7))) {
			var got []byte
			for _, nalu := range SplitAnnexB(au) {
				for _, payload := range PackageH264NALU(nalu) {
					packets++
					if len(payload) > mtuPayloadMax {
						t.Fatalf("payload of %d bytes exceeds the %d byte budget", len(payload), mtuPayloadMax)
					}
					for _, n := range depack.Depacketize(payload) {
						got = append(got, 0, 0, 0, 1)
						got = append(got, n...)
					}
				}
			}
			if !bytes.Equal(got, au) {
				t.Fatalf("%s: access unit changed in the RTP round trip", name)
			}
			rebuilt = append(rebuilt, got...)
		}
		t.Logf("%s: %d access units in %d RTP payloads", name, fixtureFrames, packets)

		ffmpeg, err := exec.LookPath("ffmpeg")
		if err != nil {
			t.Log("ffmpeg not installed: decode comparison skipped")
			continue
		}
		out := filepath.Join(t.TempDir(), "rebuilt.h264")
		if err := os.WriteFile(out, rebuilt, 0o600); err != nil {
			t.Fatal(err)
		}
		md5 := func(p string) string {
			b, err := exec.Command(ffmpeg, "-v", "error", "-i", p, "-f", "framemd5", "-").Output()
			if err != nil {
				t.Fatalf("ffmpeg decode %s: %v", p, err)
			}
			return string(b)
		}
		a, b := md5(path), md5(out)
		if a != b || bytes.Count([]byte(a), []byte("\n0,")) != fixtureFrames {
			t.Fatalf("%s: decoded pictures differ after the round trip", name)
		}
	}
}
