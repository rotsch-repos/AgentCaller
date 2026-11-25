// Package resample converts mono PCM between sample rates with a band-limited
// (windowed-sinc, polyphase) filter.
//
// The call codec runs at 16 kHz; text-to-speech engines and files arrive at 22.05, 24,
// 44.1 or 48 kHz. Converting by linear interpolation, as this code base used to, has no
// anti-alias filter: everything above 8 kHz in the input folds back into the speech
// band. This resampler removes it first (stop band from the output Nyquist frequency,
// about 80 dB down) and keeps the pass band flat to roughly 0.85 of that frequency.
//
// It is streaming: Process may be fed chunks of any size and the concatenated output
// is identical to converting the whole signal at once. The output is time-aligned with
// the input (no leading delay); the price is a look-ahead of Lookahead input samples,
// about 2 ms at 48 kHz, which Flush drains at the end of a stream.
package resample

import "math"

const (
	// zeroCrossings is the filter half-length in zero crossings of the sinc.
	zeroCrossings = 32
	// kaiserBeta gives about 80 dB of stop-band attenuation.
	kaiserBeta = 7.857
	// rolloff places the cut-off so that the transition band ends at the Nyquist
	// frequency of the slower side (derived from the Kaiser transition width for the
	// two constants above).
	rolloff = 0.927
	// maxPhases bounds the filter table for rate pairs with a tiny common divisor.
	// Beyond it the fractional position is rounded to the nearest phase.
	maxPhases = 2048
)

// Resampler converts one mono stream. It is not safe for concurrent use.
type Resampler struct {
	in, out int
	l, m    int64 // out/in reduced to lowest terms
	phases  int
	taps    int       // taps per phase
	table   []float32 // phases × taps
	buf     []float32 // input not yet fully consumed, starting at input index base
	base    int64     // index of buf[0] in the input stream (may be negative: priming zeros)
	k       int64     // index of the next output sample
}

// New returns a resampler from inRate to outRate (both in Hz, both positive).
func New(inRate, outRate int) *Resampler {
	if inRate <= 0 || outRate <= 0 {
		panic("resample: rates must be positive")
	}
	g := gcd(int64(inRate), int64(outRate))
	r := &Resampler{in: inRate, out: outRate, l: int64(outRate) / g, m: int64(inRate) / g}
	if inRate == outRate {
		return r
	}
	cutoff := rolloff * math.Min(1, float64(outRate)/float64(inRate)) // 1 = input Nyquist
	half := int(math.Ceil(zeroCrossings / cutoff))
	r.taps = 2 * half
	r.phases = int(min(r.l, maxPhases))
	r.table = make([]float32, r.phases*r.taps)
	i0 := bessel0(kaiserBeta)
	for p := 0; p < r.phases; p++ {
		frac := float64(p) / float64(r.phases)
		row := r.table[p*r.taps : (p+1)*r.taps]
		var sum float64
		coef := make([]float64, r.taps)
		for n := range coef {
			u := frac - float64(n-(half-1)) // distance from the output instant, in input samples
			x := u / float64(half)
			if x <= -1 || x >= 1 {
				continue
			}
			w := bessel0(kaiserBeta*math.Sqrt(1-x*x)) / i0
			coef[n] = cutoff * sinc(cutoff*u) * w
			sum += coef[n]
		}
		for n := range coef {
			row[n] = float32(coef[n] / sum) // unity gain at DC for every phase
		}
	}
	// Prime with zeros so that output sample 0 sits on input sample 0.
	r.buf = make([]float32, half-1, 4096)
	r.base = -int64(half - 1)
	return r
}

// Lookahead is the number of future input samples one output sample depends on.
func (r *Resampler) Lookahead() int { return r.taps / 2 }

// Process consumes in and appends the output samples that are now computable to out.
func (r *Resampler) Process(in []float32, out []float32) []float32 {
	if r.table == nil {
		return append(out, in...)
	}
	r.buf = append(r.buf, in...)
	half := int64(r.taps / 2)
	for {
		num := r.k * r.m // position of output k in units of 1/l input samples
		ipos := num / r.l
		first := ipos - (half - 1) - r.base // index in buf of the first tap
		if first+int64(r.taps) > int64(len(r.buf)) {
			break
		}
		phase := num % r.l
		if r.l > maxPhases {
			phase = (phase*maxPhases + r.l/2) / r.l
			if phase == maxPhases { // rounds up to the next input sample
				phase = 0
				first++
				if first+int64(r.taps) > int64(len(r.buf)) {
					break
				}
			}
		}
		row := r.table[int(phase)*r.taps : (int(phase)+1)*r.taps]
		win := r.buf[first : first+int64(r.taps)]
		var acc float32
		for n, c := range row {
			acc += c * win[n]
		}
		out = append(out, acc)
		r.k++
	}
	// Drop input that no later output can reach.
	keepFrom := (r.k*r.m)/r.l - (half - 1) - r.base
	if keepFrom > 0 {
		keepFrom = min(keepFrom, int64(len(r.buf)))
		n := copy(r.buf, r.buf[keepFrom:])
		r.buf = r.buf[:n]
		r.base += keepFrom
	}
	return out
}

// Flush appends the output that was still waiting for look-ahead, as if the input
// continued with silence. Call it once, at the end of the stream; the total output is
// then ceil(inputSamples × outRate / inRate) samples.
func (r *Resampler) Flush(out []float32) []float32 {
	if r.table == nil {
		return out
	}
	received := r.base + int64(len(r.buf))
	owed := (received*r.l+r.m-1)/r.m - r.k
	start := len(out)
	out = r.Process(make([]float32, r.taps), out)
	if int64(len(out)-start) > owed {
		out = out[:int64(start)+max(owed, 0)]
	}
	return out
}

func sinc(x float64) float64 {
	if math.Abs(x) < 1e-12 {
		return 1
	}
	return math.Sin(math.Pi*x) / (math.Pi * x)
}

// bessel0 is the zeroth-order modified Bessel function of the first kind.
func bessel0(x float64) float64 {
	sum, term := 1.0, 1.0
	for k := 1.0; k < 64; k++ {
		term *= (x / (2 * k)) * (x / (2 * k))
		sum += term
		if term < 1e-18*sum {
			break
		}
	}
	return sum
}

func gcd(a, b int64) int64 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}
