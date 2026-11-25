package agentcaller

import (
	"sync"
	"time"

	"github.com/rotsch-repos/agentcaller/jitter"
	"github.com/rotsch-repos/agentcaller/mlow"
	"github.com/rotsch-repos/agentcaller/plc"
)

// maxConcealPackets bounds how much audio one loss event is filled with. It keeps the
// timeline honest for a burst without flooding the sink after a long outage.
const maxConcealPackets = 16

// ReceiveStats describes the inbound audio path of a call.
type ReceiveStats struct {
	jitter.Stats
	// ConcealedSamples is the amount of audio filled in for lost packets (silence, or a
	// synthesised continuation with Call.ConcealLosses).
	ConcealedSamples uint64
}

// audioReceiver turns unprotected inbound audio packets into an ordered PCM stream with
// an intact timeline: reorder → decode → fill losses → sink. Packets in sequence pass
// straight through; nothing is delayed unless a gap opens (see package jitter).
//
// A lost packet is replaced by silence of the same length unless conceal is set. That
// default is a measurement, not a shortcut: on German speech through MLow with 10 % and
// 20 % packet loss, Whisper (small) made 9 and 21 word errors per 108 words with silent
// gaps, and 15 and 30 with the waveform concealment of package plc (one Piper sentence,
// three loss patterns per rate: a small sample, but the same direction in all six
// pairs but one tie). Concealment is for human ears (a softphone speaker), so it is
// opt-in via Call.ConcealLosses.
type audioReceiver struct {
	mu        sync.Mutex
	ro        *jitter.Reorder
	dec       *mlow.MlowDecoder
	plc       plc.Concealer
	sink      AudioSink
	timer     *time.Timer
	ev        []jitter.Event
	concealed uint64
	conceal   bool // synthesise a continuation for lost packets instead of silence
	closed    bool

	// onDecoded, if set, observes every decoded packet (diagnostics).
	onDecoded func(seq uint16, frame []float32, payloadLen int)
	// onLost, if set, observes every loss event.
	onLost func(packets int, tsGap uint32, samples int)
}

func newAudioReceiver(dec *mlow.MlowDecoder) *audioReceiver {
	return &audioReceiver{ro: jitter.New(jitter.Config{}), dec: dec}
}

// push feeds one packet that arrived at now. The payload is copied, so the caller may
// reuse its buffer. Decoded audio goes to sink; it reports how many packets were decoded
// by this call.
func (r *audioReceiver) push(seq uint16, ts uint32, payload []byte, now time.Time, sink AudioSink, conceal bool) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return 0
	}
	r.sink, r.conceal = sink, conceal
	p := jitter.Packet{Seq: seq, TS: ts, Payload: append([]byte(nil), payload...)}
	r.ev = r.ro.Push(p, now, r.ev[:0])
	n := r.process()
	r.arm(now)
	return n
}

// flush runs when the reorder deadline expires without a new packet.
func (r *audioReceiver) flush() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	now := time.Now()
	r.ev = r.ro.Flush(now, r.ev[:0])
	r.process()
	r.arm(now)
}

// arm schedules flush for the current reorder deadline, if packets are held.
func (r *audioReceiver) arm(now time.Time) {
	dl, holding := r.ro.Deadline()
	if !holding {
		return
	}
	d := max(dl.Sub(now), 0)
	if r.timer == nil {
		r.timer = time.AfterFunc(d, r.flush)
		return
	}
	r.timer.Reset(d)
}

// process decodes and conceals the pending events in order.
func (r *audioReceiver) process() (decoded int) {
	for _, e := range r.ev {
		switch e.Kind {
		case jitter.Audio:
			frame := r.dec.Decode(e.Packet.Payload)
			if r.onDecoded != nil {
				r.onDecoded(e.Packet.Seq, frame, len(e.Packet.Payload))
			}
			decoded++
			// Always record the frame so the concealer has history when it is switched
			// on mid-call; it alters a frame only right after a gap it concealed itself.
			r.write(r.plc.Good(frame))
		case jitter.Lost:
			packets := min(e.Packets, maxConcealPackets)
			for i := 0; i < packets; i++ {
				if r.conceal {
					r.write(r.plc.Conceal(FrameSamples))
				} else {
					r.write(make([]float32, FrameSamples))
				}
			}
			r.concealed += uint64(packets * FrameSamples)
			if r.onLost != nil {
				r.onLost(e.Packets, e.TSGap, packets*FrameSamples)
			}
		}
	}
	return decoded
}

func (r *audioReceiver) write(frame []float32) {
	if r.sink != nil && len(frame) > 0 {
		_ = r.sink.WriteFrame(frame)
	}
}

func (r *audioReceiver) stats() ReceiveStats {
	r.mu.Lock()
	defer r.mu.Unlock()
	return ReceiveStats{Stats: r.ro.Stats(), ConcealedSamples: r.concealed}
}

// close stops the timer; later pushes are ignored.
func (r *audioReceiver) close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	if r.timer != nil {
		r.timer.Stop()
	}
}
