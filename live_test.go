package agentcaller

import (
	"io"
	"testing"
	"time"
)

func ramp(n int, start float32) []float32 {
	out := make([]float32, n)
	for i := range out {
		out[i] = start + float32(i)
	}
	return out
}

// The send tick must never wait for audio: an empty source answers at once with "send
// silence", and whole frames come out exactly as written.
func TestLiveSourceNeverBlocksAndKeepsOrder(t *testing.T) {
	s := NewLiveSource(LiveConfig{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		if f, err := s.ReadFrame(); f != nil || err != nil {
			t.Errorf("empty source: frame %v, err %v; want nil, nil", f != nil, err)
		}
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("ReadFrame blocked on an empty live source")
	}

	s.Write(ramp(2*FrameSamples+100, 0))
	for f := 0; f < 2; f++ {
		frame, err := s.ReadFrame()
		if err != nil || len(frame) != FrameSamples {
			t.Fatalf("frame %d: len %d, err %v", f, len(frame), err)
		}
		for i, v := range frame {
			if v != float32(f*FrameSamples+i) {
				t.Fatalf("frame %d sample %d = %v", f, i, v)
			}
		}
	}
	// 100 samples left: wait one tick for more, then send them padded.
	if frame, _ := s.ReadFrame(); frame != nil {
		t.Fatal("a partial frame went out without waiting one tick")
	}
	frame, err := s.ReadFrame()
	if err != nil || len(frame) != FrameSamples || frame[99] != float32(2*FrameSamples+99) || frame[100] != 0 {
		t.Fatalf("padded tail wrong: len %d err %v", len(frame), err)
	}
	st := s.Stats()
	if st.Written != 2*FrameSamples+100 || st.Played != st.Written || st.Underruns != 1 {
		t.Fatalf("stats %+v", st)
	}
}

// A writer that keeps delivering must not get its audio padded mid-stream.
func TestLiveSourcePartialFrameWaitsForWriter(t *testing.T) {
	s := NewLiveSource(LiveConfig{})
	s.Write(ramp(600, 0))
	if frame, _ := s.ReadFrame(); frame != nil {
		t.Fatal("partial frame sent although the writer may still deliver")
	}
	s.Write(ramp(600, 600))
	frame, _ := s.ReadFrame()
	if frame == nil || frame[959] != 959 {
		t.Fatal("frame not contiguous across two writes")
	}
}

func TestLiveSourceFlushIsBargeIn(t *testing.T) {
	s := NewLiveSource(LiveConfig{})
	s.Write(make([]float32, 5*SampleRate))
	if _, err := s.ReadFrame(); err != nil {
		t.Fatal(err)
	}
	if got, want := s.Flush(), 5*time.Second-60*time.Millisecond; got != want {
		t.Fatalf("flushed %v, want %v", got, want)
	}
	if frame, err := s.ReadFrame(); frame != nil || err != nil {
		t.Fatal("audio still playing after Flush")
	}
	s.Write(ramp(FrameSamples, 7))
	if frame, _ := s.ReadFrame(); frame == nil || frame[0] != 7 {
		t.Fatal("writes after Flush do not play")
	}
}

// With MaxBuffered the delay heard by the peer is bounded: the oldest audio goes.
func TestLiveSourceBoundsLatency(t *testing.T) {
	s := NewLiveSource(LiveConfig{MaxBuffered: 120 * time.Millisecond})
	s.Write(ramp(10*FrameSamples, 0))
	if got := s.Buffered(); got != 120*time.Millisecond {
		t.Fatalf("buffered %v, want 120ms", got)
	}
	frame, _ := s.ReadFrame()
	if frame == nil || frame[0] != float32(8*FrameSamples) {
		t.Fatalf("kept the old audio instead of the newest (first sample %v)", frame[0])
	}
	if st := s.Stats(); st.Dropped != 8*FrameSamples {
		t.Fatalf("dropped %d samples, want %d", st.Dropped, 8*FrameSamples)
	}
}

func TestLiveSourceCloseWriteDrainsThenEOF(t *testing.T) {
	s := NewLiveSource(LiveConfig{Rate: 48000})
	s.Write(make([]float32, 48000)) // 1 s at 48 kHz
	s.CloseWrite()
	s.Write(make([]float32, 48000)) // ignored after CloseWrite
	total := 0
	for {
		frame, err := s.ReadFrame()
		if err == io.EOF {
			break
		}
		if err != nil || frame == nil {
			t.Fatalf("frame nil=%v err=%v before EOF", frame == nil, err)
		}
		total += len(frame)
	}
	if st := s.Stats(); st.Written != SampleRate || st.Played != SampleRate {
		t.Fatalf("1 s at 48 kHz became %d written / %d played samples, want %d", st.Written, st.Played, SampleRate)
	}
	if total != 17*FrameSamples { // 16000 samples padded to whole frames
		t.Fatalf("%d samples out", total)
	}
}

func TestLiveSourcePCM16OddChunks(t *testing.T) {
	s := NewLiveSource(LiveConfig{})
	pcm := make([]byte, 2*FrameSamples)
	for i := 0; i < FrameSamples; i++ {
		pcm[2*i], pcm[2*i+1] = byte(i), byte(i>>8)
	}
	for rest := pcm; len(rest) > 0; {
		n := min(333, len(rest))
		s.WritePCM16(rest[:n])
		rest = rest[n:]
	}
	frame, _ := s.ReadFrame()
	if frame == nil {
		t.Fatal("no frame")
	}
	for i, v := range frame {
		if v != float32(i)/32768 {
			t.Fatalf("sample %d = %v", i, v)
		}
	}
}

// Through a Player, a live source that runs dry keeps the player playing (silence goes
// out), and finishing it fires OnFinish.
func TestLiveSourceWithPlayer(t *testing.T) {
	s := NewLiveSource(LiveConfig{})
	p := NewPlayer()
	finished := make(chan struct{})
	p.OnFinish(func() { close(finished) })
	p.Play(s)
	if p.nextFrame() != nil || p.State() != PlayerPlaying {
		t.Fatal("an empty live source must leave the player playing silence")
	}
	s.Write(ramp(FrameSamples, 1))
	if f := p.nextFrame(); f == nil || f[0] != 1 {
		t.Fatal("live audio did not reach the player")
	}
	s.CloseWrite()
	p.nextFrame()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("OnFinish not fired after CloseWrite")
	}
}
