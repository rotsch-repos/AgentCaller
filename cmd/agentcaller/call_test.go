package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	agentcaller "github.com/rotsch-repos/agentcaller"
	"github.com/rotsch-repos/agentcaller/rtp"
)

func TestParseCallArgs(t *testing.T) {
	cfg, err := parseCallArgs([]string{"-in-rate", "22050", "-barge-in", "-out", "none", "+41790000000"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.target != "+41790000000" || cfg.inRate != 22050 || !cfg.bargeIn || cfg.outPath != "none" ||
		cfg.inPath != "-" || cfg.conceal || cfg.answer || cfg.endSilence != 700*time.Millisecond {
		t.Fatalf("unexpected config: %+v", cfg)
	}
	for _, bad := range [][]string{
		{},                                     // no target
		{"a", "b"},                             // two targets
		{"-in-rate", "100", "x"},               // absurd rate
		{"-end-silence", "0s", "x"},            // would end speech on every frame
		{"-max-duration", "-1s", "x"},          // negative
		{"-hangup-on-eof", "-in", "none", "x"}, // nothing can ever reach EOF
	} {
		if _, err := parseCallArgs(bad, io.Discard); err == nil {
			t.Fatalf("arguments %v accepted", bad)
		}
	}
	if _, err := parseCallArgs(nil, io.Discard); !errors.Is(err, errUsage) {
		t.Fatalf("missing target: %v, want a usage error", err)
	}
}

func tone(frames int, amp float64) []float32 {
	out := make([]float32, frames*frameSamples)
	for i := range out {
		out[i] = float32(amp * math.Sin(2*math.Pi*440*float64(i)/agentcaller.SampleRate))
	}
	return out
}

func TestSpeechGateSynthetic(t *testing.T) {
	g := newSpeechGate(300 * time.Millisecond)
	var got []string
	feed := func(samples []float32) {
		for i := 0; i+frameSamples <= len(samples); i += frameSamples {
			started, ended, at, length := g.feed(samples[i : i+frameSamples])
			if started {
				got = append(got, "start@"+at.String())
			}
			if ended {
				got = append(got, "end@"+at.String()+"+"+length.String())
			}
		}
	}
	feed(make([]float32, 10*frameSamples)) // 600 ms of silence
	feed(tone(1, 0.2))                     // a click: one loud frame
	feed(make([]float32, 4*frameSamples))
	feed(tone(10, 0.2)) // 600 ms of speech starting at 900 ms
	feed(make([]float32, 3*frameSamples))
	feed(tone(5, 0.2)) // a 180 ms pause does not end the turn
	feed(make([]float32, 10*frameSamples))
	want := []string{"start@900ms", "end@900ms+1.08s"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("events %v, want %v", got, want)
	}
}

// On the call fixture (low noise, then speech from about 2.0 s) the gate must stay
// shut during the noise and open at the voice.
func TestSpeechGateOnCallFixture(t *testing.T) {
	path := wavPath("call-opus16k.wav")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("fixture missing: %v", err)
	}
	src, err := agentcaller.WAVFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	g := newSpeechGate(700 * time.Millisecond)
	var starts []time.Duration
	for {
		frame, err := src.ReadFrame()
		if len(frame) > 0 {
			if started, _, at, _ := g.feed(frame); started {
				starts = append(starts, at)
			}
		}
		if err != nil {
			break
		}
	}
	if len(starts) == 0 || starts[0] < 1900*time.Millisecond || starts[0] > 2300*time.Millisecond {
		t.Fatalf("speech starts at %v, the voice begins at about 2.1 s", starts)
	}
}

type stalledWriter struct{ release chan struct{} }

func (w stalledWriter) Write(p []byte) (int, error) { <-w.release; return len(p), nil }

// The media loop must never wait for a slow reader of the output pipe.
func TestPCMPipeNeverBlocks(t *testing.T) {
	w := stalledWriter{release: make(chan struct{})}
	p := newPCMPipe(w)
	begin := time.Now()
	for i := 0; i < 500; i++ {
		p.write(make([]float32, frameSamples))
	}
	if took := time.Since(begin); took > time.Second {
		t.Fatalf("500 frames into a stalled reader took %v", took)
	}
	if d := p.dropped.Load(); d < 400 || d > 449 {
		t.Fatalf("dropped %d frames, want the excess over the 50 frame queue", d)
	}
	close(w.release)
	p.close()
	p.write(make([]float32, frameSamples)) // a late frame after close must not panic
}

func TestPCMPipeSamples(t *testing.T) {
	var buf bytes.Buffer
	p := newPCMPipe(&buf)
	p.write([]float32{0, 0.5, -0.5, 1, -1, 2, -2})
	p.close()
	want := []int16{0, 16384, -16384, 32767, -32767, 32767, -32768}
	for i, w := range want {
		if got := int16(binary.LittleEndian.Uint16(buf.Bytes()[2*i:])); got != w {
			t.Fatalf("sample %d: %d, want %d", i, got, w)
		}
	}
}

// fakeDuplexCall stands in for a live call: the test pulls the outgoing frames the way
// the media loop does and pushes the peer's audio into the sink.
type fakeDuplexCall struct {
	mu      sync.Mutex
	phase   agentcaller.CallPhase
	onReady func()
	onEnd   func(string)
	sink    agentcaller.AudioSink
	src     agentcaller.AudioSource
	conceal bool
	hungUp  chan struct{}
	once    sync.Once
}

func newFakeDuplexCall(phase agentcaller.CallPhase) *fakeDuplexCall {
	return &fakeDuplexCall{phase: phase, hungUp: make(chan struct{})}
}

func (c *fakeDuplexCall) OnReady(fn func())                         { c.mu.Lock(); c.onReady = fn; c.mu.Unlock() }
func (c *fakeDuplexCall) OnEnd(fn func(string))                     { c.mu.Lock(); c.onEnd = fn; c.mu.Unlock() }
func (c *fakeDuplexCall) OnStateChange(func(agentcaller.CallPhase)) {}
func (c *fakeDuplexCall) State() agentcaller.CallPhase              { return c.phase }
func (c *fakeDuplexCall) Receive(s agentcaller.AudioSink)           { c.mu.Lock(); c.sink = s; c.mu.Unlock() }
func (c *fakeDuplexCall) ConcealLosses(on bool)                     { c.conceal = on }
func (c *fakeDuplexCall) ReceiveStats() agentcaller.ReceiveStats    { return agentcaller.ReceiveStats{} }
func (c *fakeDuplexCall) Hangup() error                             { c.once.Do(func() { close(c.hungUp) }); return nil }
func (c *fakeDuplexCall) Play(src agentcaller.AudioSource) *agentcaller.Player {
	c.mu.Lock()
	c.src = src
	c.mu.Unlock()
	return agentcaller.NewPlayer()
}

func (c *fakeDuplexCall) source(t *testing.T) agentcaller.AudioSource {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		c.mu.Lock()
		src := c.src
		c.mu.Unlock()
		if src != nil {
			return src
		}
	}
	t.Fatal("the call never got an audio source")
	return nil
}

