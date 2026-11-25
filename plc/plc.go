// Package plc conceals lost audio packets on the receive side, for human listeners.
//
// The MLow decoder has no concealment of its own: a lost packet is a hole, and with
// 60 ms packets every hole swallows a syllable and clicks at both edges. Concealer works
// on decoded PCM, so it is independent of the codec: it continues the last pitch cycle
// of the audio it has already seen (waveform substitution, the approach of ITU-T G.711
// Appendix I), fades that continuation out, and cross-fades back into the first packet
// that arrives after the gap. It adds no delay.
//
// What it is not: a way to recover the lost speech. Measured on MLow-decoded speech, the
// continuation does not track the real signal (about -1.4 dB against a silent gap over
// the first 20 ms, where the best possible repetition lag reaches +0.9 dB), and a speech
// recogniser does worse with it than with plain silence (see the note on audioReceiver
// in the root package). It removes the clicks and the dead air a person hears; that is
// all, and the receive path therefore leaves it off unless asked.
package plc

import "math"

const (
	// SampleRate is the only rate the concealer is tuned for (the call audio rate).
	SampleRate = 16000

	minPeriod = SampleRate / 400 // 400 Hz, highest pitch searched
	maxPeriod = SampleRate / 66  // 66 Hz, lowest pitch searched
	corrLen   = SampleRate / 50  // 20 ms correlation window
	histLen   = maxPeriod + corrLen + maxPeriod

	holdLen = SampleRate / 50      // 20 ms at full level before the fade starts
	fadeLen = SampleRate * 8 / 100 // 80 ms linear fade to silence
	joinLen = SampleRate / 100     // 10 ms cross-fade into the next good packet

	// voicedThreshold is the normalised correlation above which the tail of the history
	// is treated as periodic.
	voicedThreshold = 0.5
)

// Concealer keeps the recent output and synthesises replacements for lost packets. The
// zero value is ready to use. It is not safe for concurrent use.
type Concealer struct {
	hist   [histLen]float32 // most recent output, oldest first
	filled int              // valid samples at the end of hist

	concealed int  // samples synthesised since the last good packet
	period    int  // pitch period used for the current loss
	voiced    bool // whether the current loss continues a periodic signal
	phase     int  // read position inside the repeated cycle
	cycle     [maxPeriod]float32
}

// Good records a correctly decoded frame. After a loss it cross-fades the start of the
// frame with the running continuation so the splice does not click. The frame is
// modified in place and returned.
func (c *Concealer) Good(frame []float32) []float32 {
	if c.concealed > 0 {
		n := min(joinLen, len(frame))
		for i := 0; i < n; i++ {
			w := float32(i+1) / float32(n+1)
			frame[i] = w*frame[i] + (1-w)*c.next()
		}
		c.concealed = 0
	}
	c.remember(frame)
	return frame
}

// Conceal returns n samples standing in for lost audio. Consecutive calls continue the
// same fade; once it has run out the output is exact silence.
func (c *Concealer) Conceal(n int) []float32 {
	out := make([]float32, n)
	if c.filled < histLen {
		// Not enough history to find a pitch cycle: silence is the honest answer.
		c.concealed += n
		return out
	}
	if c.concealed == 0 {
		c.start()
	}
	for i := range out {
		out[i] = c.next()
	}
	return out
}

// Reset forgets the history (call at a stream discontinuity).
func (c *Concealer) Reset() { *c = Concealer{} }

// start analyses the history tail and prepares the cycle to repeat. A periodic tail is
// repeated as is. A noise-like tail is played backwards and forwards alternately (see
// next): repeating a short stretch of hiss would turn it into a buzz.
func (c *Concealer) start() {
	c.period, c.voiced = c.findPeriod()
	copy(c.cycle[:c.period], c.hist[histLen-c.period:])
	c.phase = 0
}

// next produces one sample of the continuation, including the fade.
func (c *Concealer) next() float32 {
	g := c.gain()
	c.concealed++
	if g == 0 || c.period == 0 {
		return 0
	}
	var s float32
	if c.voiced {
		s = c.cycle[c.phase%c.period]
	} else {
		// Ping-pong through the cycle: 0..P-1, P-1..0, 0..P-1, …
		k := c.phase % (2 * c.period)
		if k >= c.period {
			k = 2*c.period - 1 - k
		}
		s = c.cycle[c.period-1-k]
	}
	c.phase++
	return s * g
}

// gain is the level of the continuation after c.concealed samples.
func (c *Concealer) gain() float32 {
	hold := holdLen
	if !c.voiced {
		hold = holdLen / 2
	}
	switch {
	case c.concealed < hold:
		return 1
	case c.concealed >= hold+fadeLen:
		return 0
	default:
		return 1 - float32(c.concealed-hold)/float32(fadeLen)
	}
}

// findPeriod returns the lag in [minPeriod, maxPeriod] at which the last 20 ms of the
// history best match the audio one lag earlier, and whether that match is good enough to
// call the signal periodic.
func (c *Concealer) findPeriod() (int, bool) {
	ref := c.hist[histLen-corrLen:]
	var refE float64
	for _, v := range ref {
		refE += float64(v) * float64(v)
	}
	if refE < 1e-9 {
		return maxPeriod, false // digital silence: nothing to continue
	}
	var corrs [maxPeriod + 1]float64
	bestCorr := -1.0
	for lag := minPeriod; lag <= maxPeriod; lag++ {
		cand := c.hist[histLen-corrLen-lag : histLen-lag]
		var xy, yy float64
		for i, v := range ref {
			xy += float64(v) * float64(cand[i])
			yy += float64(cand[i]) * float64(cand[i])
		}
		if yy < 1e-12 {
			continue
		}
		corrs[lag] = xy / math.Sqrt(refE*yy)
		bestCorr = math.Max(bestCorr, corrs[lag])
	}
	// Speech is only locally periodic, so the most recent cycle predicts the next one
	// best: take the shortest lag that is a correlation peak nearly as good as the best.
	bestLag := maxPeriod
	for lag := minPeriod + 1; lag < maxPeriod; lag++ {
		if corrs[lag] >= 0.9*bestCorr && corrs[lag] >= corrs[lag-1] && corrs[lag] >= corrs[lag+1] {
			bestLag = lag
			break
		}
	}
	if bestCorr < voicedThreshold {
		return maxPeriod, false
	}
	return bestLag, true
}

// remember appends frame to the history.
func (c *Concealer) remember(frame []float32) {
	if len(frame) >= histLen {
		copy(c.hist[:], frame[len(frame)-histLen:])
		c.filled = histLen
		return
	}
	copy(c.hist[:], c.hist[len(frame):])
	copy(c.hist[histLen-len(frame):], frame)
	c.filled = min(histLen, c.filled+len(frame))
}
