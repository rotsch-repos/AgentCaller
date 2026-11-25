package agentcaller

import (
	"io"
	"sync"
	"time"

	"github.com/rotsch-repos/agentcaller/resample"
)

// LiveConfig configures a LiveSource.
type LiveConfig struct {
	// Rate is the sample rate of the audio that will be written, in Hz. 0 means
	// SampleRate (16 kHz); anything else is converted with a band-limited resampler.
	Rate int
	// MaxBuffered bounds the audio waiting to be sent. When a write would exceed it, the
	// oldest audio is dropped, so the peer never hears more than this much delay. Use it
	// for a real-time producer (a microphone, another call). 0 means unbounded, which
	// is right for speech synthesis: it arrives faster than real time and every sample
	// must be played.
	MaxBuffered time.Duration
}

// LiveStats counts what happened to the audio written to a LiveSource, in 16 kHz samples.
type LiveStats struct {
	Written   uint64 // accepted from the writer (after resampling)
	Played    uint64 // handed to the call
	Dropped   uint64 // discarded because MaxBuffered was exceeded
	Flushed   uint64 // discarded by Flush
	Underruns uint64 // send ticks that found too little audio while a write was pending
}

// LiveSource is an AudioSource for audio that is produced while the call runs: speech
// synthesis, a pipe, a microphone. The file and stream sources block the call's 60 ms
// send tick until a whole frame has been read, which stalls the packets that keep the
// relay bridge open; a LiveSource never blocks either side:
//
//   - Write queues samples and returns at once.
//   - ReadFrame returns a frame if one is queued and nil (the call sends silence)
//     if not. A partial frame waits one tick for more audio, then goes out padded, so
//     the end of an utterance is delayed by at most 60 ms.
//   - Flush discards what is queued: the peer started talking, stop mid-sentence.
//
// CloseWrite ends the stream after the queued audio has played (the Player then fires
// OnFinish). All methods are safe for concurrent use.
type LiveSource struct {
	mu     sync.Mutex
	queue  []float32
	head   int
	limit  int // max queued samples, 0 = unbounded
	res    *resample.Resampler
	stale  bool // a partial frame has already waited one tick
	eof    bool
	closed bool
	stats  LiveStats
	scr    []float32
	carry  []byte // odd trailing byte of a WritePCM16 call
}

// NewLiveSource returns an empty live source.
func NewLiveSource(cfg LiveConfig) *LiveSource {
	s := &LiveSource{limit: int(cfg.MaxBuffered.Seconds() * SampleRate)}
	if cfg.Rate != 0 && cfg.Rate != SampleRate {
		s.res = resample.New(cfg.Rate, SampleRate)
	}
	return s
}

// Write queues mono samples in [-1, 1] at the configured rate. It never blocks and
// keeps no reference to samples.
func (s *LiveSource) Write(samples []float32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.eof || len(samples) == 0 {
		return
	}
	if s.res != nil {
		s.scr = s.res.Process(samples, s.scr[:0])
		samples = s.scr
	}
	s.enqueue(samples)
}

func (s *LiveSource) enqueue(samples []float32) {
	if len(samples) == 0 {
		return
	}
	if s.head > 0 && s.head >= len(s.queue)/2 { // reclaim the played part
		n := copy(s.queue, s.queue[s.head:])
		s.queue, s.head = s.queue[:n], 0
	}
	s.queue = append(s.queue, samples...)
	s.stats.Written += uint64(len(samples))
	s.stale = false
	if over := len(s.queue) - s.head - s.limit; s.limit > 0 && over > 0 {
		s.head += over
		s.stats.Dropped += uint64(over)
	}
}

// WritePCM16 queues signed 16-bit little-endian mono PCM at the configured rate. A
// trailing odd byte is kept for the next call, so it can be fed straight from a pipe.
func (s *LiveSource) WritePCM16(b []byte) {
	s.mu.Lock()
	if len(s.carry) > 0 {
		b = append(s.carry, b...)
		s.carry = nil
	}
	if len(b)%2 == 1 {
		s.carry = []byte{b[len(b)-1]}
		b = b[:len(b)-1]
	}
	s.mu.Unlock()
	samples := make([]float32, len(b)/2)
	for i := range samples {
		samples[i] = float32(int16(uint16(b[2*i])|uint16(b[2*i+1])<<8)) / 32768
	}
	s.Write(samples)
}

// Flush discards the queued audio and reports how much that was. Later writes play
// normally. Use it for barge-in.
func (s *LiveSource) Flush() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.queue) - s.head
	s.queue, s.head, s.stale = s.queue[:0], 0, false
	s.stats.Flushed += uint64(n)
	return samplesDuration(n)
}

// Buffered reports how much audio is waiting to be sent.
func (s *LiveSource) Buffered() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return samplesDuration(len(s.queue) - s.head)
}

// Stats returns the counters so far.
func (s *LiveSource) Stats() LiveStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stats
}

// CloseWrite marks the end of the stream: the queued audio still plays, then ReadFrame
// returns io.EOF.
func (s *LiveSource) CloseWrite() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.eof || s.closed {
		return
	}
	if s.res != nil {
		s.enqueue(s.res.Flush(nil))
	}
	s.eof = true
}

// ReadFrame implements AudioSource. It never blocks: a nil frame with a nil error means
// "nothing to send right now".
func (s *LiveSource) ReadFrame() ([]float32, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, io.EOF
	}
	n := len(s.queue) - s.head
	switch {
	case n >= FrameSamples:
		n = FrameSamples
	case n == 0:
		if s.eof {
			return nil, io.EOF
		}
		return nil, nil
	case !s.eof && !s.stale:
		// Less than a frame: give the writer one more tick before padding with silence.
		s.stale = true
		s.stats.Underruns++
		return nil, nil
	}
	frame := make([]float32, FrameSamples)
	copy(frame, s.queue[s.head:s.head+n])
	s.head += n
	s.stale = false
	s.stats.Played += uint64(n)
	return frame, nil
}

// Close implements AudioSource: it drops everything and ends the stream.
func (s *LiveSource) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	s.queue, s.head = nil, 0
	return nil
}

func samplesDuration(n int) time.Duration {
	return time.Duration(n) * time.Second / SampleRate
}
