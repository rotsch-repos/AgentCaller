package plc

import (
	"math"
	"math/rand"
	"testing"
)

const frame = 960

// voiced synthesises a vowel-like signal: harmonics of f0 with a slow level change.
func voiced(n int, f0 float64) []float32 {
	out := make([]float32, n)
	for i := range out {
		t := float64(i) / SampleRate
		var s float64
		for h := 1; h <= 8; h++ {
			s += math.Sin(2*math.Pi*f0*float64(h)*t+float64(h)) / float64(h)
		}
		out[i] = float32(0.25 * s * (1 + 0.2*math.Sin(2*math.Pi*1.5*t)))
	}
	return out
}

func energy(x []float32) float64 {
	var e float64
	for _, v := range x {
		e += float64(v) * float64(v)
	}
	return e
}

func diffEnergy(a, b []float32) float64 {
	var e float64
	for i := range a {
		d := float64(a[i] - b[i])
		e += d * d
	}
	return e
}

func maxStep(x []float32) float64 {
	var m float64
	for i := 1; i < len(x); i++ {
		m = math.Max(m, math.Abs(float64(x[i]-x[i-1])))
	}
	return m
}

func TestVoicedLossIsContinued(t *testing.T) {
	for _, f0 := range []float64{90, 120, 180, 240} {
		sig := voiced(4*frame, f0)
		var c Concealer
		c.Good(append([]float32(nil), sig[:frame]...))
		c.Good(append([]float32(nil), sig[frame:2*frame]...))
		got := c.Conceal(frame)
		want := sig[2*frame : 3*frame]

		// First 20 ms: the continuation has to be far closer to the truth than a hole.
		head := SampleRate / 50
		snr := 10 * math.Log10(energy(want[:head])/diffEnergy(got[:head], want[:head]))
		if snr < 12 {
			t.Errorf("f0=%v: first 20 ms only %.1f dB better than silence", f0, snr)
		}
		// No click where the concealment starts.
		joined := append(append([]float32(nil), sig[2*frame-64:2*frame]...), got[:64]...)
		if s, ref := maxStep(joined), maxStep(sig[:2*frame]); s > 1.5*ref {
			t.Errorf("f0=%v: click at loss start: step %.3f, signal max step %.3f", f0, s, ref)
		}
		// No click where good audio resumes.
		next := c.Good(append([]float32(nil), sig[3*frame:]...))
		joined = append(append([]float32(nil), got[frame-64:]...), next[:joinLen+64]...)
		if s, ref := maxStep(joined), maxStep(sig); s > 1.5*ref {
			t.Errorf("f0=%v: click at resume: step %.3f, signal max step %.3f", f0, s, ref)
		}
	}
}

func TestLongLossFadesToExactSilence(t *testing.T) {
	sig := voiced(2*frame, 130)
	var c Concealer
	c.Good(sig)
	a := append(c.Conceal(frame), c.Conceal(frame)...) // two lost packets in a row
	if energy(a[:holdLen]) == 0 {
		t.Fatal("concealment starts silent")
	}
	if e := energy(a[holdLen+fadeLen:]); e != 0 {
		t.Fatalf("still audible %d ms into the loss (energy %g)", (holdLen+fadeLen)*1000/SampleRate, e)
	}
	if e := energy(c.Conceal(frame)); e != 0 {
		t.Fatalf("third lost packet is not silent (energy %g)", e)
	}
	// Level never rises during the fade (per 5 ms block).
	prev := math.Inf(1)
	for i := holdLen; i+320 <= holdLen+fadeLen; i += 320 {
		e := energy(a[i : i+320])
		if e > prev*1.05 {
			t.Fatalf("level rises during the fade at sample %d", i)
		}
		prev = e
	}
}

func TestNoiseIsNotTurnedIntoATone(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	sig := make([]float32, 2*frame)
	for i := range sig {
		sig[i] = float32(rng.NormFloat64() * 0.05)
	}
	var c Concealer
	c.Good(sig)
	got := c.Conceal(frame)
	if c.voiced {
		t.Fatal("white noise classified as voiced")
	}
	// Never louder than what was heard before.
	if e, ref := energy(got[:320]), energy(sig[len(sig)-320:]); e > 1.5*ref {
		t.Fatalf("concealed noise louder than the signal: %g vs %g", e, ref)
	}
}

func TestSilenceAndColdStart(t *testing.T) {
	var c Concealer
	if e := energy(c.Conceal(frame)); e != 0 {
		t.Fatalf("cold start produced sound (energy %g)", e)
	}
	c.Good(make([]float32, 2*frame))
	if e := energy(c.Conceal(frame)); e != 0 {
		t.Fatalf("silence continued as sound (energy %g)", e)
	}
	// A good frame after a cold-start loss passes through untouched in level.
	sig := voiced(frame, 150)
	want := energy(sig[joinLen:])
	got := c.Good(sig)
	if math.Abs(energy(got[joinLen:])-want) > 1e-9 {
		t.Fatal("good frame altered beyond the join window")
	}
}