// pull reads one outgoing frame like the media loop: nil means silence, done means EOF.
func pull(src agentcaller.AudioSource) (frame []float32, done bool) {
	frame, err := src.ReadFrame()
	if frame == nil {
		frame = make([]float32, frameSamples)
	}
	return frame, err != nil
}

func waitQueued(t *testing.T, d *duplex, want time.Duration) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); d.live.Buffered() < want; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("only %v of input queued", d.live.Buffered())
		}
	}
}

func pcm16(samples []float32) []byte {
	b := make([]byte, 2*len(samples))
	for i, s := range samples {
		binary.LittleEndian.PutUint16(b[2*i:], uint16(int16(math.Round(float64(s)*32767))))
	}
	return b
}

type event map[string]any

func parseEvents(t *testing.T, raw string) (events []event, names []string) {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		var e event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("event line %q: %v", line, err)
		}
		events = append(events, e)
		names = append(names, e["event"].(string))
	}
	return events, names
}

func find(events []event, name string) event {
	for _, e := range events {
		if e["event"] == name {
			return e
		}
	}
	return nil
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuffer) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

// Both directions at once: the input stream reaches the call sample for sample, the
// peer's audio reaches the output sample for sample, the peer's speech is reported, and
// the call is hung up when the input has been sent.
func TestDuplexBothDirections(t *testing.T) {
	said := tone(25, 0.3) // 1.5 s for the peer to hear
	var peer []float32    // what the peer says meanwhile: speech from 300 ms to 900 ms
	peer = append(peer, make([]float32, 5*frameSamples)...)
	peer = append(peer, tone(10, 0.2)...)
	peer = append(peer, make([]float32, 25*frameSamples)...)

	var evBuf lockedBuffer
	var out bytes.Buffer
	cfg := callConfig{inRate: agentcaller.SampleRate, endSilence: 300 * time.Millisecond, hangupOnEOF: true, answerTimeout: time.Second}
	d := newDuplex(cfg, &eventLog{w: &evBuf, start: time.Now()}, &out)
	call := newFakeDuplexCall(agentcaller.CallPhaseActive)
	result := make(chan error, 1)
	go func() { result <- d.run(context.Background(), call, bytes.NewReader(pcm16(said)), nil) }()

	src := call.source(t)
	waitQueued(t, d, 1500*time.Millisecond)
	var sent []float32
	fed := 0
	for i := 0; i < 40; i++ {
		frame, done := pull(src)
		sent = append(sent, frame...)
		if err := d.WriteFrame(peer[i*frameSamples : (i+1)*frameSamples]); err != nil {
			t.Fatal(err)
		}
		fed++
		if done {
			break
		}
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the call did not end after the input was sent")
	}
	select {
	case <-call.hungUp:
	default:
		t.Fatal("the call was not hung up")
	}

	for i, v := range said { // exactly what a 16-bit stream can carry, with no gap
		if want := float32(int16(math.Round(float64(v)*32767))) / 32768; sent[i] != want {
			t.Fatalf("outgoing sample %d is %v, want %v", i, sent[i], want)
		}
	}
	if got, want := out.Bytes(), pcm16(peer[:fed*frameSamples]); !bytes.Equal(got, want) {
		t.Fatalf("received audio differs: %d bytes out, %d in", len(got), len(want))
	}

	events, names := parseEvents(t, evBuf.String())
	if names[0] != "ready" || names[len(names)-1] != "ended" || names[len(names)-2] != "stats" {
		t.Fatalf("event order %v", names)
	}
	if e := find(events, "speech_start"); e == nil || e["at"] != 0.3 {
		t.Fatalf("speech_start %v, want at 0.3 s (events %v)", e, names)
	}
	if e := find(events, "speech_end"); e == nil || e["at"] != 0.3 || e["duration"] != 0.6 {
		t.Fatalf("speech_end %v, want 0.6 s from 0.3 s", e)
	}
	if find(events, "input_eof") == nil || find(events, "played") == nil || find(events, "flushed") != nil {
		t.Fatalf("events %v", names)
	}
	if e := find(events, "ended"); e["reason"] != "input finished" {
		t.Fatalf("ended: %v", e)
	}
	if e := find(events, "stats"); e["tx_played_ms"] != 1500.0 || e["out_dropped_frames"] != 0.0 {
		t.Fatalf("stats: %v", e)
	}
}

