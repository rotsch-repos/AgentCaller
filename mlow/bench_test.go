package mlow

import (
	"encoding/binary"
	"os"
	"testing"
)

// benchSpeech loads the 16 kHz mono speech fixture used across the CLI tests and
// returns it as whole 60 ms frames. Speech (not a tone) is what exercises the
// pitch search and the pulse quantiser, the two hot spots of the encoder.
func benchSpeech(tb testing.TB) [][]float32 {
	tb.Helper()
	raw, err := os.ReadFile("../cmd/agentcaller/testdata/call-opus16k.wav")
	if err != nil {
		tb.Skipf("speech fixture missing: %v", err)
	}
	pcm := raw[44:]
	n := len(pcm) / 2 / opusFrameSamps
	frames := make([][]float32, n)
	for f := range frames {
		frames[f] = make([]float32, opusFrameSamps)
		for i := range frames[f] {
			frames[f][i] = float32(int16(binary.LittleEndian.Uint16(pcm[2*(f*opusFrameSamps+i):]))) / 32768
		}
	}
	return frames
}

// BenchmarkEncode reports the cost of one 60 ms frame. ns/op divided by 60e6 is
// the share of real time the encoder needs on one core.
func BenchmarkEncode(b *testing.B) {
	frames := benchSpeech(b)
	enc := NewMlowEncoder()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := enc.Encode(frames[i%len(frames)]); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkDecode reports the cost of decoding one 60 ms packet.
func BenchmarkDecode(b *testing.B) {
	frames := benchSpeech(b)
	enc := NewMlowEncoder()
	packets := make([][]byte, len(frames))
	for i, f := range frames {
		p, err := enc.Encode(f)
		if err != nil {
			b.Fatal(err)
		}
		packets[i] = p
	}
	dec := NewMlowDecoder()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dec.Decode(packets[i%len(packets)])
	}
}
