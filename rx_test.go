package agentcaller

import (
	"encoding/binary"
	"math"
	"math/rand"
	"os"
	"testing"
	"time"

	"github.com/rotsch-repos/agentcaller/mlow"
)

// speechFrames loads the recorded call fixture as 60 ms frames.
func speechFrames(tb testing.TB) [][]float32 {
	tb.Helper()
	raw, err := os.ReadFile("cmd/agentcaller/testdata/call-opus16k.wav")
	if err != nil {
		tb.Skipf("speech fixture missing: %v", err)
	}
	pcm := raw[44:]
	frames := make([][]float32, len(pcm)/2/FrameSamples)
	for f := range frames {
		frames[f] = make([]float32, FrameSamples)
		for i := range frames[f] {
			frames[f][i] = float32(int16(binary.LittleEndian.Uint16(pcm[2*(f*FrameSamples+i):]))) / 32768
		}
	}
	return frames
}

type collectSink struct{ pcm []float32 }

func (c *collectSink) WriteFrame(f []float32) error { c.pcm = append(c.pcm, f...); return nil }
func (c *collectSink) Close() error                 { return nil }

func encodeAll(tb testing.TB, frames [][]float32) [][]byte {
	tb.Helper()
	enc := mlow.NewMlowEncoder()
	out := make([][]byte, len(frames))
	for i, f := range frames {
		p, err := enc.Encode(f)
		if err != nil {
			tb.Fatal(err)
		}
		out[i] = p
	}
	return out
}

// TestReceiverCleanStreamIsUntouched: with nothing lost the new receive path must hand
// the sink exactly what the bare decoder produces, packet by packet, with no delay.
func TestReceiverCleanStreamIsUntouched(t *testing.T) {
	packets := encodeAll(t, speechFrames(t))
	ref := mlow.NewMlowDecoder()
	var want []float32
	for _, p := range packets {
		want = append(want, ref.Decode(p)...)
	}
	sink := &collectSink{}
	rx := newAudioReceiver(mlow.NewMlowDecoder())
	now := time.Now()
	for i, p := range packets {
		if n := rx.push(uint16(65530+i), uint32(i)*FrameSamples, p, now, sink, false); n != 1 {
			t.Fatalf("packet %d: decoded %d packets on arrival, want 1", i, n)
		}
		if len(sink.pcm) != (i+1)*FrameSamples {
			t.Fatalf("packet %d: sink has %d samples, want %d (delay added)", i, len(sink.pcm), (i+1)*FrameSamples)
		}
	}
	for i := range want {
		if math.Float32bits(want[i]) != math.Float32bits(sink.pcm[i]) {
			t.Fatalf("sample %d differs from the bare decoder", i)
		}
	}
}

// TestReceiverLossAndReorder runs the fixture through a lossy, reordering network and
// checks what a listener depends on: every packet is accounted for, reordered packets
// cost nothing, the timeline keeps its length, and a gap is silence unless concealment
// was asked for.
func TestReceiverLossAndReorder(t *testing.T) {
	frames := speechFrames(t)
	packets := encodeAll(t, frames)
	ref := mlow.NewMlowDecoder()
	var clean []float32
	for _, p := range packets {
		clean = append(clean, ref.Decode(p)...)
	}

	rng := rand.New(rand.NewSource(3))
	lost := make([]bool, len(packets))
	for i := 5; i < len(packets)-1; i++ { // keep the first packets so there is history
		lost[i] = rng.Float64() < 0.10 && !lost[i-1]
	}
	run := func(conceal bool) []float32 {
		sink := &collectSink{}
		rx := newAudioReceiver(mlow.NewMlowDecoder())
		now := time.Now()
		send := func(i int) { rx.push(uint16(i), uint32(i)*FrameSamples, packets[i], now, sink, conceal) }
		for i := 0; i < len(packets); i++ {
			now = now.Add(60 * time.Millisecond)
			if dl, holding := rx.ro.Deadline(); holding && !now.Before(dl) {
				rx.mu.Lock()
				rx.ev = rx.ro.Flush(now, rx.ev[:0])
				rx.process()
				rx.mu.Unlock()
			}
			if lost[i] {
				continue
			}
			// Every 7th surviving pair arrives swapped.
			if i > 0 && i%7 == 0 && i+1 < len(packets) && !lost[i+1] {
				send(i + 1)
				send(i)
				i++
				now = now.Add(60 * time.Millisecond)
				continue
			}
			send(i)
		}
		st := rx.stats()
		nLost := 0
		for _, l := range lost {
			if l {
				nLost++
			}
		}
		if int(st.Lost) != nLost {
			t.Fatalf("reported %d lost packets, network dropped %d", st.Lost, nLost)
		}
		if st.Late != 0 || st.Reordered == 0 {
			t.Fatalf("reordered packets were not repaired: %+v", st.Stats)
		}
		return sink.pcm
	}
	for _, conceal := range []bool{false, true} {
		got := run(conceal)
		if len(got) != len(clean) {
			t.Fatalf("conceal=%v: timeline changed: %d samples out, %d in", conceal, len(got), len(clean))
		}
		var gapE float64
		for i, l := range lost {
			if !l {
				continue
			}
			for _, v := range got[i*FrameSamples : (i+1)*FrameSamples] {
				gapE += float64(v) * float64(v)
			}
		}
		if !conceal && gapE != 0 {
			t.Fatalf("default gap fill is not silence (energy %g)", gapE)
		}
		if conceal && gapE == 0 {
			t.Fatal("concealment requested but the gaps are silent")
		}
	}
}