// Barge-in: when the peer starts to speak, what the agent still had queued is thrown
// away at once. The control channel can do the same and end the call.
func TestDuplexBargeInAndControl(t *testing.T) {
	var evBuf lockedBuffer
	cfg := callConfig{inRate: agentcaller.SampleRate, endSilence: 300 * time.Millisecond, bargeIn: true}
	d := newDuplex(cfg, &eventLog{w: &evBuf, start: time.Now()}, nil)
	call := newFakeDuplexCall(agentcaller.CallPhaseActive)
	control := make(chan string)
	result := make(chan error, 1)
	in, feed := io.Pipe()
	go func() { result <- d.run(context.Background(), call, in, control) }()

	src := call.source(t)
	go feed.Write(pcm16(tone(25, 0.3)))
	waitQueued(t, d, 1500*time.Millisecond)
	for i := 0; i < 3; i++ { // 180 ms reach the peer
		if frame, _ := pull(src); frameRMS(frame) < 0.1 {
			t.Fatalf("frame %d is not the queued audio", i)
		}
	}
	speech := tone(2, 0.2)
	_ = d.WriteFrame(speech[:frameSamples])
	if d.live.Buffered() == 0 {
		t.Fatal("a single loud frame already cut the agent off")
	}
	_ = d.WriteFrame(speech[frameSamples:])
	if d.live.Buffered() != 0 {
		t.Fatalf("%v still queued after the peer started to speak", d.live.Buffered())
	}
	if frame, _ := pull(src); frameRMS(frame) != 0 {
		t.Fatal("audio still went out after the barge-in")
	}

	go feed.Write(pcm16(tone(10, 0.3))) // the agent's next sentence
	waitQueued(t, d, 600*time.Millisecond)
	control <- "stats"
	control <- `{"cmd":"flush"}`
	control <- "sing"
	control <- "hangup"
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	events, names := parseEvents(t, evBuf.String())
	var flushes []event
	for _, e := range events {
		if e["event"] == "flushed" {
			flushes = append(flushes, e)
		}
	}
	if len(flushes) != 2 || flushes[0]["cause"] != "barge-in" || flushes[0]["ms"] != 1320.0 ||
		flushes[1]["cause"] != "control" || flushes[1]["ms"] != 600.0 {
		t.Fatalf("flushes %v", flushes)
	}
	if e := find(events, "control_error"); e == nil || !strings.Contains(e["error"].(string), "sing") {
		t.Fatalf("unknown command not reported: %v", names)
	}
	if e := find(events, "ended"); e["reason"] != "hangup command" {
		t.Fatalf("ended: %v", e)
	}
	if e := find(events, "stats"); e["tx_queued_ms"] != 600.0 || e["tx_flushed_ms"] != 1320.0 {
		t.Fatalf("stats before the flush: %v", e)
	}
}

