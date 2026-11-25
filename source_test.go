package agentcaller

import (
	"encoding/binary"
	"io"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// writeTestWAV writes interleaved s16le PCM as a RIFF/WAVE file.
func writeTestWAV(t *testing.T, rate, channels int, pcm []int16) string {
	t.Helper()
	b := make([]byte, 44+2*len(pcm))
	copy(b, "RIFF")
	binary.LittleEndian.PutUint32(b[4:], uint32(36+2*len(pcm)))
	copy(b[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(b[16:], 16)
	binary.LittleEndian.PutUint16(b[20:], 1)
	binary.LittleEndian.PutUint16(b[22:], uint16(channels))
	binary.LittleEndian.PutUint32(b[24:], uint32(rate))
	binary.LittleEndian.PutUint32(b[28:], uint32(rate*channels*2))
	binary.LittleEndian.PutUint16(b[32:], uint16(channels*2))
	binary.LittleEndian.PutUint16(b[34:], 16)
	copy(b[36:], "data")
	binary.LittleEndian.PutUint32(b[40:], uint32(2*len(pcm)))
	for i, v := range pcm {
		binary.LittleEndian.PutUint16(b[44+2*i:], uint16(v))
	}
	path := filepath.Join(t.TempDir(), "in.wav")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// A 44.1 kHz stereo file must arrive as the same sound at 16 kHz: the speech-band tone
// intact and in place, the tone above 8 kHz gone instead of folded into the speech band.
func TestWAVFileResamplesBandLimited(t *testing.T) {
	const rate, seconds = 44100, 1
	pcm := make([]int16, 2*rate*seconds)
	for i := 0; i < rate*seconds; i++ {
		ts := float64(i) / rate
		v := 0.4*math.Sin(2*math.Pi*1000*ts) + 0.4*math.Sin(2*math.Pi*11000*ts)
		pcm[2*i], pcm[2*i+1] = int16(v*32767), int16(v*32767)
	}
	src, err := WAVFile(writeTestWAV(t, rate, 2, pcm))
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	var got []float32
	for {
		frame, err := src.ReadFrame()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if len(frame) != FrameSamples {
			t.Fatalf("frame of %d samples", len(frame))
		}
		got = append(got, frame...)
	}
	if want := SampleRate * seconds; len(got) < want || len(got) >= want+FrameSamples {
		t.Fatalf("%d samples out, want %d padded to a whole frame", len(got), want)
	}
	var errE, sigE float64
	for i := 500; i < SampleRate*seconds-500; i++ {
		want := 0.4 * math.Sin(2*math.Pi*1000*float64(i)/SampleRate)
		d := float64(got[i]) - want
		errE += d * d
		sigE += want * want
	}
	if snr := 10 * math.Log10(sigE/errE); snr < 55 {
		t.Fatalf("1 kHz tone recovered with %.1f dB SNR, want ≥ 55 (the 11 kHz tone must not alias)", snr)
	}
}
