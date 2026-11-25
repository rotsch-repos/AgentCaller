package resample

import (
	"math"
	"math/rand"
	"testing"
)

func sine(rate int, freq float64, n int) []float32 {
	out := make([]float32, n)
	for i := range out {
		out[i] = float32(0.5 * math.Sin(2*math.Pi*freq*float64(i)/float64(rate)))
	}
	return out
}

func rmsDB(x []float32) float64 {
	var e float64
	for _, v := range x {
		e += float64(v) * float64(v)
	}
	return 10 * math.Log10(e/float64(len(x))+1e-30)
}

func convert(in []float32, inRate, outRate int) []float32 {
	r := New(inRate, outRate)
	return r.Flush(r.Process(in, nil))
}

// linear is the interpolation this package replaced, kept as the yardstick.
func linear(in []float32, inRate, outRate int) []float32 {
	step := float64(inRate) / float64(outRate)
	var out []float32
	for pos := 0.0; int(pos)+1 < len(in); pos += step {
		i := int(pos)
		f := float32(pos - float64(i))
		out = append(out, in[i]*(1-f)+in[i+1]*f)
	}
	return out
}

// Tones above the output Nyquist frequency must vanish instead of folding into the
// speech band.
func TestAliasRejection(t *testing.T) {
	for _, rate := range []int{48000, 44100, 24000, 22050} {
		ref := rmsDB(sine(rate, 1000, rate))
		for _, freq := range []float64{8200, 9000, 10500} {
			if freq >= float64(rate)/2 {
				continue
			}
			in := sine(rate, freq, rate)
			got := convert(in, rate, 16000)
			mid := got[2000 : len(got)-2000]
			rej := ref - rmsDB(mid)
			old := ref - rmsDB(linear(in, rate, 16000))
			t.Logf("%5d Hz in, %5.0f Hz tone: rejected by %5.1f dB (linear interpolation: %4.1f dB)", rate, freq, rej, old)
			if freq >= 9000 && rej < 75 {
				t.Errorf("%d Hz, tone %.0f Hz: only %.1f dB rejection", rate, freq, rej)
			}
			if rej < 40 {
				t.Errorf("%d Hz, tone %.0f Hz just above Nyquist: only %.1f dB rejection", rate, freq, rej)
			}
		}
	}
}

// Tones in the speech band must come through at the right level and in the right place
// in time: compared sample by sample with the same tone generated at the output rate.
func TestPassbandIsTransparent(t *testing.T) {
	for _, rate := range []int{48000, 44100, 24000, 22050, 8000} {
		for _, freq := range []float64{200, 1000, 3400, 6500} {
			if freq >= 0.85*float64(min(rate, 16000))/2 {
				continue
			}
			got := convert(sine(rate, freq, rate), rate, 16000)
			want := sine(16000, freq, len(got))
			var errE, sigE float64
			for i := 1000; i < len(got)-1000; i++ {
				d := float64(got[i] - want[i])
				errE += d * d
				sigE += float64(want[i]) * float64(want[i])
			}
			snr := 10 * math.Log10(sigE/(errE+1e-30))
			if snr < 60 {
				t.Errorf("%d → 16000 Hz, %4.0f Hz tone: %.1f dB SNR against the ideal tone, want ≥ 60", rate, freq, snr)
			}
		}
	}
}

func TestLengthAndIdentity(t *testing.T) {
	for _, rate := range []int{48000, 44100, 24000, 22050, 8000, 11025, 44101} {
		n := rate/2 + 37
		got := convert(make([]float32, n), rate, 16000)
		want := (n*16000 + rate - 1) / rate
		if len(got) != want {
			t.Errorf("%d Hz: %d samples in gave %d out, want %d", rate, n, len(got), want)
		}
	}
	in := sine(16000, 440, 500)
	got := convert(in, 16000, 16000)
	for i := range in {
		if got[i] != in[i] {
			t.Fatal("equal rates must pass the signal through untouched")
		}
	}
}

// Chunk boundaries must not be audible: any chunking gives the same samples.
func TestStreamingMatchesOneShot(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for _, rate := range []int{48000, 44100, 22050, 8000, 44101} {
		in := make([]float32, rate/4)
		for i := range in {
			in[i] = rng.Float32()*2 - 1
		}
		want := convert(in, rate, 16000)
		r := New(rate, 16000)
		var got []float32
		for rest := in; len(rest) > 0; {
			n := min(1+rng.Intn(700), len(rest))
			got = r.Process(rest[:n], got)
			rest = rest[n:]
		}
		got = r.Flush(got)
		if len(got) != len(want) {
			t.Fatalf("%d Hz: chunked %d samples, one-shot %d", rate, len(got), len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("%d Hz: sample %d differs between chunked and one-shot", rate, i)
			}
		}
	}
}

func BenchmarkResample48kTo16k(b *testing.B) {
	in := sine(48000, 1000, 48000)
	r := New(48000, 16000)
	out := make([]float32, 0, 20000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out = r.Process(in, out[:0])
	}
}