func TestDuplexNotAnswered(t *testing.T) {
	var evBuf lockedBuffer
	d := newDuplex(callConfig{inRate: agentcaller.SampleRate, endSilence: time.Second, answerTimeout: 30 * time.Millisecond}, &eventLog{w: &evBuf, start: time.Now()}, nil)
	call := newFakeDuplexCall(agentcaller.CallPhaseRinging)
	if err := d.run(context.Background(), call, nil, nil); err == nil {
		t.Fatal("an unanswered call reported success")
	}
	select {
	case <-call.hungUp:
	default:
		t.Fatal("the unanswered call was left ringing")
	}
	if _, names := parseEvents(t, evBuf.String()); names[len(names)-1] != "ended" || strings.Contains(strings.Join(names, " "), "ready") {
		t.Fatalf("events %v", names)
	}
}

func TestDuplexPeerHangsUp(t *testing.T) {
	var evBuf lockedBuffer
	d := newDuplex(callConfig{inRate: agentcaller.SampleRate, endSilence: time.Second}, &eventLog{w: &evBuf, start: time.Now()}, nil)
	call := newFakeDuplexCall(agentcaller.CallPhaseActive)
	result := make(chan error, 1)
	go func() { result <- d.run(context.Background(), call, nil, nil) }()
	call.source(t)
	call.mu.Lock()
	end := call.onEnd
	call.mu.Unlock()
	end("remote hangup")
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	events, _ := parseEvents(t, evBuf.String())
	if e := find(events, "ended"); e["reason"] != "remote hangup" {
		t.Fatalf("ended: %v", e)
	}
}

// A video file is cut into pictures, held back until media flows, and paced.
func TestPumpVideoPacesAndCounts(t *testing.T) {
	stream, err := os.ReadFile("../../rtp/testdata/testsrc2_160x120_24f_3slices.h264")
	if err != nil {
		t.Fatal(err)
	}
	var evBuf lockedBuffer
	ready := make(chan struct{})
	var mu sync.Mutex
	var pictures [][]byte
	send := func(au []byte, n int) error {
		mu.Lock()
		defer mu.Unlock()
		if n != len(pictures) {
			t.Errorf("picture %d sent as number %d", len(pictures), n)
		}
		pictures = append(pictures, au)
		return nil
	}
	done := make(chan struct{})
	begin := time.Now()
	go func() {
		pumpVideo(context.Background(), send, bytes.NewReader(stream), 240, ready, &eventLog{w: &evBuf, start: begin})
		close(done)
	}()
	time.Sleep(30 * time.Millisecond)
	mu.Lock()
	early := len(pictures)
	mu.Unlock()
	if early != 0 {
		t.Fatalf("%d pictures sent before the call was ready", early)
	}
	close(ready)
	began := time.Now()
	<-done
	if took := time.Since(began); took < 90*time.Millisecond {
		t.Fatalf("24 pictures at 240 fps went out in %v, want about 96 ms", took)
	}
	if len(pictures) != 24 {
		t.Fatalf("%d pictures, want 24", len(pictures))
	}
	var rebuilt []byte
	for _, au := range pictures {
		rebuilt = append(rebuilt, au...)
	}
	if got, want := len(rtp.SplitAnnexB(rebuilt)), len(rtp.SplitAnnexB(stream)); got != want {
		t.Fatalf("%d NAL units sent, the file holds %d", got, want)
	}
	events, _ := parseEvents(t, evBuf.String())
	if e := find(events, "video_input_eof"); e == nil || e["pictures_sent"] != 24.0 || e["pictures_refused"] != 0.0 {
		t.Fatalf("video_input_eof: %v", e)
	}
}
