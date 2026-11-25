package mlow

import (
	"math"
	"math/rand"
	"testing"
)

// fftRecReference is the original recursive transform, kept verbatim as the oracle for
// the planned implementation: same recursion, trig evaluated inline, fresh buffers.
func fftRecReference(x []cpx, stride, n int, sign float32, out []cpx) {
	if n == 1 {
		out[0] = x[0]
		return
	}
	p := smallestFactor(n)
	if p == n {
		for k := 0; k < n; k++ {
			var acc cpx
			angK := sign * 2.0 * smplPI * float32(k) / float32(n)
			for j := 0; j < n; j++ {
				ang := angK * float32(j)
				w := cpx{re: float32(math.Cos(float64(ang))), im: float32(math.Sin(float64(ang)))}
				acc = acc.add(x[j*stride].mul(w))
			}
			out[k] = acc
		}
		return
	}
	m := n / p
	sub := make([]cpx, n)
	for q := 0; q < p; q++ {
		fftRecReference(x[q*stride:], stride*p, m, sign, sub[q*m:(q+1)*m])
	}
	for k := 0; k < n; k++ {
		kmod := k % m
		var acc cpx
		for q := 0; q < p; q++ {
			ang := sign * 2.0 * smplPI * float32(k) * float32(q) / float32(n)
			tw := cpx{re: float32(math.Cos(float64(ang))), im: float32(math.Sin(float64(ang)))}
			acc = acc.add(sub[q*m+kmod].mul(tw))
		}
		out[k] = acc
	}
}

// TestPlannedFFTBitExact pins the planned FFT to the reference recursion bit for bit, in
// both directions, for the codec's own sizes and for mixed-radix and prime lengths. The
// encoder's decisions hang on these spectra, so "close" is not good enough.
func TestPlannedFFTBitExact(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for _, n := range []int{1, 2, 3, 5, 7, 12, 30, 45, 64, 120, 240, 320, SmplLPCNFFT, percwNfft, 960} {
		in := make([]cpx, n)
		for i := range in {
			in[i] = cpx{re: rng.Float32()*2 - 1, im: rng.Float32()*2 - 1}
		}
		for _, sign := range []float32{-1, 1} {
			want := make([]cpx, n)
			fftRecReference(in, 1, n, sign, want)
			for round := 0; round < 2; round++ { // second round reuses pooled scratch
				got := make([]cpx, n)
				cfft(in, got, sign)
				for i := range got {
					if math.Float32bits(got[i].re) != math.Float32bits(want[i].re) ||
						math.Float32bits(got[i].im) != math.Float32bits(want[i].im) {
						t.Fatalf("n=%d sign=%v round=%d bin %d: got %v, want %v", n, sign, round, i, got[i], want[i])
					}
				}
			}
		}
	}
}
